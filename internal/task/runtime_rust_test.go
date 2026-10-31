package task

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRustDist serves a 1.83.0 channel manifest for x86_64 Linux and its
// packages. edit may change the manifest before its .sha256 is computed;
// sum, if set, replaces the published manifest checksum.
func fakeRustDist(t *testing.T, edit func(string) string, sum string) *atomic.Int32 {
	const host = "x86_64-unknown-linux-gnu"
	pkg := func(name, comp string, files map[string]string) []byte {
		all := map[string]string{name + "/components": comp + "\n", name + "/install.sh": "#!/bin/sh\nexit 1\n",
			name + "/" + comp + "/manifest.in": "file:x\n"}
		for p, body := range files {
			all[name+"/"+comp+"/"+p] = body
		}
		return tarGz(t, all)
	}
	tarballs := map[string][]byte{
		"rustc-1.83.0-" + host:                   pkg("rustc-1.83.0-"+host, "rustc", map[string]string{"bin/rustc": "rustc", "lib/librustc_driver.so": "so"}),
		"cargo-1.83.0-" + host:                   pkg("cargo-1.83.0-"+host, "cargo", map[string]string{"bin/cargo": "cargo"}),
		"rust-std-1.83.0-" + host:                pkg("rust-std-1.83.0-"+host, "rust-std-"+host, map[string]string{"lib/rustlib/" + host + "/lib/libcore.rlib": "core"}),
		"rust-std-1.83.0-wasm32-unknown-unknown": pkg("rust-std-1.83.0-wasm32-unknown-unknown", "rust-std-wasm32-unknown-unknown", map[string]string{"lib/rustlib/wasm32-unknown-unknown/lib/libcore.rlib": "core"}),
		"clippy-1.83.0-" + host:                  pkg("clippy-1.83.0-"+host, "clippy-preview", map[string]string{"bin/cargo-clippy": "clippy", "bin/clippy-driver": "driver"}),
		"rust-src-1.83.0":                        pkg("rust-src-1.83.0", "rust-src", map[string]string{"lib/rustlib/src/rust/library/core/lib.rs": "src"}),
	}
	var m strings.Builder
	m.WriteString("manifest-version = \"2\"\ndate = \"2024-11-28\"\n\n")
	entry := func(name, target, file string) {
		fmt.Fprintf(&m, "[pkg.%s.target.%s]\navailable = true\nurl = \"https://static.rust-lang.org/dist/2024-11-28/%s.tar.gz\"\nhash = \"%s\"\nxz_url = \"x\"\nxz_hash = \"y\"\n\n",
			name, target, file, sha256hex(tarballs[file]))
	}
	entry("rustc", host, "rustc-1.83.0-"+host)
	entry("cargo", host, "cargo-1.83.0-"+host)
	entry("rust-std", host, "rust-std-1.83.0-"+host)
	entry("rust-std", "wasm32-unknown-unknown", "rust-std-1.83.0-wasm32-unknown-unknown")
	entry("clippy-preview", host, "clippy-1.83.0-"+host)
	entry("rust-src", `"*"`, "rust-src-1.83.0")
	m.WriteString("[pkg.rustfmt-preview.target." + host + "]\navailable = false\n\n[renames.clippy]\nto = \"clippy-preview\"\n")
	manifest := m.String()
	if edit != nil {
		manifest = edit(manifest)
	}
	if sum == "" {
		sum = sha256hex([]byte(manifest))
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch p := r.URL.Path; {
		case p == "/channel-rust-1.83.0.toml":
			fmt.Fprint(w, manifest)
		case p == "/channel-rust-1.83.0.toml.sha256":
			fmt.Fprintf(w, "%s  channel-rust-1.83.0.toml\n", sum)
		case strings.HasPrefix(p, "/2024-11-28/") && tarballs[strings.TrimSuffix(p[12:], ".tar.gz")] != nil:
			w.Write(tarballs[strings.TrimSuffix(p[12:], ".tar.gz")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := rustDist
	rustDist = srv.URL
	t.Cleanup(func() { rustDist = old })
	return &hits
}

// A toolchain is the minimal profile plus the pin's components and targets,
// merged into one prefix without install.sh, then reused.
func TestRustProvisioned(t *testing.T) {
	hits := fakeRustDist(t, nil, "")
	data := t.TempDir()
	root, err := provisionRust(data, "1.83.0+clippy+rust-src+wasm32-unknown-unknown", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(data, "runtimes", "rust", "1.83.0+clippy+rust-src+wasm32-unknown-unknown-x86_64-unknown-linux-gnu"); root != want {
		t.Fatalf("root %s, want %s", root, want)
	}
	for _, f := range []string{"bin/rustc", "bin/cargo", "bin/cargo-clippy", "lib/librustc_driver.so",
		"lib/rustlib/x86_64-unknown-linux-gnu/lib/libcore.rlib", "lib/rustlib/wasm32-unknown-unknown/lib/libcore.rlib",
		"lib/rustlib/src/rust/library/core/lib.rs"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	for _, f := range []string{"install.sh", "components", "manifest.in"} {
		if _, err := os.Lstat(filepath.Join(root, f)); err == nil {
			t.Errorf("installer file %s installed", f)
		}
	}
	if l, err := os.Readlink(filepath.Join(root, "cargo-home-bin")); err != nil || l != "/home/runner/.cargo/bin" {
		t.Errorf("cargo-home-bin: %q %v", l, err)
	}
	if left, _ := filepath.Glob(filepath.Join(data, "runtimes", "rust", ".install-*")); len(left) != 0 {
		t.Errorf("temp dirs left: %v", left)
	}
	n := hits.Load()
	if again, err := provisionRust(data, "1.83.0+clippy+rust-src+wasm32-unknown-unknown", "linux", "amd64"); err != nil || again != root || hits.Load() != n {
		t.Fatalf("second provision: %v %s (%d requests)", err, again, hits.Load()-n)
	}

	// Through the registry: the toolchain and ~/.cargo/bin on PATH, cargo's download caches.
	tk := &Task{ID: "t", Argv: []string{"true"}, Platform: PlatformAny, Runtime: Runtime{Other: map[string]string{"rust": "1.83.0"}}}
	bins, _, caches, err := tk.RuntimeSandbox(data, "linux", "amd64")
	if err != nil || len(bins) != 2 || bins[0] != filepath.Join(data, "runtimes", "rust", "1.83.0-x86_64-unknown-linux-gnu", "bin") ||
		caches["cargo-registry"] != ".cargo/registry" || caches["cargo-git"] != ".cargo/git" {
		t.Fatalf("sandbox: %v %v %v", err, bins, caches)
	}
}

func TestRustProvisionRefused(t *testing.T) {
	cases := []struct {
		name, ver    string
		edit         func(string) string
		sum, wantErr string
	}{
		{"manifest checksum", "1.83.0", nil, strings.Repeat("0", 64), "channel manifest: checksum mismatch"},
		{"tarball checksum", "1.83.0", func(m string) string {
			i := strings.Index(m, "[pkg.cargo.")
			j := i + strings.Index(m[i:], "hash = \"") + 8
			return m[:j] + strings.Repeat("0", 64) + m[j+64:]
		}, "", "cargo: checksum mismatch"},
		{"another host", "1.83.0", func(m string) string {
			return strings.Replace(m, "https://static.rust-lang.org/dist/2024-11-28/cargo", "https://example.com/dist/2024-11-28/cargo", 1)
		}, "", "isn't on https://static.rust-lang.org/dist/"},
		{"unknown component", "1.83.0+miri", nil, "", `no component or target "miri"`},
		{"unavailable component", "1.83.0+rustfmt-preview", nil, "", "has no rustfmt-preview"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeRustDist(t, c.edit, c.sum)
			data := t.TempDir()
			_, err := provisionRust(data, c.ver, "linux", "amd64")
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want %q", err, c.wantErr)
			}
			if left, _ := os.ReadDir(filepath.Join(data, "runtimes", "rust")); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
	for _, v := range []string{"1.83", "stable", "nightly", "1.83.0+Clippy", "1.83.0-x86_64-unknown-linux-gnu", "nightly-2026-1-01"} {
		if rustVersionRE.MatchString(v) {
			t.Errorf("%q accepted as exact", v)
		}
	}
	for _, v := range []string{"1.83.0", "nightly-2026-10-01", "beta-2026-10-01", "1.83.0+clippy+rustfmt+wasm32-unknown-unknown"} {
		if !rustVersionRE.MatchString(v) {
			t.Errorf("%q refused", v)
		}
	}
	if _, err := provisionRust(t.TempDir(), "1.83.0", "windows", "amd64"); err == nil {
		t.Error("windows provisioned")
	}
}
