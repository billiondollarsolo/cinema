package task

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/billiondollarsolo/cinema/internal/record"
)

// Runtime pins a language runtime that CInema provisions for the task and its
// fingerprint probes, identically on the evidence machine and in the gate, so
// environments match by construction instead of by hoping two machines have
// the same patch release installed (lab finding: the main cause of misses).
type Runtime struct {
	Go   string `yaml:"go"`   // exact, e.g. 1.26.10: GOTOOLCHAIN=go1.26.10 (Go downloads it, sumdb-verified)
	Node string `yaml:"node"` // exact, e.g. 22.23.3: official tarball, SHA-256 checked against nodejs.org's SHASUMS256.txt
	// Other: the registered runtimes (python, java, …: runtimes.go), by key.
	Other map[string]string `yaml:",inline"`
}

var (
	goVersionRE   = regexp.MustCompile(`^1\.[0-9]+\.[0-9]+$`)
	nodeVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

func (r Runtime) validate() error {
	if r.Go != "" && !goVersionRE.MatchString(r.Go) {
		return fmt.Errorf("runtime.go must be an exact version like 1.26.10, got %q", r.Go)
	}
	if r.Node != "" && !nodeVersionRE.MatchString(r.Node) {
		return fmt.Errorf("runtime.node must be an exact version like 22.23.3, got %q", r.Node)
	}
	return r.validateOther()
}

// RuntimeEnv returns the environment the task (and its probes) must run with:
// the current environment plus the pinned runtimes, provisioning Node into
// dataDir if needed. nil means "inherit unchanged" (no runtime pinned).
func (t *Task) RuntimeEnv(dataDir string) ([]string, error) {
	if !t.Runtime.Set() {
		return nil, nil
	}
	env := os.Environ()
	if t.Runtime.Go != "" {
		env = setEnv(env, "GOTOOLCHAIN", "go"+t.Runtime.Go)
		provisionGo(env)
	}
	if t.Runtime.Node != "" {
		bin, err := provisionNode(dataDir, t.Runtime.Node)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", t.Runtime.Node, err)
		}
		path := bin
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok && v != "" {
				path = bin + string(os.PathListSeparator) + v
			}
		}
		env = setEnv(env, "PATH", path)
	}
	if len(t.Runtime.Other) > 0 {
		osName, arch := runtimeGOOS, runtimeGOARCH
		bins, vars, _, err := t.otherInstalls(dataDir, osName, arch)
		if err != nil {
			return nil, err
		}
		path := strings.Join(bins, string(os.PathListSeparator))
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok && v != "" {
				path += string(os.PathListSeparator) + v
			}
		}
		env = setEnv(env, "PATH", path)
		for _, kv := range vars {
			k, v, _ := strings.Cut(kv, "=")
			env = setEnv(env, k, v)
		}
	}
	return env, nil
}

// The host's OS and arch, for runtimes provisioned for host runs (vars for tests).
var runtimeGOOS, runtimeGOARCH = runtime.GOOS, runtime.GOARCH

// PrivateGlobals points global package installs (`npm i -g pnpm`, `pnpm add -g`,
// common in CI scripts) at dir, a per-run scratch directory, and puts their bin
// dirs first on PATH. Otherwise they land in the shared provisioned runtime,
// where concurrent tasks race on them and later runs inherit them (lab: eslint).
// Evidence runs and gate runs both use it, so they behave alike.
//
// HOME is private too, as on a fresh runner: scripts that write dotfiles (lab:
// traefik's safe-chain → ~/.safe-chain) leave nothing behind, and no run sees
// what an earlier one left. Download and build caches stay in the real home, so
// they stay warm; they're keyed by content, not results.
func PrivateGlobals(env []string, dir string) []string {
	npm, pnpm := filepath.Join(dir, "npm-global"), filepath.Join(dir, "pnpm-home")
	path, home := "", ""
	set := map[string]bool{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		set[k] = true
		switch k {
		case "PATH":
			path = v
		case "HOME":
			home = v
		}
	}
	if home != "" {
		for _, c := range [][2]string{
			{"XDG_CACHE_HOME", ".cache"}, // go-build, yarn 1, pip, Cypress, puppeteer, playwright
			{"npm_config_cache", ".npm"},
			{"YARN_GLOBAL_FOLDER", ".yarn/berry"},
			{"DOCKER_CONFIG", ".docker"}, // logins and buildx builders
			{"RUSTUP_HOME", ".rustup"},   // installed toolchains
			{"CARGO_HOME", ".cargo"},
		} {
			if !set[c[0]] {
				env = setEnv(env, c[0], filepath.Join(home, c[1]))
			}
		}
		if !set["GOPATH"] && !set["GOMODCACHE"] {
			env = setEnv(env, "GOMODCACHE", filepath.Join(home, "go", "pkg", "mod"))
		}
		h := filepath.Join(dir, "home")
		if os.MkdirAll(h, 0o700) == nil {
			env = setEnv(env, "HOME", h)
		}
	}
	env = setEnv(env, "NPM_CONFIG_PREFIX", npm)
	env = setEnv(env, "PNPM_HOME", pnpm)
	return setEnv(env, "PATH", strings.Join([]string{filepath.Join(npm, "bin"), pnpm, filepath.Join(pnpm, "bin"), path}, string(os.PathListSeparator)))
}

func setEnv(env []string, k, v string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	return append(out, k+"="+v)
}

// nodeDist is nodejs.org's release host (a variable so tests can point it at a fake).
var nodeDist = "https://nodejs.org/dist"

// provisionNode returns the bin dir of Node <ver> under dataDir/runtimes,
// downloading and verifying it on first use. Install is atomic (extract to a
// temp dir, then rename), so a crash never leaves a half-installed runtime.
func provisionNode(dataDir, ver string) (string, error) {
	return provisionNodeFor(dataDir, ver, runtime.GOOS, runtime.GOARCH)
}

// provisionNodeFor is provisionNode for another OS/arch: a Linux build for a
// container on a Mac, say (it's mounted in, never run on the host).
func provisionNodeFor(dataDir, ver, osName, arch string) (string, error) {
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
	default:
		return "", fmt.Errorf("no Node build for %s/%s", osName, arch)
	}
	if osName != "linux" && osName != "darwin" {
		return "", fmt.Errorf("no Node build for %s", osName)
	}
	name := fmt.Sprintf("node-v%s-%s-%s", ver, osName, arch)
	root := filepath.Join(dataDir, "runtimes", "node")
	dest := filepath.Join(root, name)
	if fi, err := os.Stat(filepath.Join(dest, "bin", "node")); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now) // last use, for the cleanup sweep
		return filepath.Join(dest, "bin"), nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	sums, err := httpGet(client, fmt.Sprintf("%s/v%s/SHASUMS256.txt", nodeDist, ver), 1<<20)
	if err != nil {
		return "", err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name+".tar.gz" {
			want = f[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("%s.tar.gz not listed in SHASUMS256.txt", name)
	}
	tarball, err := httpGet(client, fmt.Sprintf("%s/v%s/%s.tar.gz", nodeDist, ver, name), 200<<20)
	if err != nil {
		return "", err
	}
	if h := sha256.Sum256(tarball); hex.EncodeToString(h[:]) != want {
		return "", errors.New("checksum mismatch")
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractTarGz(tarball, tmp, name+"/"); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(dest, "bin", "node")); serr == nil {
			return filepath.Join(dest, "bin"), nil // another process installed it first
		}
		return "", err
	}
	return filepath.Join(dest, "bin"), nil
}

func httpGet(c *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return b, err
}

// extractTarGz unpacks entries under prefix into dir. Paths that escape dir,
// and links pointing outside it, are refused.
func extractTarGz(data []byte, dir, prefix string) error {
	zr, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, ok := strings.CutPrefix(h.Name, prefix)
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes the install dir", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0o755)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			resolved := filepath.Join(filepath.Dir(target), h.Linkname)
			if filepath.IsAbs(h.Linkname) || !strings.HasPrefix(resolved, filepath.Clean(dir)+string(os.PathSeparator)) {
				return fmt.Errorf("tar link %q points outside the install dir", h.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// goDist is go.dev's download host (a variable so tests can point it at a fake).
var goDist = "https://go.dev/dl"

// provisionGoFor returns the bin dir of the official Go <ver> release for
// osName/arch under dataDir/runtimes/go, downloading it on first use and
// checking it against the SHA-256 go.dev publishes. Unlike GOTOOLCHAIN it needs
// no go already installed (P4-1: a clean container has none).
func provisionGoFor(dataDir, ver, osName, arch string) (string, error) {
	name := fmt.Sprintf("go%s.%s-%s", ver, osName, arch)
	root := filepath.Join(dataDir, "runtimes", "go")
	dest := filepath.Join(root, name)
	if fi, err := os.Stat(filepath.Join(dest, "bin", "go")); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now)
		return filepath.Join(dest, "bin"), nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	idx, err := httpGet(client, goDist+"/?mode=json&include=all", 32<<20)
	if err != nil {
		return "", err
	}
	var rels []struct {
		Version string `json:"version"`
		Files   []struct {
			Filename string `json:"filename"`
			SHA256   string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(idx, &rels); err != nil {
		return "", fmt.Errorf("go.dev release index: %w", err)
	}
	want := ""
	for _, r := range rels {
		for _, f := range r.Files {
			if r.Version == "go"+ver && f.Filename == name+".tar.gz" {
				want = f.SHA256
			}
		}
	}
	if len(want) != 64 {
		return "", fmt.Errorf("%s.tar.gz not in go.dev's release index", name)
	}
	tarball, err := httpGet(client, goDist+"/"+name+".tar.gz", 300<<20)
	if err != nil {
		return "", err
	}
	if h := sha256.Sum256(tarball); hex.EncodeToString(h[:]) != want {
		return "", errors.New("checksum mismatch")
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractTarGz(tarball, tmp, "go/"); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(dest, "bin", "go")); serr == nil {
			return filepath.Join(dest, "bin"), nil
		}
		return "", err
	}
	return filepath.Join(dest, "bin"), nil
}

// RuntimeBins provisions t's pinned runtimes for osName/arch and returns their
// bin dirs, in PATH order. For a container: they're mounted in read-only.
func (t *Task) RuntimeBins(dataDir, osName, arch string) ([]string, error) {
	var bins []string
	if v := t.Runtime.Node; v != "" {
		b, err := provisionNodeFor(dataDir, v, osName, arch)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", v, err)
		}
		bins = append(bins, b)
	}
	if v := t.Runtime.Go; v != "" {
		b, err := provisionGoFor(dataDir, v, osName, arch)
		if err != nil {
			return nil, fmt.Errorf("go %s: %w", v, err)
		}
		bins = append(bins, b)
	}
	return bins, nil
}

// provisionGo makes the go command fetch the pinned toolchain now. On a fresh
// machine its first call prints "go: downloading goX" to stderr; inside a probe
// that line would change the environment digest (lab: prometheus's gate).
// Errors are left to the probes and the task, which report them.
func provisionGo(env []string) {
	cmd := exec.Command("go", "version")
	cmd.Env = env
	if p, err := record.LookPath("go", env); err == nil {
		cmd.Path, cmd.Err = p, nil
	}
	_ = cmd.Run()
}
