package task

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Bun and Deno: the build setup-bun/setup-deno install on a runner, checked
// against the publisher's SHA-256, installed once (bunx beside bun), reused,
// and refused when the checksum doesn't match.
func TestProvisionBunDeno(t *testing.T) {
	bunZip := zipOf(t, map[string]string{"bun-linux-x64/bun": "#!/bin/sh\necho 1.2.23\n"})
	denoZip := zipOf(t, map[string]string{"deno": "#!/bin/sh\necho deno 2.5.6\n"})
	sums := map[string]string{
		"/bun/bun-v1.2.23/SHASUMS256.txt":                          hexSum([]byte("other")) + "  bun-linux-x64-baseline.zip\n" + hexSum(bunZip) + "  bun-linux-x64.zip\n",
		"/bun/bun-v1.2.24/SHASUMS256.txt":                          hexSum([]byte("tampered")) + "  bun-linux-x64.zip\n",
		"/deno/v2.5.6/deno-x86_64-unknown-linux-gnu.zip.sha256sum": hexSum(denoZip) + "  deno-x86_64-unknown-linux-gnu.zip\n",
		"/deno/v2.5.7/deno-x86_64-unknown-linux-gnu.zip.sha256sum": hexSum([]byte("tampered")) + "  deno-x86_64-unknown-linux-gnu.zip\n",
		"/bun/bun-v1.2.23/bun-linux-x64.zip":                       string(bunZip),
		"/bun/bun-v1.2.24/bun-linux-x64.zip":                       string(bunZip),
		"/deno/v2.5.6/deno-x86_64-unknown-linux-gnu.zip":           string(denoZip),
		"/deno/v2.5.7/deno-x86_64-unknown-linux-gnu.zip":           string(denoZip),
		"/deno/v1.46.3/deno-x86_64-unknown-linux-gnu.zip":          string(denoZip), // 1.x: no .sha256sum
	}
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		b, ok := sums[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, b)
	}))
	defer srv.Close()
	oldB, oldD := bunDist, denoDist
	bunDist, denoDist = srv.URL+"/bun", srv.URL+"/deno"
	defer func() { bunDist, denoDist = oldB, oldD }()

	data := t.TempDir()
	root, err := provisionBun(data, "1.2.23", "linux", "amd64")
	if err != nil || root != filepath.Join(data, "runtimes", "bun", "bun-v1.2.23-linux-amd64") {
		t.Fatalf("bun: %v %s", err, root)
	}
	if l, err := os.Readlink(filepath.Join(root, "bin", "bunx")); err != nil || l != "bun" {
		t.Fatalf("bunx: %q %v", l, err)
	}
	if fi, err := os.Stat(filepath.Join(root, "bin", "bun")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("bun not executable: %v", err)
	}
	n := len(got)
	if _, err := provisionBun(data, "1.2.23", "linux", "amd64"); err != nil || len(got) != n {
		t.Fatalf("second provision should reuse the install: %v %v", err, got[n:])
	}
	if _, err := provisionBun(data, "1.2.24", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("bun checksum mismatch: %v", err)
	}
	root, err = provisionDeno(data, "2.5.6", "linux", "amd64")
	if err != nil || root != filepath.Join(data, "runtimes", "deno", "deno-v2.5.6-linux-amd64") {
		t.Fatalf("deno: %v %s", err, root)
	}
	if b, err := os.ReadFile(filepath.Join(root, "bin", "deno")); err != nil || !strings.Contains(string(b), "deno 2.5.6") {
		t.Fatalf("deno binary: %v", err)
	}
	if _, err := provisionDeno(data, "2.5.7", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("deno checksum mismatch: %v", err)
	}
	if _, err := provisionDeno(data, "1.46.3", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "publishes no checksum") {
		t.Fatalf("deno without a checksum: %v", err)
	}
	if _, err := provisionBun(data, "1.2.23", "windows", "amd64"); err == nil {
		t.Fatal("bun for windows: want an error")
	}
	// Nothing half-installed is left behind: only finished installs.
	for _, lang := range []string{"bun", "deno"} {
		ents, _ := os.ReadDir(filepath.Join(data, "runtimes", lang))
		if len(ents) != 1 {
			t.Fatalf("%s: %v", lang, ents)
		}
	}
	// The registered kinds use these, with the bun download cache for sandboxes.
	k := LookupRuntime("bun")
	if k == nil || k.Caches["bun"] != ".bun/install/cache" || LookupRuntime("deno") == nil || !k.Version.MatchString("1.2.23") || k.Version.MatchString("1.2") {
		t.Fatalf("registry: %+v", k)
	}
}

// A zip entry that climbs out of the install dir is refused.
func TestBinaryZipTraversal(t *testing.T) {
	for _, name := range []string{"../evil", "bun-linux-x64/../../evil", "/abs/evil"} {
		dir := filepath.Join(t.TempDir(), "bin")
		if err := extractBinaryZip(zipOf(t, map[string]string{name: "x"}), dir, ""); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil")); err == nil {
			t.Errorf("%s: written outside the dir", name)
		}
	}
	dir := t.TempDir()
	if err := extractBinaryZip(zipOf(t, map[string]string{"bun-linux-x64/bun": "x", "README": "y"}), dir, "bun-linux-x64/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err == nil {
		t.Fatal("entries outside the prefix must be skipped")
	}
}
