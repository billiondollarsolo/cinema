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
	"runtime"
	"strings"
	"testing"
)

func nodeName(ver string) string {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	return fmt.Sprintf("node-v%s-%s-%s", ver, runtime.GOOS, arch)
}

func tarGz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// fakeDist serves SHASUMS256.txt and the tarball; sum overrides the listed checksum.
func fakeDist(t *testing.T, ver string, tarball []byte, sum string) {
	if sum == "" {
		h := sha256.Sum256(tarball)
		sum = hex.EncodeToString(h[:])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v" + ver + "/SHASUMS256.txt":
			fmt.Fprintf(w, "%s  %s.tar.gz\n", sum, nodeName(ver))
		case "/v" + ver + "/" + nodeName(ver) + ".tar.gz":
			w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := nodeDist
	nodeDist = srv.URL
	t.Cleanup(func() { nodeDist = old })
}

func TestNodeRuntimeProvisioned(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no node builds for this OS")
	}
	ver := "99.1.2"
	fakeDist(t, ver, tarGz(t, map[string]string{nodeName(ver) + "/bin/node": "#!/bin/sh\necho v" + ver + "\n"}), "")
	data := t.TempDir()
	tk := mustParse(t, "schema_version: 1\ntasks:\n  t:\n    argv: [node, --version]\n    platform: linux/amd64\n    runtime: {node: "+ver+"}\n    toolchain: [{cmd: [node, --version]}]\n").Tasks["t"]
	if !tk.RunsCommands() {
		t.Fatal("a pinned runtime must require trust (it downloads and runs a binary)")
	}
	env, err := tk.RuntimeEnv(data)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := tk.EnvFingerprint(t.TempDir(), env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fp.Inputs, " "), "node --version → v"+ver) {
		t.Fatalf("probe didn't use the provisioned node: %v", fp.Inputs)
	}
	// Second use: no download needed (server would 404 a different version anyway).
	if _, err := tk.RuntimeEnv(data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "runtimes", "node", nodeName(ver), "bin", "node")); err != nil {
		t.Fatal("not installed under the data dir")
	}
}

func TestNodeRuntimeChecksumAndTraversal(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip()
	}
	ver := "99.1.3"
	good := tarGz(t, map[string]string{nodeName(ver) + "/bin/node": "#!/bin/sh\n"})
	fakeDist(t, ver, good, strings.Repeat("0", 64))
	tk := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [node], platform: any, runtime: {node: "+ver+"}}\n").Tasks["t"]
	if _, err := tk.RuntimeEnv(t.TempDir()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("checksum mismatch accepted: %v", err)
	}
	ver = "99.1.4"
	evil := tarGz(t, map[string]string{nodeName(ver) + "/../../escaped": "x", nodeName(ver) + "/bin/node": "#!/bin/sh\n"})
	fakeDist(t, ver, evil, "")
	tk = mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [node], platform: any, runtime: {node: "+ver+"}}\n").Tasks["t"]
	data := t.TempDir()
	if _, err := tk.RuntimeEnv(data); err == nil {
		t.Fatal("tar entry escaping the install dir accepted")
	}
	if _, err := os.Stat(filepath.Join(data, "escaped")); err == nil {
		t.Fatal("file written outside the install dir")
	}
}

func TestGoRuntimeAndValidation(t *testing.T) {
	tk := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [go, version], platform: any, runtime: {go: 1.26.10}}\n").Tasks["t"]
	env, err := tk.RuntimeEnv(t.TempDir())
	if err != nil || !strings.Contains(strings.Join(env, "\n"), "GOTOOLCHAIN=go1.26.10") {
		t.Fatalf("go runtime env: %v", err)
	}
	for _, bad := range []string{"{go: 1.26}", "{go: latest}", "{node: 22}", "{node: lts}", "{node: '22.1.0; rm -rf /'}"} {
		if _, err := Parse([]byte("schema_version: 1\ntasks:\n  t: {argv: [x], platform: any, runtime: " + bad + "}\n")); err == nil {
			t.Errorf("runtime %s accepted", bad)
		}
	}
	a := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [x], platform: any, runtime: {node: 22.1.0}}\n").Tasks["t"]
	b := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [x], platform: any, runtime: {node: 24.1.0}}\n").Tasks["t"]
	da, _ := a.ContractDigest(t.TempDir())
	db, _ := b.ContractDigest(t.TempDir())
	if da == db {
		t.Fatal("runtime version must be part of the contract")
	}
}

func TestPrivateGlobals(t *testing.T) {
	env := PrivateGlobals([]string{"PATH=/usr/bin", "NPM_CONFIG_PREFIX=/shared"}, "/s")
	want := map[string]bool{"NPM_CONFIG_PREFIX=/s/npm-global": true, "PNPM_HOME=/s/pnpm-home": true, "PATH=/s/npm-global/bin:/s/pnpm-home:/s/pnpm-home/bin:/usr/bin": true}
	if len(env) != 3 {
		t.Fatalf("%q", env)
	}
	for _, kv := range env {
		if !want[kv] {
			t.Errorf("unexpected %q", kv)
		}
	}
}
