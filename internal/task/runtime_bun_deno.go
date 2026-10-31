package task

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Release hosts (variables so tests can point them at a fake). Deno: GitHub,
// as setup-deno downloads stable releases from there; dl.deno.land serves a
// differently built zip of the same version (2.9.8: other bytes, other size).
var (
	bunDist  = "https://github.com/oven-sh/bun/releases/download"
	denoDist = "https://github.com/denoland/deno/releases/download"
)

var bunDenoVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func init() {
	RegisterRuntime(&RuntimeKind{Name: "bun", Example: "1.2.23", Version: bunDenoVersion,
		Provision: provisionBun, Bins: []string{"bin"},
		// Where `bun install` keeps packages on a runner (no XDG_CACHE_HOME there).
		Caches: map[string]string{"bun": ".bun/install/cache"},
		Probe:  []string{"bun", "--version"}})
	// DENO_DIR defaults to ~/.cache/deno on Linux: already the sandbox's cache volume.
	RegisterRuntime(&RuntimeKind{Name: "deno", Example: "2.5.6", Version: bunDenoVersion,
		Provision: provisionDeno, Bins: []string{"bin"},
		Probe: []string{"deno", "--version"}})
}

// provisionBun installs the build setup-bun picks on a GitHub runner: glibc
// (not musl), and for x64 the AVX2 build (not -baseline: setup-bun reads
// /proc/cpuinfo, and hosted runners have AVX2). bunx is a link to bun, as
// setup-bun makes it (bun acts on the name it's run as).
func provisionBun(dataDir, ver, osName, arch string) (string, error) {
	a := map[string]string{"amd64": "x64", "arm64": "aarch64"}[arch]
	if a == "" || (osName != "linux" && osName != "darwin") {
		return "", fmt.Errorf("no Bun build for %s/%s", osName, arch)
	}
	name := fmt.Sprintf("bun-%s-%s", osName, a)
	base := fmt.Sprintf("%s/bun-v%s/", bunDist, ver)
	return installBinaryZip(dataDir, "bun", "bun-v"+ver+"-"+osName+"-"+arch, base+name+".zip", name+"/", "bun", func(c *http.Client) (string, error) {
		sums, err := httpGet(c, base+"SHASUMS256.txt", 1<<20)
		if err != nil {
			return "", err
		}
		sc := bufio.NewScanner(bytes.NewReader(sums))
		for sc.Scan() {
			if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name+".zip" {
				return f[0], nil
			}
		}
		return "", fmt.Errorf("%s.zip not listed in SHASUMS256.txt", name)
	}, "bunx")
}

// provisionDeno installs the release zip setup-deno downloads. Deno publishes
// a <zip>.sha256sum from 2.0.1 on; 1.x and 2.0.0 have none, so they're refused.
func provisionDeno(dataDir, ver, osName, arch string) (string, error) {
	a := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
	target := map[string]string{"linux": "unknown-linux-gnu", "darwin": "apple-darwin"}[osName]
	if a == "" || target == "" {
		return "", fmt.Errorf("no Deno build for %s/%s", osName, arch)
	}
	zipName := fmt.Sprintf("deno-%s-%s.zip", a, target)
	url := fmt.Sprintf("%s/v%s/%s", denoDist, ver, zipName)
	return installBinaryZip(dataDir, "deno", "deno-v"+ver+"-"+osName+"-"+arch, url, "", "deno", func(c *http.Client) (string, error) {
		b, err := httpGet(c, url+".sha256sum", 4<<10)
		if err != nil {
			return "", fmt.Errorf("deno %s publishes no checksum for %s (from 2.0.1 on it does): %w", ver, zipName, err)
		}
		if f := strings.Fields(string(b)); len(f) == 2 && strings.TrimPrefix(f[1], "*") == zipName {
			return f[0], nil
		}
		return "", fmt.Errorf("%s.sha256sum: unexpected content", zipName)
	})
}

// installBinaryZip installs a runtime that ships as one executable in a zip, as
// provisionNodeFor does a tarball: <dataDir>/runtimes/<lang>/<install>, with
// exe (from the zip's entries under prefix) in bin/ and links to it beside it.
// The zip must match the publisher's SHA-256 (sum); the install is atomic.
func installBinaryZip(dataDir, lang, install, url, prefix, exe string, sum func(*http.Client) (string, error), links ...string) (string, error) {
	root := filepath.Join(dataDir, "runtimes", lang)
	dest := filepath.Join(root, install)
	if fi, err := os.Stat(filepath.Join(dest, "bin", exe)); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now) // last use, for the cleanup sweep
		return dest, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	want, err := sum(client)
	if err != nil {
		return "", err
	}
	data, err := httpGet(client, url, 300<<20)
	if err != nil {
		return "", err
	}
	if h := sha256.Sum256(data); hex.EncodeToString(h[:]) != strings.ToLower(want) {
		return "", errors.New("checksum mismatch")
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "bin")
	if err := extractBinaryZip(data, bin, prefix); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(filepath.Join(bin, exe)); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: no %s%s in the zip", url, prefix, exe)
	}
	if err := os.Chmod(filepath.Join(bin, exe), 0o755); err != nil {
		return "", err
	}
	for _, l := range links {
		if err := os.Symlink(exe, filepath.Join(bin, l)); err != nil { // relative: works where it's mounted
			return "", err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(dest, "bin", exe)); serr == nil {
			return dest, nil // another process installed it first
		}
		return "", err
	}
	return dest, nil
}

// extractBinaryZip unpacks the regular files and dirs under prefix into dir. Paths
// that escape dir are refused; links are skipped (these zips have none).
func extractBinaryZip(data []byte, dir, prefix string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if !filepath.IsLocal(filepath.FromSlash(rel)) || !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry %q escapes the install dir", f.Name)
		}
		switch m := f.Mode(); {
		case m.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case m.IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeZipEntry(f, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.Mode().Perm()&0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
