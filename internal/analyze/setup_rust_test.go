package analyze

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rustJob analyzes a one-job workflow (steps, then `cargo test`) in a repo
// holding files, with static.rust-lang.org's manifest index faked.
func rustJob(t *testing.T, steps string, files map[string]string) Job {
	t.Helper()
	old := fetch
	t.Cleanup(func() { fetch = old; releases.m = map[string][]release{} })
	releases.m = map[string][]release{}
	fetch = func(url string) ([]byte, error) {
		if url != "https://static.rust-lang.org/manifests.txt" {
			return nil, errors.New("offline")
		}
		return []byte(`static.rust-lang.org/dist/2024-10-17/channel-rust-1.82.0.toml
static.rust-lang.org/dist/2024-11-28/channel-rust-1.83.0.toml
static.rust-lang.org/dist/2024-11-28/channel-rust-nightly.toml
static.rust-lang.org/dist/2024-09-05/channel-rust-1.81.0.toml
static.rust-lang.org/dist/2024-11-28/channel-rust-beta.toml
static.rust-lang.org/dist/2024-12-04/channel-rust-1.82.1.toml
`), nil
	}
	root := t.TempDir()
	files[".github/workflows/ci.yml"] = "on: pull_request\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n" + steps + "      - run: cargo test\n"
	for p, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := Repo(root)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%v %+v", err, jobs)
	}
	return jobs[0]
}

func TestRustSetupActions(t *testing.T) {
	cases := []struct {
		name, steps string
		files       map[string]string
		pin         string // "" with review: the reason contains review
		review      string
		body        []string // in the script
	}{
		{name: "dtolnay stable", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: stable, components: 'rustfmt, clippy', targets: wasm32-unknown-unknown}\n",
			pin: "1.83.0+clippy+rustfmt+wasm32-unknown-unknown", body: []string{`echo CARGO_INCREMENTAL=0 >> "$GITHUB_ENV"`}},
		{name: "dtolnay exact, minimal", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: 1.81.0}\n", pin: "1.81.0"},
		{name: "dtolnay minor", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: '1.82', components: clippy-preview}\n", pin: "1.82.1+clippy"},
		{name: "dtolnay dated nightly", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: nightly-2024-11-01, components: miri}\n", pin: "nightly-2024-11-01+miri"},
		{name: "dtolnay ref", steps: "      - uses: dtolnay/rust-toolchain@stable\n", pin: "1.83.0+clippy+rustfmt"}, // the toolchain is the ref
		{name: "dtolnay floating nightly", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: nightly}\n", review: `"nightly" isn't exact`},
		{name: "toolchain file wins over default", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: stable}\n",
			files: map[string]string{"rust-toolchain.toml": "[toolchain]\nchannel = \"1.81.0\"\n"}, review: "names 1.81.0, which rustup uses over the action's stable"},
		{name: "toolchain file adds components", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: 1.81.0}\n",
			files: map[string]string{"rust-toolchain.toml": "# pinned\n[toolchain]\nchannel = \"1.81.0\" # msrv\ncomponents = [\n  \"rust-src\",\n  'llvm-tools-preview',\n]\ntargets = [\"thumbv7em-none-eabihf\"]\nprofile = \"minimal\"\n"},
			pin:   "1.81.0+llvm-tools+rust-src+thumbv7em-none-eabihf"},
		{name: "toolchain file path", steps: "      - uses: dtolnay/rust-toolchain@master\n        with: {toolchain: 1.81.0}\n",
			files: map[string]string{"rust-toolchain.toml": "[toolchain]\npath = \"/opt/rust\"\n"}, review: "rust-toolchain.toml: path"},
		{name: "setup-rust-toolchain reads the file", steps: "      - uses: actions-rust-lang/setup-rust-toolchain@v1\n        with: {components: clippy}\n",
			files: map[string]string{"rust-toolchain": "1.81.0\n"}, pin: "1.81.0+clippy",
			body: []string{`echo RUST_BACKTRACE=short >> "$GITHUB_ENV"`}},
		{name: "setup-rust-toolchain overrides the file", steps: "      - uses: actions-rust-lang/setup-rust-toolchain@v1\n        with: {toolchain: 1.82.0}\n",
			files: map[string]string{"rust-toolchain": "1.81.0\n"}, pin: "1.82.0", body: []string{`RUSTFLAGS=-D warnings`}},
		{name: "setup-rust-toolchain stable file", steps: "      - uses: actions-rust-lang/setup-rust-toolchain@v1\n",
			files: map[string]string{"rust-toolchain.toml": "[toolchain]\nchannel = \"stable\"\n"}, review: "runner image's stable"},
		{name: "actions-rs not default", steps: "      - uses: actions-rs/toolchain@v1\n        with: {toolchain: nightly-2024-11-01}\n", review: "without default: or override: true"},
		{name: "actions-rs override", steps: "      - uses: actions-rs/toolchain@v1\n        with: {toolchain: 1.81.0, override: true, profile: default, target: x86_64-unknown-linux-musl}\n",
			files: map[string]string{"rust-toolchain": "1.80.0"}, pin: "1.81.0+clippy+rustfmt+x86_64-unknown-linux-musl"},
		{name: "actions-rs toml only", steps: "      - uses: actions-rs/toolchain@v1\n",
			files: map[string]string{"rust-toolchain.toml": "[toolchain]\nchannel = \"1.81.0\"\n"}, review: "no one-line rust-toolchain file"},
		{name: "rust-cache", steps: "      - uses: Swatinem/rust-cache@v2\n", body: []string{`echo CARGO_INCREMENTAL=0 >> "$GITHUB_ENV"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.files == nil {
				c.files = map[string]string{}
			}
			j := rustJob(t, c.steps, c.files)
			reasons := strings.Join(j.Reasons, "; ")
			if j.Proposal == nil {
				t.Fatalf("no proposal: %s %v", j.Verdict, j.Reasons)
			}
			if got := j.Proposal.Runtimes["rust"]; got != c.pin {
				t.Errorf("pin %q, want %q (%v)", got, c.pin, j.Reasons)
			}
			if c.review == "" && j.Verdict != Task {
				t.Errorf("verdict %s: %s", j.Verdict, reasons)
			}
			if c.review != "" && !strings.Contains(reasons, c.review) {
				t.Errorf("reasons %q, want %q", reasons, c.review)
			}
			for _, b := range c.body {
				if !strings.Contains(j.Proposal.Body, b) {
					t.Errorf("script lacks %q:\n%s", b, j.Proposal.Body)
				}
			}
		})
	}
}

// With the ref known, setup-rust-toolchain's defaults are mirrored per major
// version, only where the job hasn't set the variable, as the action does.
func TestSetupRustToolchainRef(t *testing.T) {
	run := func(ref string, with map[string]string, env map[string]string) (string, string, []string) {
		var pin, step string
		var reviews []string
		c := &SetupCtx{With: func(k string) string { return with[k] }, Env: env,
			pin:    func(_, v string) { pin = v },
			step:   func(run, _ string) { step = run },
			Review: func(s string) { reviews = append(reviews, s) }, Note: func(string) {}}
		setupRustToolchain(c, ref)
		return pin, step, reviews
	}
	releases.m = map[string][]release{"rust": {{Version: "1.83.0", Stable: true}}}
	t.Cleanup(func() { releases.m = map[string][]release{} })

	pin, step, rev := run("v1", nil, nil)
	if pin != "1.83.0+clippy+rustfmt" || len(rev) != 0 || !strings.Contains(step, `echo 'RUSTFLAGS=-D warnings'`) || strings.Contains(step, "CARGO_BUILD_WARNINGS") {
		t.Errorf("v1: %q %v\n%s", pin, rev, step)
	}
	_, step, _ = run("v2.0.0", map[string]string{"rustflags": "--cfg x"}, nil)
	if !strings.Contains(step, `'RUSTFLAGS=--cfg x'`) || !strings.Contains(step, "echo CARGO_BUILD_WARNINGS=deny ") {
		t.Errorf("v2:\n%s", step)
	}
	if _, _, rev = run("", nil, map[string]string{"RUSTFLAGS": "-D warnings", "CARGO_BUILD_WARNINGS": "deny"}); len(rev) != 0 {
		t.Errorf("unknown ref with both set in env: %v", rev)
	}

	// The step really leaves a variable the job set alone.
	d := t.TempDir()
	cmd := exec.Command("bash", "-ec", "export GITHUB_ENV="+filepath.Join(d, "env")+" RUSTFLAGS=mine\n"+step)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(d, "env"))
	if strings.Contains(string(b), "RUSTFLAGS") || !strings.Contains(string(b), "CARGO_BUILD_WARNINGS=deny\n") {
		t.Errorf("GITHUB_ENV:\n%s", b)
	}

	// dtolnay's branch refs are the toolchain.
	var pin2 string
	c := &SetupCtx{With: func(string) string { return "" }, pin: func(_, v string) { pin2 = v }, step: func(string, string) {},
		Review: func(s string) { t.Error(s) }, Note: func(string) {}}
	dtolnayToolchain(c, "stable")
	if pin2 != "1.83.0+clippy+rustfmt" {
		t.Errorf("dtolnay@stable: %q", pin2)
	}
}
