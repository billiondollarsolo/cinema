package analyze

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Rust setup actions run rustup on the runner. Their translation pins the
// toolchain cinema provisions (task/runtime_rust.go): the exact release, then
// "+<name>" for each component and target beyond rustup's minimal profile, so
// what the toolchain can do (cargo clippy, a wasm build) is part of the pin.

var rustExactRE = regexp.MustCompile(`^(1\.[0-9]+\.[0-9]+|(nightly|beta)-[0-9]{4}-[0-9]{2}-[0-9]{2})$`)

func init() {
	RegisterReleases("rust", rustReleases)
	// The toolchain (dtolnay@stable) and the defaults (setup-rust-toolchain@v1) depend on the ref.
	RegisterSetupAction("dtolnay/rust-toolchain", func(c *SetupCtx) { dtolnayToolchain(c, c.Ref) })
	RegisterSetupAction("actions-rust-lang/setup-rust-toolchain", func(c *SetupCtx) { setupRustToolchain(c, c.Ref) })
	RegisterSetupAction("actions-rs/toolchain", actionsRSToolchain)
	RegisterSetupAction("Swatinem/rust-cache", rustCache)
}

// rustReleases lists stable releases, newest first, from static.rust-lang.org's
// index of every channel manifest it publishes, so "stable" and "1.83" resolve
// as rustup resolves them (dated nightlies and betas are exact already).
func rustReleases() ([]Release, error) {
	b, err := fetch("https://static.rust-lang.org/manifests.txt")
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`/channel-rust-(1\.([0-9]+)\.([0-9]+))\.toml$`)
	type v struct {
		s            string
		minor, patch int
	}
	var vs []v
	for _, line := range strings.Split(string(b), "\n") {
		if m := re.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			minor, _ := strconv.Atoi(m[2])
			patch, _ := strconv.Atoi(m[3])
			vs = append(vs, v{m[1], minor, patch})
		}
	}
	slices.SortFunc(vs, func(a, b v) int {
		if a.minor != b.minor {
			return b.minor - a.minor
		}
		return b.patch - a.patch
	})
	vs = slices.CompactFunc(vs, func(a, b v) bool { return a.s == b.s })
	out := make([]Release, len(vs))
	for i, x := range vs {
		out[i] = Release{Version: x.s, Stable: true}
	}
	return out, nil
}

// rustPin resolves spec as rustup would today and pins it with extras.
func rustPin(c *SetupCtx, spec string, extras []string) {
	if strings.Contains(spec, ",") {
		c.Review("Rust: several toolchains (" + spec + "): cinema provisions one; jobs that pick one with cargo +<toolchain> need rustup")
		return
	}
	ver := c.Exact("rust", spec, "", rustExactRE)
	if ver == "" {
		return
	}
	if spec == "stable" {
		// The runner's preinstalled stable has clippy and rustfmt; installing
		// stable updates that toolchain in place, components kept.
		extras = append(extras, "clippy", "rustfmt")
	}
	var set []string
	for _, x := range extras {
		x = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(x)), "-preview")
		switch {
		case x == "" || x == "rustc" || x == "cargo" || x == "rust-std" || x == "rust-mingw" || x == "rust-docs":
			// in every toolchain, Windows-only, or documentation: no effect on a Linux job
		case !rustNameRE.MatchString(x):
			c.Review("Rust component or target " + strconv.Quote(x) + ": not a name rustup takes")
			return
		default:
			set = append(set, x)
		}
	}
	slices.Sort(set)
	c.Pin("rust", strings.Join(append([]string{ver}, slices.Compact(set)...), "+"))
}

var rustNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

func rustList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// rustEnv is a step that sets each variable as the action does: only if the
// job hasn't set it.
func rustEnv(c *SetupCtx, kv ...string) {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(`[ -n "${` + kv[i] + `+set}" ] || echo ` + shellQuote(kv[i]+"="+kv[i+1]) + ` >> "$GITHUB_ENV"` + "\n")
	}
	c.Step(strings.TrimSuffix(b.String(), "\n"), "")
}

var dtolnayBranch = regexp.MustCompile(`^(stable|beta|nightly|1\.[0-9]+(\.[0-9]+)?)$`)

// dtolnayToolchain: `rustup toolchain install <toolchain> --profile minimal
// --component … --target …` then `rustup default`. On branch refs (@stable,
// @nightly, @1.83) the toolchain input defaults to the ref.
func dtolnayToolchain(c *SetupCtx, ref string) {
	spec := c.With("toolchain")
	if spec == "" && dtolnayBranch.MatchString(ref) {
		spec = ref
	}
	if spec == "" {
		c.Review("dtolnay/rust-toolchain: the toolchain comes from the action's ref (@stable, @1.83), which cinema can't read yet: set with: toolchain")
		return
	}
	extras := append(rustList(c.With("components")), rustList(cmp.Or(c.With("targets"), c.With("target")))...)
	spec, extras, ok := rustToolchainFile(c, spec, extras, false)
	if !ok {
		return
	}
	rustPin(c, spec, extras)
	rustEnv(c, "CARGO_INCREMENTAL", "0", "CARGO_TERM_COLOR", "always")
}

// setupRustToolchain: like dtolnay's, plus a rust-toolchain file when no
// toolchain is given, `rustup override` (beats the file) by default, and
// variables. v1 sets RUSTFLAGS="-D warnings" and v2 CARGO_BUILD_WARNINGS=deny
// by default: warnings fail the build, so the major version is part of what
// the job checks.
func setupRustToolchain(c *SetupCtx, ref string) {
	if c.With("rust-src-dir") != "" {
		c.Review("setup-rust-toolchain: rust-src-dir reads the toolchain file of another directory: pin the toolchain in runtime:")
		return
	}
	spec := c.With("toolchain")
	extras := append(rustList(c.With("components")), rustList(c.With("target"))...)
	if spec == "" {
		file, _, _ := readRustToolchain(c.Root)
		switch {
		case file == nil:
			spec = "stable"
		case file.channel == "stable":
			// `rustup show active-toolchain` succeeds on the runner's own stable,
			// which isn't updated: the image's release, not today's.
			c.Review("setup-rust-toolchain: the toolchain file names stable, and CI uses the runner image's stable as installed (not updated): pin an exact release in the file")
			return
		}
	}
	spec, extras, ok := rustToolchainFile(c, spec, extras, c.With("toolchain") != "" && c.With("override") != "false")
	if !ok {
		return
	}
	rustPin(c, spec, extras)
	kv := []string{"CARGO_INCREMENTAL", "0", "CARGO_PROFILE_DEV_DEBUG", "0", "CARGO_TERM_COLOR", "always",
		"RUST_BACKTRACE", "short", "CARGO_UNSTABLE_SPARSE_REGISTRY", "true", "CARGO_REGISTRIES_CRATES_IO_PROTOCOL", "sparse"}
	flags, warnings := c.With("rustflags"), c.With("build-warnings")
	switch major, _, _ := strings.Cut(ref, "."); major {
	case "v1":
		// An empty rustflags input reads as unset (the default): the only
		// difference is -D warnings, unless it was emptied to keep the
		// rustflags in .cargo/config, which RUSTFLAGS replaces.
		if flags == "" && rustConfigFlags(c.Root) {
			c.Review("setup-rust-toolchain@v1 sets RUSTFLAGS=\"-D warnings\" unless rustflags is set, replacing the rustflags in .cargo/config: set rustflags: explicitly")
			return
		}
		kv = append(kv, "RUSTFLAGS", cmp.Or(flags, "-D warnings"))
	case "v2":
		if flags != "" {
			kv = append(kv, "RUSTFLAGS", flags)
		}
		kv = append(kv, "CARGO_BUILD_WARNINGS", cmp.Or(warnings, "deny"))
	default:
		if c.Env["RUSTFLAGS"] == "" || c.Env["CARGO_BUILD_WARNINGS"] == "" {
			c.Review("setup-rust-toolchain: v1 sets RUSTFLAGS=\"-D warnings\", v2 CARGO_BUILD_WARNINGS=deny, and cinema can't read the action's ref yet: set both in the job's env")
		}
	}
	rustEnv(c, kv...)
}

// actionsRSToolchain: actions-rs/toolchain (archived, still common) installs
// the toolchain and makes it the default or override only when asked.
func actionsRSToolchain(c *SetupCtx) {
	spec, override := c.With("toolchain"), c.With("override") == "true"
	extras := append(rustList(c.With("components")), c.With("target"))
	switch c.With("profile") {
	case "", "minimal": // the runner's rustup profile is minimal
	case "default":
		extras = append(extras, "clippy", "rustfmt")
	default:
		c.Review("actions-rs/toolchain: profile " + c.With("profile") + ": pin the toolchain in runtime:")
		return
	}
	if spec == "" {
		// It reads ./rust-toolchain whole as the toolchain's name (not .toml).
		file, name, _ := readRustToolchain(c.Root)
		if file == nil || name != "rust-toolchain" || file.toml {
			c.Review("actions-rs/toolchain: no toolchain input and no one-line rust-toolchain file: CI fails, or cinema can't tell what it installs")
			return
		}
		spec = file.channel
	} else if !override && c.With("default") != "true" && spec != "stable" {
		c.Review("actions-rs/toolchain installs " + spec + " without default: or override: true: cargo uses the runner's preinstalled stable")
		return
	}
	spec, extras, ok := rustToolchainFile(c, spec, extras, override)
	if ok {
		rustPin(c, spec, extras)
	}
}

// rustCache restores cargo's caches (no effect on results) and sets
// CARGO_INCREMENTAL=0.
func rustCache(c *SetupCtx) { c.Step(`echo CARGO_INCREMENTAL=0 >> "$GITHUB_ENV"`, "") }

// rustToolchainFile applies the repo's rust-toolchain file: rustup prefers it
// to the default an action set (not to an override), and installs its
// components and targets. spec "" means the action reads the file.
func rustToolchainFile(c *SetupCtx, spec string, extras []string, override bool) (string, []string, bool) {
	file, name, why := readRustToolchain(c.Root)
	switch {
	case why != "":
		c.Review(why)
		return "", nil, false
	case file == nil || override:
		return spec, extras, true
	case spec != "" && spec != file.channel:
		c.Review("Rust: " + name + " names " + file.channel + ", which rustup uses over the action's " + spec + ": cinema provisions one toolchain")
		return "", nil, false
	}
	c.Note("Rust toolchain from " + name)
	return file.channel, append(extras, file.extras...), true
}

type rustFile struct {
	channel string
	extras  []string // components and targets, and the profile's
	toml    bool
}

// readRustToolchain reads rust-toolchain.toml or the legacy rust-toolchain
// (one line, or TOML) at the repo root, as rustup does. nil if there is none;
// why if cinema can't read it the way rustup would.
func readRustToolchain(root string) (f *rustFile, name, why string) {
	if root == "" {
		return nil, "", ""
	}
	var found []string
	for _, n := range []string{"rust-toolchain", "rust-toolchain.toml"} {
		if _, err := os.Lstat(filepath.Join(root, n)); err == nil {
			found = append(found, n)
		}
	}
	switch len(found) {
	case 0:
		return nil, "", ""
	case 2:
		return nil, "", "Rust: both rust-toolchain and rust-toolchain.toml: pin the toolchain in runtime:"
	}
	name = found[0]
	src := versionFromFile(filepath.Join(root, name)) // no symlinks, capped
	bad := func(what string) (*rustFile, string, string) {
		return nil, name, "Rust: " + name + ": " + what + ": pin the toolchain in runtime:"
	}
	if src == "" {
		return bad("unreadable or empty")
	}
	if name == "rust-toolchain" && !strings.Contains(src, "[toolchain]") && !strings.ContainsAny(src, "\n=") {
		return &rustFile{channel: src}, name, ""
	}
	f = &rustFile{toml: true}
	lines := strings.Split(src, "\n")
	section := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(rustUncomment(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		for ok && strings.HasPrefix(v, "[") && !strings.Contains(v, "]") && i+1 < len(lines) {
			i++
			v += " " + strings.TrimSpace(rustUncomment(lines[i]))
		}
		if !ok || section != "[toolchain]" {
			return bad("unexpected " + strconv.Quote(line))
		}
		var strs []string
		for _, m := range rustTOMLString.FindAllStringSubmatch(v, -1) {
			strs = append(strs, m[1]+m[2])
		}
		switch k {
		case "channel":
			if len(strs) != 1 {
				return bad("channel " + v)
			}
			f.channel = strs[0]
		case "components", "targets":
			f.extras = append(f.extras, strs...)
		case "profile":
			switch strings.Join(strs, "") {
			case "minimal":
			case "default":
				f.extras = append(f.extras, "clippy", "rustfmt")
			default:
				return bad("profile " + v)
			}
		default: // path: a custom toolchain
			return bad(k)
		}
	}
	if f.channel == "" {
		return bad("no channel")
	}
	return f, name, ""
}

var rustTOMLString = regexp.MustCompile(`"([^"\\]*)"|'([^']*)'`)

// rustUncomment drops a TOML comment (a # outside quotes).
func rustUncomment(s string) string {
	var q rune
	for i, r := range s {
		switch {
		case q != 0 && r == q:
			q = 0
		case q == 0 && (r == '"' || r == '\''):
			q = r
		case q == 0 && r == '#':
			return s[:i]
		}
	}
	return s
}

// rustConfigFlags: the repo's .cargo/config sets rustflags.
func rustConfigFlags(root string) bool {
	for _, n := range []string{"config.toml", "config"} {
		if b, err := os.ReadFile(filepath.Join(root, ".cargo", n)); err == nil && strings.Contains(string(b), "rustflags") {
			return true
		}
	}
	return false
}
