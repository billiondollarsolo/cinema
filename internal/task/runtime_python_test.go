package task

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

// tarOf builds a tar.gz in order; a body starting with "->" is a symlink.
func tarOf(entries ...string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i := 0; i < len(entries); i += 2 {
		name, body := entries[i], entries[i+1]
		if to, ok := strings.CutPrefix(body, "->"); ok {
			tw.WriteHeader(&tar.Header{Name: name, Linkname: to, Typeflag: tar.TypeSymlink})
			continue
		}
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// fakePython serves a release index listing ver in two builds (the older
// with only install_only, the newer stripped too) at sum ("": the tarball's),
// SHA256SUMS (sums overrides it) and the tarball.
func fakePython(t *testing.T, ver string, tarball []byte, sum, sums string) {
	if sum == "" {
		sum = sha(tarball)
	}
	tr := "x86_64-unknown-linux-gnu"
	file := func(tag, variant string) string {
		return "cpython-" + ver + "+" + tag + "-" + tr + "-" + variant + ".tar.gz"
	}
	art := func(tag, variant string) string {
		return fmt.Sprintf(`{"platform":%q,"variant":%q,"url":"https://github.com/astral-sh/python-build-standalone/releases/download/%s/%s","archive_format":"tar.gz","sha256":%q}`,
			tr, variant, tag, strings.ReplaceAll(file(tag, variant), "+", "%2B"), sum)
	}
	index := `{"version":"3.99.0+20250101","artifacts":[]}` + "\n" +
		`{"version":"` + ver + `+20241008","artifacts":[` + art("20241008", "install_only") + "," + art("20241008", "install_only_stripped") + `]}` + "\n" +
		`{"version":"` + ver + `+20241002","artifacts":[` + art("20241002", "debug-full") + "," + art("20241002", "install_only") + `]}` + "\n"
	if sums == "" {
		sums = sum + "  " + file("20241002", "install_only") + "\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/index.ndjson":
			fmt.Fprint(w, index)
		case "/20241002/SHA256SUMS":
			fmt.Fprint(w, sums)
		case "/20241002/" + strings.ReplaceAll(file("20241002", "install_only"), "+", "%2B"):
			w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldIndex, oldDist := pythonIndex, pythonDist
	pythonIndex, pythonDist = srv.URL+"/index.ndjson", srv.URL
	t.Cleanup(func() { pythonIndex, pythonDist = oldIndex, oldDist })
}

func TestPythonRuntimeProvisioned(t *testing.T) {
	tb := tarOf("python/bin/python3.12", "#!/bin/sh\necho Python 3.12.7\n", "python/bin/python3", "->python3.12", "python/bin/pip3", "#!/bin/sh\n",
		"python/share/terminfo/2/2621A", "a", "python/share/terminfo/2/2621a", "b")
	fakePython(t, "3.12.7", tb, "", "")
	k := LookupRuntime("python")
	data := t.TempDir()
	root, err := k.Provision(data, "3.12.7", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	// The oldest build, at <data>/runtimes/python/<install>.
	if want := filepath.Join(data, "runtimes", "python", "cpython-3.12.7+20241002-x86_64-unknown-linux-gnu"); root != want {
		t.Fatalf("installed at %s, want %s", root, want)
	}
	for _, link := range []string{"python", "pip"} { // as setup-python's installs have
		if _, err := os.Stat(filepath.Join(root, "bin", link)); err != nil {
			t.Errorf("no bin/%s: %v", link, err)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "share", "terminfo", "2")); len(ents) != 1 {
		t.Errorf("case twins: %v (one install on case-insensitive and case-sensitive disks)", ents)
	}
	if vars := strings.Join(k.Vars(root), " "); !strings.Contains(vars, "pythonLocation="+root) || !strings.Contains(vars, "PKG_CONFIG_PATH="+filepath.Join(root, "lib", "pkgconfig")) {
		t.Errorf("vars: %s", vars)
	}
	pythonIndex, pythonDist = "http://127.0.0.1:1", "http://127.0.0.1:1" // offline: found, not fetched
	if again, err := k.Provision(data, "3.12.7", "linux", "amd64"); err != nil || again != root {
		t.Fatalf("reuse: %v %s", err, again)
	}
	if _, err := k.Provision(data, "3.12.7", "windows", "amd64"); err == nil {
		t.Fatal("windows build provisioned")
	}
}

func TestPythonRuntimeChecksums(t *testing.T) {
	tb := tarOf("python/bin/python3", "#!/bin/sh\n")
	file := "  cpython-3.12.8+20241002-x86_64-unknown-linux-gnu-install_only.tar.gz\n"
	for _, c := range []struct {
		name, sums, want string
		served           []byte
	}{
		{"SHA256SUMS disagrees", strings.Repeat("0", 64) + file, "disagree", tb},
		{"not in SHA256SUMS", strings.Repeat("0", 64) + "  other.tar.gz\n", "not listed", tb},
		{"tarball doesn't match", sha(tb) + file, "checksum mismatch", append(append([]byte{}, tb...), 0)},
	} {
		fakePython(t, "3.12.8", c.served, sha(tb), c.sums)
		data := t.TempDir()
		if _, err := LookupRuntime("python").Provision(data, "3.12.8", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if m, _ := filepath.Glob(filepath.Join(data, "runtimes", "python", "*")); len(m) > 0 {
			t.Errorf("%s: left %v", c.name, m)
		}
	}
	if _, err := LookupRuntime("python").Provision(t.TempDir(), "3.12.99", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "no python-build-standalone build") {
		t.Errorf("unknown version: %v", err)
	}
}

func fakeUv(t *testing.T, ver string, tarball []byte, sum string) {
	file := "uv-aarch64-apple-darwin.tar.gz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + ver + "/" + file + ".sha256":
			fmt.Fprintf(w, "%s  %s\n", sum, file)
		case "/" + ver + "/" + file:
			w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := uvDist
	uvDist = srv.URL
	t.Cleanup(func() { uvDist = old })
}

func TestUvRuntime(t *testing.T) {
	tb := tarOf("uv-aarch64-apple-darwin/uv", "#!/bin/sh\necho uv 0.9.5\n", "uv-aarch64-apple-darwin/uvx", "#!/bin/sh\n")
	fakeUv(t, "0.9.5", tb, sha(tb))
	data := t.TempDir()
	root, err := LookupRuntime("uv").Provision(data, "0.9.5", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.Join(data, "runtimes", "uv", "uv-0.9.5-aarch64-apple-darwin") {
		t.Fatalf("installed at %s", root)
	}
	for _, b := range []string{"uv", "uvx"} {
		if _, err := os.Stat(filepath.Join(root, "bin", b)); err != nil {
			t.Error(err)
		}
	}
	fakeUv(t, "0.9.6", tb, strings.Repeat("0", 64))
	data = t.TempDir()
	if _, err := LookupRuntime("uv").Provision(data, "0.9.6", "darwin", "arm64"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum mismatch accepted: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(data, "runtimes", "uv", "*")); len(m) > 0 {
		t.Fatalf("left behind: %v", m)
	}
}

// Pinned in tasks.yaml: validated, probed with python3 --version.
func TestPythonUvPins(t *testing.T) {
	tk := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [true], platform: any, runtime: {python: 3.12.7, uv: 0.9.5}}\n").Tasks["t"]
	if tk.Runtime.Get("python") != "3.12.7" || tk.Runtime.Get("uv") != "0.9.5" {
		t.Fatalf("%+v", tk.Runtime)
	}
	for _, bad := range []string{"python: '3.12'", "python: 3.12.x", "uv: latest"} {
		if _, err := Parse([]byte("schema_version: 1\ntasks:\n  t: {argv: [true], platform: any, runtime: {" + bad + "}}\n")); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if p := LookupRuntime("python").Probe; strings.Join(p, " ") != "python3 --version" {
		t.Errorf("probe %v", p)
	}
}
