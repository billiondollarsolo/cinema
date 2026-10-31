package task

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CPython from python-build-standalone (astral-sh; uv installs these too):
// relocatable, so they run from wherever the install is mounted, read-only.
// setup-python's own builds (actions/python-versions) are compiled for the
// toolcache path, unpacked by a setup script, and publish no checksums. Both
// are CPython at the same version; the probe fingerprints it.
var (
	// Astral's release index (astral-sh/versions, the one setup-uv reads), one
	// release per line: maps an exact version to the build tags that ship it
	// without GitHub's API, whose unauthenticated limit is 60 calls an hour.
	pythonIndex = "https://releases.astral.sh/github/versions/main/v1/python-build-standalone.ndjson"
	pythonDist  = "https://github.com/astral-sh/python-build-standalone/releases/download"
)

var (
	pythonVersionRE = regexp.MustCompile(`^3\.[0-9]+\.[0-9]+$`)
	buildTagRE      = regexp.MustCompile(`^[0-9]{8}$`) // python-build-standalone tags are dates
)

func init() {
	RegisterRuntime(&RuntimeKind{Name: "python", Example: "3.12.7", Version: pythonVersionRE,
		Provision: provisionPython, Bins: []string{"bin"},
		// What setup-python exports (pythonLocation is what scripts and the
		// writable copy in analyze's setup-python translation read).
		Vars: func(root string) []string {
			return []string{"pythonLocation=" + root, "Python_ROOT_DIR=" + root, "Python2_ROOT_DIR=" + root, "Python3_ROOT_DIR=" + root,
				"PKG_CONFIG_PATH=" + filepath.Join(root, "lib", "pkgconfig"), "LD_LIBRARY_PATH=" + filepath.Join(root, "lib")}
		},
		// pip's cache is ~/.cache/pip: already on the sandbox's cache volume.
		Probe: []string{"python3", "--version"}})
}

// triple is the target name Astral's builds use for osName/arch.
func triple(osName, arch string) (string, error) {
	a := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
	o := map[string]string{"linux": "unknown-linux-gnu", "darwin": "apple-darwin"}[osName]
	if a == "" || o == "" {
		return "", fmt.Errorf("no build for %s/%s", osName, arch)
	}
	return a + "-" + o, nil
}

// provisionPython installs CPython ver under dataDir/runtimes/python. Several
// builds (release tags) ship the same version; the oldest is taken, so a
// pin means the same files on every machine and every day (the newest moves
// until the next patch release replaces it).
func provisionPython(dataDir, ver, osName, arch string) (string, error) {
	tr, err := triple(osName, arch)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dataDir, "runtimes", "python")
	if m, _ := filepath.Glob(filepath.Join(root, "cpython-"+ver+"+*-"+tr)); len(m) > 0 { // sorted: oldest tag first
		if fi, err := os.Stat(filepath.Join(m[0], "bin", "python3")); err == nil && fi.Mode().IsRegular() {
			now := time.Now()
			_ = os.Chtimes(m[0], now, now) // last use, for the cleanup sweep
			return m[0], nil
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	tag, file, sum, err := pythonBuild(client, ver, tr)
	if err != nil {
		return "", err
	}
	sums, err := httpGet(client, pythonDist+"/"+tag+"/SHA256SUMS", 4<<20)
	if err != nil {
		return "", err
	}
	if want := sumFor(sums, file); want == "" {
		return "", fmt.Errorf("%s not listed in the release's SHA256SUMS", file)
	} else if want != sum {
		return "", fmt.Errorf("%s: the release index and SHA256SUMS disagree", file)
	}
	tarball, err := httpGet(client, pythonDist+"/"+tag+"/"+strings.ReplaceAll(file, "+", "%2B"), 300<<20)
	if err != nil {
		return "", err
	}
	if h := sha256.Sum256(tarball); hex.EncodeToString(h[:]) != sum {
		return "", errors.New("checksum mismatch")
	}
	if tarball, err = dropCaseTwins(tarball); err != nil {
		return "", err
	}
	return installTarGz(root, "cpython-"+ver+"+"+tag+"-"+tr, tarball, "python/", "", "bin/python3", func(dir string) error {
		// setup-python's installs have python and pip too; these have python3, pip3.
		for link, to := range map[string]string{"python": "python3", "pip": "pip3"} {
			p := filepath.Join(dir, "bin", link)
			if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
				if _, err := os.Stat(filepath.Join(dir, "bin", to)); err == nil {
					if err := os.Symlink(to, p); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// pythonBuild finds the oldest build of ver for triple in the release index:
// its tag, tarball name (install_only_stripped, else install_only) and SHA-256.
func pythonBuild(c *http.Client, ver, triple string) (tag, file, sum string, err error) {
	resp, err := c.Get(pythonIndex)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("GET %s: %s", pythonIndex, resp.Status)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 256<<20))
	for {
		var r struct {
			Version   string `json:"version"` // 3.12.7+20241016
			Artifacts []struct {
				Platform, Variant, URL, SHA256 string
				Format                         string `json:"archive_format"`
			} `json:"artifacts"`
		}
		if err := dec.Decode(&r); err == io.EOF {
			break
		} else if err != nil {
			return "", "", "", fmt.Errorf("python release index: %w", err)
		}
		t, ok := strings.CutPrefix(r.Version, ver+"+")
		if !ok || !buildTagRE.MatchString(t) || (tag != "" && t >= tag) {
			continue
		}
	variants:
		for _, variant := range []string{"install_only_stripped", "install_only"} {
			for _, a := range r.Artifacts {
				name := ""
				if u, err := url.Parse(a.URL); err == nil {
					name = path.Base(u.Path)
				}
				if a.Platform == triple && a.Variant == variant && a.Format == "tar.gz" && len(a.SHA256) == 64 &&
					name == "cpython-"+ver+"+"+t+"-"+triple+"-"+variant+".tar.gz" {
					tag, file, sum = t, name, strings.ToLower(a.SHA256)
					break variants
				}
			}
		}
	}
	if tag == "" {
		return "", "", "", fmt.Errorf("no python-build-standalone build of CPython %s for %s", ver, triple)
	}
	return tag, file, sum, nil
}

// dropCaseTwins re-packs a tarball without entries whose path differs from an
// earlier one's only in case. The Linux builds' terminfo has both 2621A and
// 2621a, and a Mac's file system is case-insensitive: one install everywhere.
func dropCaseTwins(tgz []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	tw := tar.NewWriter(zw)
	seen := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		k := strings.ToLower(strings.TrimSuffix(h.Name, "/"))
		if seen[k] {
			continue
		}
		seen[k] = true
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// sumFor is file's SHA-256 in a sha256sum-style list ("" if absent).
func sumFor(list []byte, file string) string {
	sc := bufio.NewScanner(strings.NewReader(string(list)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == file && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// installTarGz unpacks a verified tarball's entries under prefix into
// root/name/sub, atomically (a temp dir, then rename), runs fix on the
// unpacked tree first, and returns root/name. check is a file the install
// must have, also how a concurrent install that won the rename is recognized.
func installTarGz(root, name string, tarball []byte, prefix, sub, check string, fix func(dir string) error) (string, error) {
	dest := filepath.Join(root, name)
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractTarGz(tarball, filepath.Join(tmp, sub), prefix); err != nil {
		return "", err
	}
	if fi, err := os.Stat(filepath.Join(tmp, filepath.FromSlash(check))); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: no %s in the archive", name, check)
	}
	if fix != nil {
		if err := fix(tmp); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(dest, filepath.FromSlash(check))); serr == nil {
			return dest, nil // another process installed it first
		}
		return "", err
	}
	return dest, nil
}
