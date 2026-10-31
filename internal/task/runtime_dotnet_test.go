package task

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDotnet serves the 8.0 channel's releases.json, listing ver for
// linux-x64 with hash (the archive's own SHA-512 if ""), and the archive.
func fakeDotnet(t *testing.T, ver string, tarball []byte, hash string) *int {
	if hash == "" {
		h := sha512.Sum512(tarball)
		hash = hex.EncodeToString(h[:])
	}
	file := "dotnet-sdk-" + ver + "-linux-x64.tar.gz"
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		switch r.URL.Path {
		case "/release-metadata/8.0/releases.json":
			fmt.Fprintf(w, `{"releases":[{"sdk":{"version":"8.0.999","files":[]},"sdks":[{"version":"%[1]s","files":[
				{"name":"dotnet-sdk-linux-arm64.tar.gz","url":"https://builds.dotnet.microsoft.com/dotnet/Sdk/%[1]s/dotnet-sdk-%[1]s-linux-arm64.tar.gz","hash":"%[2]s"},
				{"name":"dotnet-sdk-linux-x64.tar.gz","url":"https://builds.dotnet.microsoft.com/dotnet/Sdk/%[1]s/%[3]s","hash":"%[4]s"}]}]}]}`,
				ver, strings.Repeat("0", 128), file, hash)
		case "/Sdk/" + ver + "/" + file:
			w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := dotnetDist
	dotnetDist = srv.URL
	t.Cleanup(func() { dotnetDist = old })
	return &gets
}

func TestDotnetProvisioned(t *testing.T) {
	ver := "8.0.404"
	gets := fakeDotnet(t, ver, tarGz(t, map[string]string{"./dotnet": "#!/bin/sh\n", "./sdk/" + ver + "/dotnet.dll": "x"}), "")
	data := t.TempDir()
	tk := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [dotnet, test], platform: linux/amd64, runtime: {dotnet: "+ver+"}}\n").Tasks["t"]
	bins, vars, caches, err := tk.RuntimeSandbox(data, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(data, "runtimes", "dotnet", "dotnet-sdk-"+ver+"-linux-amd64")
	if len(bins) != 1 || bins[0] != root || vars[0] != "DOTNET_ROOT="+root || caches["nuget"] != ".nuget/packages" {
		t.Fatalf("%v %v %v", bins, vars, caches)
	}
	if _, err := os.Stat(filepath.Join(root, "sdk", ver, "dotnet.dll")); err != nil {
		t.Fatal("not unpacked:", err)
	}
	n := *gets
	if _, _, _, err := tk.RuntimeSandbox(data, "linux", "amd64"); err != nil || *gets != n {
		t.Fatalf("reinstalled (%d → %d requests): %v", n, *gets, err)
	}
}

func TestDotnetRefused(t *testing.T) {
	ver := "8.0.405"
	good := tarGz(t, map[string]string{"./dotnet": "#!/bin/sh\n"})
	fakeDotnet(t, ver, good, strings.Repeat("ab", 64))
	data := t.TempDir()
	if _, err := provisionDotnet(data, ver, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("checksum mismatch accepted: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(data, "runtimes", "dotnet")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	fakeDotnet(t, ver, good, "")
	for _, c := range [][3]string{
		{"8.0.406", "linux", "amd64"}, // not in releases.json
		{ver, "windows", "amd64"},
		{ver, "linux", "386"},
		{"8.0.11", "linux", "amd64"},     // a runtime version, not an SDK's
		{"../../x", "linux", "amd64"},    // never a path
		{"8.0.404/..", "linux", "amd64"}, // nor this
	} {
		if _, err := provisionDotnet(data, c[0], c[1], c[2]); err == nil {
			t.Errorf("%v: provisioned", c)
		}
	}
	fakeDotnet(t, ver, tarGz(t, map[string]string{"./../escaped": "x", "./dotnet": "#!/bin/sh\n"}), "")
	if _, err := provisionDotnet(data, ver, "linux", "amd64"); err == nil {
		t.Fatal("tar entry escaping the install dir accepted")
	}
	if _, err := os.Stat(filepath.Join(data, "runtimes", "dotnet", "escaped")); err == nil {
		t.Fatal("file written outside the install dir")
	}
	fakeDotnet(t, ver, tarGz(t, map[string]string{"dotnet": "#!/bin/sh\n"}), "")
	if _, err := provisionDotnet(data, ver, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "no dotnet") {
		t.Fatalf("an archive without dotnet at its root: %v", err)
	}
}
