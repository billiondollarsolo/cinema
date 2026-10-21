package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/billiondollarsolo/cinema/internal/evidence"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cinema-e2e")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "cinema")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t          *testing.T
	repo, data string
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, repo: t.TempDir(), data: t.TempDir()}
	e.git("init", "-q", "-b", "main")
	e.write("main.go", "package main\n")
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  check:
    argv: [sh, -c, "test -f main.go"]
    platform: any
    toolchain: [{cmd: [git, --version]}]
    reuse: { allowed: true, max_age: 1h }
  other-os:
    argv: [make]
    platform: plan9/mips
    reuse: { allowed: true, max_age: 1h }
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "init")
	return e
}

func (e *env) write(rel, s string) {
	p := filepath.Join(e.repo, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) git(args ...string) {
	cmd := exec.Command("git", append([]string{"-C", e.repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		e.t.Fatalf("git %v: %s", args, out)
	}
}

func (e *env) cinema(args ...string) (string, int) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = e.repo
	cmd.Env = append(os.Environ(), "CINEMA_DATA_DIR="+e.data)
	out, _ := cmd.Output()
	return string(out), cmd.ProcessState.ExitCode()
}

func (e *env) states() map[string]string {
	out, _ := e.cinema("status", "--json")
	var v struct {
		Data struct {
			Tasks []struct{ ID, State, Reason_code string }
		}
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		e.t.Fatalf("status --json: %v\n%s", err, out)
	}
	m := map[string]string{}
	for _, t := range v.Data.Tasks {
		m[t.ID] = t.State + "/" + t.Reason_code
	}
	return m
}

func TestExecExitCodes(t *testing.T) { // ACC-001, ACC-002, ACC-006
	e := newEnv(t)
	for _, tc := range []struct {
		argv []string
		want int
	}{
		{[]string{"exec", "--", "sh", "-c", "exit 0"}, 0},
		{[]string{"exec", "--", "sh", "-c", "exit 42"}, 42},
		{[]string{"exec", "--", "no-such-command-xyz"}, 127},
		{[]string{"exec"}, 2},
	} {
		if _, code := e.cinema(tc.argv...); code != tc.want {
			t.Errorf("%v: exit %d, want %d", tc.argv, code, tc.want)
		}
	}
	e.data = "/nonexistent/cinema-data" // unwritable store: the child's code still wins
	if _, code := e.cinema("exec", "--", "sh", "-c", "exit 5"); code != 5 {
		t.Errorf("unwritable store: exit %d, want 5", code)
	}
}

func TestStatusLifecycle(t *testing.T) {
	e := newEnv(t)
	check := func(want string) {
		t.Helper()
		if got := e.states()["check"]; got != want {
			t.Fatalf("check = %s, want %s", got, want)
		}
	}
	check("unknown/CONFIG_UNTRUSTED") // toolchain cmds don't run until trusted
	if _, code := e.cinema("trust"); code != 0 {
		t.Fatal("trust failed")
	}
	check("missing/NO_RECEIPT")
	e.cinema("exec", "--", "sh", "-c", "test -f main.go")
	check("reusable/")
	if _, code := e.cinema("status"); code != 0 {
		t.Fatal("status should exit 0: other-os can't be satisfied here and doesn't count")
	}

	e.write("main.go", "package main\n// edited\n")
	check("stale/INPUTS_CHANGED")
	if _, code := e.cinema("status"); code != 1 {
		t.Fatal("status should exit 1 with a stale local task")
	}
	e.write("main.go", "package main\n")
	check("reusable/")

	// A narrowed or different command is not the task (ACC-018).
	e.write("main.go", "package main\n// v2\n")
	e.cinema("exec", "--", "sh", "-c", "test -f main.go && true")
	check("stale/INPUTS_CHANGED")

	// A failure on these exact inputs blocks reuse of the earlier pass.
	e.write("main.go", "package main\n")
	os.Rename(filepath.Join(e.repo, "main.go"), filepath.Join(e.repo, "gone.go"))
	e.cinema("exec", "--", "sh", "-c", "test -f main.go")
	check("failed/RUN_FAILED")
	os.Rename(filepath.Join(e.repo, "gone.go"), filepath.Join(e.repo, "main.go"))
	check("reusable/") // original tree: the earlier pass still qualifies

	// Editing the config: untrusted until re-trusted; policy edits don't invalidate (ADR 0001).
	e.write(".cinema/tasks.yaml", mustRead(t, filepath.Join(e.repo, ".cinema/tasks.yaml"))+"\n# comment\n")
	check("unknown/CONFIG_UNTRUSTED")
	e.cinema("trust")
	check("reusable/")
}

func TestMutatingRunNeverCertifies(t *testing.T) { // ACC-021
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  fmt:
    argv: [sh, -c, "echo formatted >> main.go"]
    platform: any
    reuse: { allowed: true, max_age: 1h }
`)
	e.cinema("trust")
	e.cinema("exec", "--", "sh", "-c", "echo formatted >> main.go")
	if got := e.states()["fmt"]; got != "stale/SOURCE_CHANGED_DURING_RUN" {
		t.Fatalf("fmt = %s", got)
	}
}

func mustRead(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRun(t *testing.T) {
	e := newEnv(t)
	e.cinema("trust")
	out, code := e.cinema("run", "--all")
	if code != 0 || !strings.Contains(out, "✓ check") {
		t.Fatalf("run --all: exit %d\n%s", code, out)
	}
	if got := e.states()["check"]; got != "reusable/" {
		t.Fatalf("after run: %s", got)
	}
	out, _ = e.cinema("run", "--all")
	if !strings.Contains(out, "- check  already reusable") || !strings.Contains(out, "nothing to run") {
		t.Fatalf("second run --all should skip:\n%s", out)
	}
	if _, code := e.cinema("run", "nope"); code != 2 {
		t.Fatal("unknown task accepted")
	}
	e.write("main.go", "package main\n// broken\n")
	os.Remove(filepath.Join(e.repo, "main.go"))
	if out, code := e.cinema("run", "check"); code != 1 || !strings.Contains(out, "✗ check") {
		t.Fatalf("failing task: exit %d\n%s", code, out)
	}
}

func TestRunCleanCheckout(t *testing.T) { // §5.2, CLEAN-002
	e := newEnv(t)
	e.cinema("trust")
	before := gitState(t, e.repo)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	// A stale dir from a dead cinema (PID 999999999) gets swept; a live one doesn't.
	stale := filepath.Join(tmp, "cinema-run-999999999-x")
	live := filepath.Join(tmp, "cinema-run-"+strconv.Itoa(os.Getpid())+"-y")
	for _, d := range []string{stale, live} {
		os.MkdirAll(d, 0o755)
		old := time.Now().Add(-time.Hour)
		os.Chtimes(d, old, old)
	}

	// Dirty working tree: the clean run is evidence for HEAD's bytes, not these.
	e.write("main.go", "package main\n// wip\n")
	cmd := exec.Command(bin, "run", "--commit", "HEAD", "check")
	cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data, "TMPDIR="+tmp)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "✓ check") {
		t.Fatalf("run --commit: %v\n%s", err, out)
	}
	if got := e.states()["check"]; got != "stale/INPUTS_CHANGED" {
		t.Fatalf("dirty tree vs clean evidence: %s", got)
	}
	e.write("main.go", "package main\n") // back to HEAD's bytes
	if got := e.states()["check"]; got != "reusable/" {
		t.Fatalf("clean evidence should cover an identical working tree: %s", got)
	}

	if got := gitState(t, e.repo); got != before {
		t.Fatalf("clean checkout modified the real repo:\nbefore %s\nafter  %s", before, got)
	}
	entries, _ := os.ReadDir(tmp)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), "cinema-run-") && filepath.Join(tmp, en.Name()) != live {
			t.Errorf("left behind: %s", en.Name())
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("swept a dir whose owner is alive")
	}
	if _, code := e.cinema("run", "--commit", "no-such-rev", "check"); code != 2 {
		t.Error("unknown rev accepted")
	}
}

// gitState summarizes what a clean checkout must not touch.
func gitState(t *testing.T, repo string) string {
	var parts []string
	for _, args := range [][]string{{"worktree", "list", "--porcelain"}, {"for-each-ref"}, {"status", "--porcelain"}, {"stash", "list"}} {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, string(out))
	}
	return strings.Join(parts, "|")
}

func TestInit(t *testing.T) {
	e := newEnv(t)
	os.Remove(filepath.Join(e.repo, ".cinema/tasks.yaml"))
	e.write("go.mod", "module x\n")
	e.write("web/package.json", `{"scripts": {"lint": "eslint .", "dev": "vite", "test": "vitest"}}`)
	e.write("web/pnpm-lock.yaml", "")
	if out, code := e.cinema("init"); code != 0 || !strings.Contains(out, "4 task(s)") {
		t.Fatalf("init: exit %d\n%s", code, out)
	}
	cfg := mustRead(t, filepath.Join(e.repo, ".cinema/tasks.yaml"))
	for _, want := range []string{"go-vet:", "go-test:", "web-lint:", "web-test:", "argv: [pnpm, run, \"lint\"]", "cwd: web", "file: web/pnpm-lock.yaml"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("missing %q in:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "platform: any") || strings.Contains(cfg, "web-dev") {
		t.Errorf("init inferred platform any or added a dev server:\n%s", cfg)
	}
	if _, code := e.cinema("init"); code != 2 {
		t.Error("init overwrote an existing config")
	}
}

func TestInitRefusesSymlinks(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	os.RemoveAll(filepath.Join(e.repo, ".cinema"))
	os.Symlink(outside, filepath.Join(e.repo, ".cinema"))
	if _, code := e.cinema("init"); code == 0 {
		t.Fatal("init wrote through a symlinked .cinema")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("file created outside the repo")
	}
}

func TestToolCacheReplayRejected(t *testing.T) { // §4.5: a wrapper hides `go test` from the config check
	e := newEnv(t)
	// Prints exactly what go test prints for a replayed package. (Making go's own
	// cache replay deterministically inside a test is environment-dependent; the
	// detector's parsing is unit-tested in package task.)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  unit:
    argv: [sh, -c, "printf 'ok  \\tgithub.com/x/y\\t(cached)\\n'"]
    platform: any
    reuse: { allowed: true, max_age: 1h }
`)
	e.cinema("trust")
	e.cinema("run", "unit")
	if got := e.states()["unit"]; got != "stale/TOOL_CACHE_REJECTED" {
		t.Fatalf("replayed run: %s", got)
	}
	if o := e.lastRunOrigin(t); o != "tool_cache" {
		t.Fatalf("origin = %q", o)
	}
}

func TestDeclaredOutputsKeepTaskReusable(t *testing.T) {
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  cover:
    argv: [sh, -c, "date > cover-profile.out"]
    platform: any
    outputs: [cover-profile.out]
    reuse: { allowed: true, max_age: 1h }
  undeclared:
    argv: [sh, -c, "date > other.out"]
    platform: any
    reuse: { allowed: true, max_age: 1h }
`)
	e.cinema("trust")
	e.cinema("run", "cover")
	if got := e.states()["cover"]; got != "reusable/" {
		t.Fatalf("declared output: %s", got)
	}
	e.cinema("run", "undeclared")
	if got := e.states()["undeclared"]; got != "stale/SOURCE_CHANGED_DURING_RUN" {
		t.Fatalf("undeclared output: %s", got)
	}
}

// lastRunOrigin reads the newest run's result_origin via run --record-json-free path: the store.
func (e *env) lastRunOrigin(t *testing.T) string {
	t.Helper()
	t.Setenv("CINEMA_DATA_DIR", e.data)
	r, err := openRepoAt(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	runs, err := r.st.TaskRuns(r.id, "unit", 1)
	if err != nil || len(runs) == 0 {
		t.Fatalf("runs: %v", err)
	}
	return runs[0].Origin
}

func TestPushSignsAndPublishes(t *testing.T) { // §8.1
	e := newEnv(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	e.git("remote", "add", "origin", "https://github.com/Example/Repo.git")
	e.git("config", "remote.origin.pushurl", bare)
	e.cinema("trust")
	if out, code := e.cinema("keys", "init"); code != 0 || !strings.HasPrefix(out, "cinema-ed25519 ") {
		t.Fatalf("keys init: %d %s", code, out)
	}
	if _, code := e.cinema("keys", "init"); code == 0 {
		t.Fatal("keys init replaced a key")
	}
	// A working-tree run is local feedback only: push signs nothing, says why (ADR 0002).
	e.cinema("run", "check")
	out, code := e.cinema("push", "--dry-run")
	if code != 0 || !strings.Contains(out, "working-tree runs only") || !strings.Contains(out, "would sign 0") {
		t.Fatalf("workspace-only dry run: %d\n%s", code, out)
	}
	e.cinema("run", "--commit", "HEAD", "check") // clean checkout: CI evidence

	out, code = e.cinema("push", "--dry-run")
	if code != 0 || !strings.Contains(out, "would sign 1") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
	out, code = e.cinema("push")
	if code != 0 || !strings.Contains(out, "evidence: 1 receipt") || !strings.Contains(out, "branch:   pushed main") {
		t.Fatalf("push: %d\n%s", code, out)
	}
	// The remote has our notes ref with a receipt that verifies against our key.
	refs, _ := exec.Command("git", "-C", bare, "for-each-ref", "--format=%(refname)", "refs/notes/cinema/").Output()
	if !strings.HasPrefix(string(refs), "refs/notes/cinema/v1/") {
		t.Fatalf("remote refs: %q", refs)
	}
	ref := strings.TrimSpace(string(refs))
	note, err := exec.Command("git", "-C", bare, "notes", "--ref="+ref, "show", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	var envs []json.RawMessage
	if err := json.Unmarshal(note, &envs); err != nil || len(envs) != 1 {
		t.Fatalf("note: %v %s", err, note)
	}
	pubOut, _ := e.cinema("keys", "export")
	keys, err := evidence.ParseTrustedKeys(pubOut)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := evidence.Verify(envs[0], keys)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vs.Predicate.RepositoryID != "github.com/example/repo" || vs.Predicate.Task.ID != "check" {
		t.Fatalf("predicate: %+v", vs.Predicate)
	}

	// A dirty tree doesn't match the commit: nothing new to sign.
	e.write("main.go", "package main // wip\n")
	e.cinema("run", "check")
	out, _ = e.cinema("push", "--notes-only")
	if !strings.Contains(out, "evidence: 1 receipt") { // HEAD still has its receipt from the clean run
		t.Fatalf("notes-only: %s", out)
	}
}

func TestRepositoryID(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/Owner/Repo.git": "github.com/owner/repo",
		"git@github.com:Owner/Repo.git":     "github.com/owner/repo",
		"ssh://git@github.com/owner/repo":   "github.com/owner/repo",
		"https://gitlab.com/group/sub/proj": "gitlab.com/group/sub/proj",
	} {
		if got, err := repositoryID(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/local/path", "https://github.com/only"} {
		if _, err := repositoryID(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPushRunMakesCleanEvidence(t *testing.T) { // ADR 0002: push --run
	e := newEnv(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	exec.Command("git", "init", "-q", "--bare", bare).Run()
	e.git("remote", "add", "origin", "https://github.com/Example/Repo.git")
	e.git("config", "remote.origin.pushurl", bare)
	e.cinema("trust")
	e.cinema("keys", "init")
	out, code := e.cinema("push", "--run")
	if code != 0 || !strings.Contains(out, "✓ check") || !strings.Contains(out, "evidence: 1 receipt") {
		t.Fatalf("push --run: %d\n%s", code, out)
	}
	// Nothing new to sign next time, and pushing works without errors.
	out, code = e.cinema("push", "--notes-only")
	if code != 0 {
		t.Fatalf("second push: %d\n%s", code, out)
	}
}

func TestCleanCheckoutRestoredBetweenTasks(t *testing.T) {
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  a-mutates:
    argv: [sh, -c, "rm main.go && echo x > stray.txt"]
    platform: any
    reuse: { allowed: true, max_age: 1h }
  b-reads:
    argv: [sh, -c, "test -f main.go && test ! -e stray.txt"]
    platform: any
    reuse: { allowed: true, max_age: 1h }
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	out, code := e.cinema("run", "--commit", "HEAD", "a-mutates", "b-reads")
	if code != 0 || !strings.Contains(out, "✓ b-reads") {
		t.Fatalf("b saw a's changes: %d\n%s", code, out)
	}
}

func TestRunThatRewritesConfigIsNotEvidence(t *testing.T) { // lab: repo-wide formatter
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  fmt-all:
    argv: [sh, -c, "printf '# reformatted\\n' >> .cinema/tasks.yaml"]
    platform: any
    reuse: { allowed: true, max_age: 1h }
`)
	e.cinema("trust")
	out, _ := e.cinema("run", "fmt-all")
	if !strings.Contains(out, "! fmt-all") {
		t.Fatalf("config-rewriting run looked like evidence:\n%s", out)
	}
}

func TestRunParallel(t *testing.T) {
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  a: {argv: [sh, -c, "sleep 1; rm main.go"], platform: any, reuse: {allowed: true, max_age: 1h}}
  b: {argv: [sh, -c, "sleep 1; test -f main.go"], platform: any, reuse: {allowed: true, max_age: 1h}}
  c: {argv: [sh, -c, "sleep 1; test -f main.go"], platform: any, reuse: {allowed: true, max_age: 1h}}
  d: {argv: [sh, -c, "echo boom; exit 4"], platform: any, reuse: {allowed: true, max_age: 1h}}
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	if _, code := e.cinema("run", "-j", "3", "--all"); code != exitBadArgs {
		t.Fatal("-j without --commit accepted")
	}
	start := time.Now()
	out, code := e.cinema("run", "--commit", "HEAD", "-j", "4", "--all")
	if code != 1 || !strings.Contains(out, "✓ b") || !strings.Contains(out, "✓ c") || !strings.Contains(out, "✗ d") || !strings.Contains(out, "    boom") {
		t.Fatalf("parallel run: %d\n%s", code, out)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("not parallel: %v", d)
	}
}

// A reused clean-checkout slot must not carry ignored files from an earlier run
// into the next one: that's the stale node_modules / dist/ false green (ADR 0002).
func TestCleanCheckoutSlotResetBetweenRuns(t *testing.T) {
	e := newEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	e.write(".gitignore", "build/\n")
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  make-artifact: {argv: [sh, -c, "mkdir -p build && echo stale > build/out"], platform: any, reuse: {allowed: true, max_age: 1h}}
  needs-fresh: {argv: [sh, -c, "test ! -e build/out"], platform: any, reuse: {allowed: true, max_age: 1h}}
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	run := func(args ...string) string {
		cmd := exec.Command(bin, append([]string{"run", "--commit", "HEAD"}, args...)...)
		cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	run("make-artifact")
	if out := run("needs-fresh"); !strings.Contains(out, "✓ needs-fresh") {
		t.Fatalf("ignored file from the previous run leaked into the slot:\n%s", out)
	}
	slots, _ := filepath.Glob(filepath.Join(e.data, "checkouts", "*", "0"))
	if len(slots) != 1 {
		t.Fatalf("expected one reused slot, got %v", slots)
	}
}

// A slot holding files cinema can't delete (root-owned output of a container
// task; simulated with a read-only dir) must fail with the cause and the fix.
func TestCleanCheckoutUndeletableSlot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can delete anything")
	}
	e := newEnv(t)
	e.write(".gitignore", "out/\n")
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  stuck: {argv: [sh, -c, "mkdir -p out/d && touch out/d/f && chmod 555 out/d"], platform: any, reuse: {allowed: true, max_age: 1h}}
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	run := func() string {
		cmd := exec.Command(bin, "run", "--commit", "HEAD", "stuck")
		cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	run()
	slots, _ := filepath.Glob(filepath.Join(e.data, "checkouts", "*", "0"))
	t.Cleanup(func() { exec.Command("chmod", "-R", "u+w", e.data).Run() })
	if out := run(); len(slots) != 1 || !strings.Contains(out, "sudo rm -rf "+slots[0]) {
		t.Fatalf("no actionable error for an undeletable slot:\n%s", out)
	}
}

// Stopping cinema (Ctrl-C, SIGTERM) mid-task is not a test failure: the run is
// recorded as interrupted and must not block reuse of a later pass (lab:
// killing a hung push marked 4 tasks failed on those exact inputs).
func TestInterruptedRunIsNotAFailure(t *testing.T) {
	e := newEnv(t)
	started := filepath.Join(t.TempDir(), "started")
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  slow: {argv: [sh, -c, "touch `+started+`; sleep 30"], platform: any, reuse: {allowed: true, max_age: 1h}}
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	cmd := exec.Command(bin, "run", "slow")
	cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err == nil {
		t.Fatal("interrupted run exited 0")
	}
	if st := e.states()["slow"]; !strings.HasPrefix(st, "missing") {
		t.Fatalf("interrupted run: state %q, want missing (neither failed nor reusable)", st)
	}
}

// Tasks may share an argv (one per runtime version: eslint's mocha-20..26).
// `cinema run X` must record the run as X, never as a look-alike chosen by
// argv (lab: a Node 20 run filed under mocha-26 with mocha-26's environment).
func TestRunRecordsTheTaskItRan(t *testing.T) {
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  a: {argv: [sh, -c, "true"], platform: any, reuse: {allowed: true, max_age: 1h}}
  b: {argv: [sh, -c, "true"], platform: any, reuse: {allowed: true, max_age: 1h}}
  c: {argv: [sh, -c, "true"], platform: any, reuse: {allowed: true, max_age: 1h}}
  d: {argv: [sh, -c, "true"], platform: any, reuse: {allowed: true, max_age: 1h}}
  e: {argv: [sh, -c, "true"], platform: any, reuse: {allowed: true, max_age: 1h}}
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	for i := 0; i < 3; i++ {
		e.cinema("run", "--force", "c")
	}
	for id, st := range e.states() {
		if want := id == "c"; strings.HasPrefix(st, "reusable") != want {
			t.Errorf("%s: %s", id, st)
		}
	}
	// A bare exec of an ambiguous argv can't say which task it was: history only.
	e.cinema("exec", "--", "sh", "-c", "true")
	for id, st := range e.states() {
		if want := id == "c"; strings.HasPrefix(st, "reusable") != want {
			t.Errorf("after exec, %s: %s", id, st)
		}
	}
}

func TestCleanRunTempFilesRemoved(t *testing.T) {
	e := newEnv(t)
	tmp := t.TempDir()
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  litter: {argv: [sh, -c, "mkdir \"$TMPDIR/go-build-junk\" && echo $TMPDIR > \"$TMPDIR/../where\""], platform: any, reuse: {allowed: true, max_age: 1h}}
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	cmd := exec.Command(bin, "run", "--commit", "HEAD", "litter")
	cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data, "TMPDIR="+tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	where, err := os.ReadFile(filepath.Join(tmp, "where"))
	if err != nil {
		t.Fatal("task didn't get a private TMPDIR under the real one")
	}
	if _, err := os.Stat(strings.TrimSpace(string(where))); !os.IsNotExist(err) {
		t.Fatalf("task TMPDIR %s not removed", where)
	}
}

func TestRunParallelExclusive(t *testing.T) {
	e := newEnv(t)
	lock := filepath.Join(t.TempDir(), "port")
	// Each task takes a "port" (a lock dir); two holders at once means a collision.
	take := func(id string) string {
		return id + `: {argv: [sh, -c, "mkdir ` + lock + ` || exit 7; sleep 1; rmdir ` + lock + `"], platform: any, exclusive: true, reuse: {allowed: true, max_age: 1h}}`
	}
	e.write(".cinema/tasks.yaml", "schema_version: 1\ntasks:\n  "+take("x")+"\n  "+take("y")+"\n  "+take("z")+"\n")
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	e.cinema("trust")
	if out, code := e.cinema("run", "--commit", "HEAD", "-j", "3", "--all"); code != 0 {
		t.Fatalf("exclusive tasks collided under -j: %d\n%s", code, out)
	}
}

func TestExecOfRuntimePinnedTaskIsHistoryOnly(t *testing.T) {
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  pinned: {argv: [sh, -c, "true"], platform: any, runtime: {go: 1.26.10}, reuse: {allowed: true, max_age: 1h}}
`)
	e.cinema("trust")
	cmd := exec.Command(bin, "exec", "--", "sh", "-c", "true")
	cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "pins a runtime") {
		t.Fatalf("no hint:\n%s", out)
	}
	if got := e.states()["pinned"]; got != "missing/NO_RECEIPT" {
		t.Fatalf("exec of a runtime-pinned task counted: %s", got)
	}
}

func TestLog(t *testing.T) {
	e := newEnv(t)
	e.write(".cinema/tasks.yaml", `schema_version: 1
tasks:
  noisy: {argv: [sh, -c, "echo hello-out; echo hello-err >&2; exit ${CODE:-0}"], platform: any, reuse: {allowed: true, max_age: 1h}}
`)
	e.cinema("trust")
	e.cinema("run", "noisy")
	cmd := exec.Command(bin, "run", "noisy")
	cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data, "CODE=3")
	cmd.Run()
	if out, code := e.cinema("log", "noisy"); code != 0 || !strings.Contains(out, "hello-out") {
		t.Fatalf("log: %d %q", code, out)
	}
	cmd = exec.Command(bin, "log", "--failed", "noisy")
	cmd.Dir, cmd.Env = e.repo, append(os.Environ(), "CINEMA_DATA_DIR="+e.data)
	both, _ := cmd.CombinedOutput()
	if !strings.Contains(string(both), "failed, exit 3") || !strings.Contains(string(both), "hello-err") {
		t.Fatalf("log --failed:\n%s", both)
	}
	if _, code := e.cinema("log", "nope"); code != exitBadArgs {
		t.Fatal("unknown task accepted")
	}
}
