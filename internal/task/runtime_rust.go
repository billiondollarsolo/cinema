package task

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A pinned Rust toolchain: a release (1.83.0) or a dated nightly/beta
// (nightly-2026-10-01), then "+name" for each component (clippy, rust-src)
// and extra target (wasm32-unknown-unknown) beyond rustup's minimal profile:
// what a CI toolchain can do is part of the pin, so receipts for a toolchain
// without clippy never stand in for one with it. Like semver build metadata,
// the part after the first "+" isn't the version.
var rustVersionRE = regexp.MustCompile(`^(1\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)|(nightly|beta)-[0-9]{4}-[0-9]{2}-[0-9]{2})(\+[a-z0-9][a-z0-9_.-]*)*$`)

// rustDist is static.rust-lang.org's dist root (a variable so tests can point
// it at a fake). Manifests name packages by absolute URL under it.
var rustDist = "https://static.rust-lang.org/dist"

const rustOfficialDist = "https://static.rust-lang.org/dist/"

// The runner keeps `cargo install`ed tools in ~/.cargo/bin, on its PATH. Bins
// are under the install root, so the install carries a link to it.
const rustCargoBin = "/home/runner/.cargo/bin"

func init() {
	RegisterRuntime(&RuntimeKind{
		Name: "rust", Example: "1.83.0", Version: rustVersionRE,
		Provision: provisionRust,
		Bins:      []string{"bin", "cargo-home-bin"},
		// CARGO_HOME stays ~/.cargo, writable: crates and git checkouts are
		// content-keyed downloads; ~/.cargo/bin is the run's own.
		Caches: map[string]string{"cargo-registry": ".cargo/registry", "cargo-git": ".cargo/git"},
		Probe:  []string{"rustc", "--version"},
	})
}

func rustHost(osName, arch string) (string, error) {
	cpu := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[arch]
	sys := map[string]string{"linux": "unknown-linux-gnu", "darwin": "apple-darwin"}[osName]
	if cpu == "" || sys == "" {
		return "", fmt.Errorf("no Rust build for %s/%s", osName, arch)
	}
	return cpu + "-" + sys, nil
}

// provisionRust installs toolchain ver for osName/arch under
// <dataDir>/runtimes/rust, as rustup would but without it (the runner image
// has none): the channel manifest, checked against its published SHA-256,
// names each package's tarball and SHA-256; each tarball's components are
// merged into one prefix, as its install.sh does (not run: it writes
// uninstall metadata and wants a writable prefix later). rustc finds its
// sysroot next to its binary, so the install works anywhere, read-only.
func provisionRust(dataDir, ver, osName, arch string) (string, error) {
	if !rustVersionRE.MatchString(ver) {
		return "", fmt.Errorf("not an exact Rust toolchain: %q", ver)
	}
	host, err := rustHost(osName, arch)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dataDir, "runtimes", "rust")
	dest := filepath.Join(root, ver+"-"+host)
	if fi, err := os.Stat(filepath.Join(dest, "bin", "rustc")); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now) // last use, for the cleanup sweep
		return dest, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	base, extras, _ := strings.Cut(ver, "+")
	manifest := "/channel-rust-" + base + ".toml"
	if ch, date, ok := strings.Cut(base, "-"); ok { // nightly-2026-10-01
		manifest = "/" + date + "/channel-rust-" + ch + ".toml"
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	m, err := httpGet(client, rustDist+manifest, 64<<20)
	if err != nil {
		return "", err
	}
	sum, err := httpGet(client, rustDist+manifest+".sha256", 4<<10)
	if err != nil {
		return "", err
	}
	if f := strings.Fields(string(sum)); len(f) == 0 || len(f[0]) != 64 || f[0] != sha256hex(m) {
		return "", errors.New("channel manifest: checksum mismatch")
	}
	pkgs, renames, err := parseRustManifest(m)
	if err != nil {
		return "", err
	}
	want := []rustPkg{pkgs["rustc "+host], pkgs["cargo "+host], pkgs["rust-std "+host]}
	for _, x := range strings.Split(extras, "+") {
		name := x
		if to := renames[x]; to != "" {
			name = to // clippy → clippy-preview
		}
		switch p, ok := pkgs[name+" "+host]; {
		case x == "" || x == host:
		case ok:
			want = append(want, p)
		case pkgs[name+" *"].name != "": // rust-src: one for every host
			want = append(want, pkgs[name+" *"])
		case pkgs["rust-std "+x].name != "":
			want = append(want, pkgs["rust-std "+x])
		default:
			return "", fmt.Errorf("%s: no component or target %q in Rust %s for %s", ver, x, base, host)
		}
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for _, p := range want {
		if p.name == "" || !p.available {
			return "", fmt.Errorf("%s: Rust %s has no %s for %s", ver, base, cmp.Or(p.name, "rustc, cargo or rust-std"), host)
		}
		if err := installRustPkg(client, root, tmp, p); err != nil {
			return "", fmt.Errorf("%s: %w", p.name, err)
		}
	}
	if err := os.Symlink(rustCargoBin, filepath.Join(tmp, "cargo-home-bin")); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(dest, "bin", "rustc")); serr == nil {
			return dest, nil // another process installed it first
		}
		return "", err
	}
	return dest, nil
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// installRustPkg downloads one package tarball, checks it, and merges the
// components it lists into prefix.
func installRustPkg(client *http.Client, root, prefix string, p rustPkg) error {
	rel, ok := strings.CutPrefix(p.url, rustOfficialDist)
	if !ok || !strings.HasSuffix(rel, ".tar.gz") || strings.Contains(rel, "..") {
		return fmt.Errorf("tarball %s isn't on %s", p.url, rustOfficialDist)
	}
	tarball, err := httpGet(client, rustDist+"/"+rel, 1<<30)
	if err != nil {
		return err
	}
	if len(p.hash) != 64 || sha256hex(tarball) != p.hash {
		return errors.New("checksum mismatch")
	}
	stage, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	top := strings.TrimSuffix(filepath.Base(rel), ".tar.gz") // rustc-1.83.0-x86_64-unknown-linux-gnu
	if err := extractTarGz(tarball, stage, top+"/"); err != nil {
		return err
	}
	list, err := os.ReadFile(filepath.Join(stage, "components"))
	if err != nil {
		return fmt.Errorf("no components list: %w", err)
	}
	for _, c := range strings.Fields(string(list)) {
		if !filepath.IsLocal(c) || strings.ContainsRune(c, '/') {
			return fmt.Errorf("bad component name %q", c)
		}
		if err := mergeRustComponent(filepath.Join(stage, c), prefix); err != nil {
			return fmt.Errorf("component %s: %w", c, err)
		}
	}
	return nil
}

// mergeRustComponent moves a component's tree into prefix: the files its
// manifest.in lists (file: and dir: entries, which are the whole tree), as
// install.sh copies them. Later components overwrite, as cp does.
func mergeRustComponent(dir, prefix string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() || rel == "manifest.in" {
			return nil
		}
		target := filepath.Join(prefix, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.Rename(p, target)
	})
}

type rustPkg struct {
	name      string
	available bool
	url, hash string // the .tar.gz (Go's library has no xz)
}

// parseRustManifest reads what provisioning needs from a channel manifest
// (TOML, written by one tool in one shape): [pkg.<name>.target.<triple>]
// tables, keyed "name triple", and [renames.<alias>] to = "<name>".
func parseRustManifest(b []byte) (map[string]rustPkg, map[string]string, error) {
	pkgs, renames := map[string]rustPkg{}, map[string]string{}
	key, alias := "", ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			key, alias = "", ""
			h := strings.Trim(line, "[]")
			if a, ok := strings.CutPrefix(h, "renames."); ok {
				alias = a
			} else if rest, ok := strings.CutPrefix(h, "pkg."); ok {
				if name, target, ok := strings.Cut(rest, ".target."); ok {
					key = name + " " + strings.Trim(target, `"`)
					pkgs[key] = rustPkg{name: name}
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || (key == "" && alias == "") {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if alias != "" {
			if k == "to" {
				renames[alias], _ = strconv.Unquote(v)
			}
			continue
		}
		p := pkgs[key]
		switch k {
		case "available":
			p.available = v == "true"
		case "url":
			p.url, _ = strconv.Unquote(v)
		case "hash":
			p.hash, _ = strconv.Unquote(v)
		}
		pkgs[key] = p
	}
	if len(pkgs) == 0 {
		return nil, nil, errors.New("channel manifest lists no packages")
	}
	return pkgs, renames, nil
}
