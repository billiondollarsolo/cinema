package task

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

const good = `
schema_version: 1
tasks:
  backend-unit:
    argv: [go, test, -count=1, ./...]
    platform: linux/amd64
    toolchain:
      - cmd: [go, version]
    env: [CGO_ENABLED]
    reuse: { allowed: true, max_age: 72h }
  frontend-lint:
    argv: [npm, run, lint]
    cwd: web
    platform: any
    toolchain:
      - file: web/package-lock.json
    reuse: { allowed: true, max_age: 24h }
  integration-db:
    argv: [make, integration-test]
    platform: linux/amd64
    reuse: { allowed: false }
`

func mustParse(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseGood(t *testing.T) {
	c := mustParse(t, good)
	be := c.Tasks["backend-unit"]
	if be.Cwd != "." || be.VCSContext != "none" || be.Reuse.MaxAge.Hours() != 72 {
		t.Fatalf("defaults: %+v", be)
	}
	if c.Tasks["integration-db"].Reuse.Allowed {
		t.Fatal("reuse.allowed false not honored")
	}
}

func TestParseRejects(t *testing.T) {
	task := func(body string) string {
		return "schema_version: 1\ntasks:\n  t:\n    argv: [make]\n    platform: any\n" + body
	}
	for name, cfg := range map[string]string{
		"unknown field":       task("    platfrom: any\n"),
		"unknown top level":   "schema_version: 1\nextra: 1\ntasks:\n  t: {argv: [make], platform: any}\n",
		"duplicate task":      "schema_version: 1\ntasks:\n  t: {argv: [a], platform: any}\n  t: {argv: [b], platform: any}\n",
		"duplicate key":       task("    cwd: a\n    cwd: b\n"),
		"two documents":       task("") + "---\nschema_version: 1\n",
		"no platform":         "schema_version: 1\ntasks:\n  t: {argv: [make]}\n",
		"bad platform":        "schema_version: 1\ntasks:\n  t: {argv: [make], platform: macos}\n",
		"wrong schema":        "schema_version: 2\ntasks:\n  t: {argv: [make], platform: any}\n",
		"no tasks":            "schema_version: 1\ntasks: {}\n",
		"empty argv":          "schema_version: 1\ntasks:\n  t: {argv: [], platform: any}\n",
		"bad id":              "schema_version: 1\ntasks:\n  Bad_ID: {argv: [make], platform: any}\n",
		"cwd escapes":         task("    cwd: ../other\n"),
		"extra escapes":       task("    extra_inputs: [/etc/passwd]\n"),
		"toolchain both":      task("    toolchain: [{cmd: [go, version], file: go.mod}]\n"),
		"toolchain neither":   task("    toolchain: [{}]\n"),
		"bad env name":        task("    env: [\"A B\"]\n"),
		"toolchain repo path": task("    toolchain: [{cmd: [./scripts/v.sh]}]\n"),
		"toolchain abs path":  task("    toolchain: [{cmd: [/tmp/v.sh]}]\n"),
		"bad vcs":             task("    vcs_context: sometimes\n"),
		"reuse without age":   task("    reuse: {allowed: true}\n"),
		"go test cache":       "schema_version: 1\ntasks:\n  t: {argv: [go, test, ./...], platform: any}\n", // ACC-020
		"null task":           "schema_version: 1\ntasks:\n  t:\n",
		"not a mapping":       "- a\n- b\n",
		"oversized":           strings.Repeat("#", maxConfigSize+1),
	} {
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// accept_tool_cache: true is the explicit opt-in; other uncacheable flags also qualify.
	for _, argv := range []string{"[go, test, ./...]\n    accept_tool_cache: true", "[go, test, -count=1, ./...]",
		"[go, test, -count, \"1\", ./...]", "[go, test, -fuzz, FuzzX, ./x]", "[go, test, --bench=., ./...]"} {
		mustParse(t, "schema_version: 1\ntasks:\n  t:\n    platform: any\n    argv: "+argv+"\n")
	}
	for _, argv := range []string{"[go, test, -race, ./...]", "[go, test, -run, TestX, -v, ./...]", "[go, test, ./count]"} {
		if _, err := Parse([]byte("schema_version: 1\ntasks:\n  t:\n    platform: any\n    argv: " + argv + "\n")); err == nil {
			t.Errorf("cacheable %s accepted without accept_tool_cache", argv)
		}
	}
}

func TestExactMatch(t *testing.T) { // ACC-001, ACC-018
	c := mustParse(t, good)
	if m := c.Match([]string{"go", "test", "-count=1", "./..."}, "."); m == nil || m.ID != "backend-unit" {
		t.Fatal("exact argv didn't match")
	}
	for _, tc := range []struct {
		argv []string
		cwd  string
	}{
		{[]string{"go", "test", "-count=1", "-run", "TestLogin", "./..."}, "."}, // narrowed filter
		{[]string{"go", "test", "-count=1", "-race", "./..."}, "."},             // extra flag
		{[]string{"go", "test", "./..."}, "."},                                  // cache allowed
		{[]string{"go", "test", "-count=1", "./..."}, "sub"},                    // other directory
		{[]string{"npm", "run", "lint"}, "."},                                   // wrong cwd
	} {
		if m := c.Match(tc.argv, tc.cwd); m != nil {
			t.Errorf("%v in %s matched %s", tc.argv, tc.cwd, m.ID)
		}
	}
	if m := c.Match([]string{"npm", "run", "lint"}, "web"); m == nil {
		t.Fatal("npm task in web/ didn't match")
	}
}

func TestContractDigest(t *testing.T) { // ACC-017
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "web"), 0o755)
	pkg := func(lint string) {
		os.WriteFile(filepath.Join(root, "web", "package.json"),
			[]byte(`{"scripts": {"lint": "`+lint+`", "test": "vitest"}}`), 0o644)
	}
	c := mustParse(t, good)
	lint := c.Tasks["frontend-lint"]
	pkg("eslint .")
	a, err := lint.ContractDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	pkg("eslint --quiet .")
	if b, _ := lint.ContractDigest(root); b == a {
		t.Fatal("script body change didn't change the contract digest")
	}

	// Acceptance policy is not part of the work.
	stricter := mustParse(t, strings.Replace(good, "max_age: 24h", "max_age: 1h", 1)).Tasks["frontend-lint"]
	pkg("eslint .")
	if b, _ := stricter.ContractDigest(root); b != a {
		t.Fatal("max_age changed the contract digest")
	}
	// Order of env names is not meaningful.
	x := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [make], platform: any, env: [A, B]}\n").Tasks["t"]
	y := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [make], platform: any, env: [B, A]}\n").Tasks["t"]
	dx, _ := x.ContractDigest(root)
	dy, _ := y.ContractDigest(root)
	if dx != dy {
		t.Fatal("env order changed the digest")
	}

	pkg("eslint .")
	missing := mustParse(t, strings.Replace(good, "argv: [npm, run, lint]", "argv: [npm, run, nope]", 1)).Tasks["frontend-lint"]
	if _, err := missing.ContractDigest(root); err == nil {
		t.Fatal("missing npm script accepted")
	}
}

func TestEnvFingerprint(t *testing.T) { // ACC-019
	root := t.TempDir()
	tool := filepath.Join(root, "tool.txt")
	version := func(v string) { os.WriteFile(tool, []byte("tool "+v), 0o644) }
	mk := func(platform string) *Task {
		return mustParse(t, "schema_version: 1\ntasks:\n  t:\n    argv: [make]\n    platform: "+platform+
			"\n    toolchain: [{file: tool.txt}]\n    env: [CINEMA_TEST_VAR]\n").Tasks["t"]
	}
	version("1.0")
	native := mk(runtime.GOOS + "/" + runtime.GOARCH)
	a, err := native.EnvFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	version("1.1")
	if b, _ := native.EnvFingerprint(root); b.Digest == a.Digest {
		t.Fatal("toolchain version change not detected")
	}
	version("1.0")
	t.Setenv("CINEMA_TEST_VAR", "")
	if b, _ := native.EnvFingerprint(root); b.Digest == a.Digest {
		t.Fatal("set-but-empty env var looks like unset")
	}
	os.Unsetenv("CINEMA_TEST_VAR")

	other := "linux/amd64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		other = "darwin/arm64"
	}
	// A task for another platform still fingerprints this machine's real platform,
	// so the run can never match the other platform's expectation.
	if b, _ := mk(other).EnvFingerprint(root); b.Digest != a.Digest || b.Inputs[0] != "platform="+Platform() {
		t.Fatalf("foreign-platform task: %v", b.Inputs)
	}
	if b, _ := mk("any").EnvFingerprint(root); b.Digest == a.Digest || b.Inputs[0] != "platform=any" || !strings.HasPrefix(b.Inputs[1], "user=") {
		t.Fatal("platform any should be machine-independent")
	}
	for _, in := range a.Inputs {
		if strings.Contains(in, "=") && strings.HasPrefix(in, "env ") {
			t.Fatalf("env value leaked into inputs: %q", in)
		}
	}
}

func TestActionKeySeparatesInputs(t *testing.T) {
	keys := map[string]bool{}
	for _, in := range [][4]string{
		{"a", "b", "c", ""}, {"a", "b", "c", "d"}, {"ab", "", "c", ""}, {"a", "bc", "", ""},
	} {
		k := ActionKey(in[0], in[1], in[2], in[3])
		if keys[k] {
			t.Fatalf("collision for %v", in)
		}
		keys[k] = true
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(good))
	f.Add([]byte("schema_version: 1\ntasks:\n  t: {argv: [a], platform: any}\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := Parse(b)
		if err != nil {
			return
		}
		for _, tk := range c.Tasks { // anything accepted must be fully validated
			if len(tk.Argv) == 0 || tk.Cwd == "" || (tk.Platform != PlatformAny && !platformRE.MatchString(tk.Platform)) {
				t.Fatalf("accepted invalid task %+v", tk)
			}
			if tk.Reuse.Allowed && tk.Reuse.MaxAge <= 0 {
				t.Fatalf("reuse without max_age: %+v", tk)
			}
		}
	})
}

func TestToolchainProbesOnly(t *testing.T) {
	cfg := func(cmd string) string {
		return "schema_version: 1\ntasks:\n  t:\n    argv: [make]\n    platform: any\n    toolchain: [{cmd: " + cmd + "}]\n"
	}
	for _, ok := range []string{"[go, version]", "[node, --version]", "[rustc, --version]", "[npm, --version]", "[python3.12, --version]", "[java, -version]"} {
		if _, err := Parse([]byte(cfg(ok))); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"[./v.sh, --version]", "[/usr/bin/node, --version]", "[node]", "[node, v.js]", "[node, scripts/v.js]",
		"[python3, -mfoo]", "[python3, script]", "[python3, -c, print]", "[sh, -c, echo]", "[node, --require=x]",
		"[go, run, x]", "[make, version, x]", "[env, make]", "[make, version]", "[npm, version]", "[ninja, -v]",
		"[rustc, -V]", "[node, version]", "[java, version]",
		"[make, \"\"]", "[python3, \"\"]", "[go, \"\"]", "[go, env, GOVERSION]", "[\"go version\", -v]", "[\"\", -v]",
	} {
		if _, err := Parse([]byte(cfg(bad))); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestToolchainFileSafety(t *testing.T) {
	root := t.TempDir()
	os.Symlink("/dev/zero", filepath.Join(root, "zero"))
	os.WriteFile(filepath.Join(root, "real"), []byte("x"), 0o644)
	os.Symlink("real", filepath.Join(root, "link"))
	syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644)
	for _, f := range []string{"zero", "link", "fifo"} {
		tk := mustParse(t, "schema_version: 1\ntasks:\n  t: {argv: [make], platform: any, toolchain: [{file: "+f+"}]}\n").Tasks["t"]
		if _, err := tk.EnvFingerprint(root); err == nil {
			t.Errorf("toolchain file %s (symlink) was read", f)
		}
	}
}
