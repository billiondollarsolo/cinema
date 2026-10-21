package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
    toolchain: [{cmd: [sh, -c, "echo tool 1"]}]
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
