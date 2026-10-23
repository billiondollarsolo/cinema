package main

import (
	"encoding/json"
	"github.com/billiondollarsolo/cinema/internal/task"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateEnv: a dev repo that pushes code + receipts to a bare remote, and a
// fresh "CI" clone that runs the gate, like actions/checkout + cinema verify.
type gateEnv struct {
	*env
	bare, ci, keys string
}

const gateTasks = `schema_version: 1
tasks:
  check:
    argv: [sh, -c, "test -f main.go"]
    platform: any
    toolchain: [{cmd: [git, --version]}]
    reuse: { allowed: true, max_age: 1h }
  never-run-locally:
    argv: [sh, -c, "exit 0"]
    platform: any
    reuse: { allowed: true, max_age: 1h }
  always:
    argv: [sh, -c, "exit 0"]
    platform: any
    reuse: { allowed: false }
`

func newGateEnv(t *testing.T) *gateEnv {
	g := &gateEnv{env: newEnv(t)}
	g.write(".cinema/tasks.yaml", gateTasks)
	g.git("add", "-A")
	g.git("commit", "-qm", "tasks")
	g.bare = filepath.Join(t.TempDir(), "remote.git")
	mustRun(t, "", "git", "init", "-q", "--bare", g.bare)
	g.git("remote", "add", "origin", "https://github.com/Example/Repo.git")
	g.git("config", "remote.origin.pushurl", g.bare)
	g.cinema("trust")
	g.keys, _ = g.cinema("keys", "init")
	return g
}

func mustRun(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gate clones the remote at main and runs cinema verify like a push-event CI job.
func (g *gateEnv) gate(t *testing.T, keys string, event map[string]any) (map[string]string, string, int) {
	t.Helper()
	ci := filepath.Join(t.TempDir(), "ci")
	mustRun(t, "", "git", "clone", "-q", g.bare, ci)
	if event == nil {
		event = map[string]any{"repository": map[string]any{"full_name": "Example/Repo"}}
	}
	b, _ := json.Marshal(event)
	evPath := filepath.Join(t.TempDir(), "event.json")
	os.WriteFile(evPath, b, 0o644)
	report := filepath.Join(t.TempDir(), "gate-report.json")
	cmd := exec.Command(bin, "verify", "--event", evPath, "--execute", "--report", report)
	cmd.Dir = ci
	cmd.Env = append(os.Environ(), "GITHUB_REPOSITORY=Example/Repo", "GITHUB_SERVER_URL=https://github.com",
		"CINEMA_TRUSTED_KEYS="+keys, "CINEMA_DATA_DIR="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	var rep gateReport
	rb, _ := os.ReadFile(report)
	json.Unmarshal(rb, &rep)
	got := map[string]string{}
	for _, tr := range rep.Tasks {
		got[tr.Task] = tr.Decision + "/" + tr.Reason
	}
	return got, string(out), cmd.ProcessState.ExitCode()
}

func TestGateEndToEnd(t *testing.T) { // ACC-035, 031, 024, 028
	g := newGateEnv(t)
	g.cinema("run", "--commit", "HEAD", "check")

	// ACC-031: notes not pushed yet → everything runs, gate still works.
	g.git("push", "-q", g.bare, "HEAD:main")
	got, out, code := g.gate(t, g.keys, nil)
	if got["check"] != "RUN/NO_RECEIPT" || code != 0 {
		t.Fatalf("no notes: %v exit %d\n%s", got, code, out)
	}

	// ACC-035: receipts pushed → check reused (verification, not skipping), others run.
	if out, code := g.cinema("push"); code != 0 {
		t.Fatalf("push: %s", out)
	}
	got, out, code = g.gate(t, g.keys, nil)
	if got["check"] != "REUSE/" || got["never-run-locally"] != "RUN/NO_RECEIPT" || got["always"] != "RUN/REUSE_DISABLED" || code != 0 {
		t.Fatalf("with receipts: %v exit %d\n%s", got, code, out)
	}

	// ACC-024 / ACC-028: key not trusted (or removed from the list) → RUN.
	other := newGateEnv(t)
	got, _, _ = g.gate(t, other.keys, nil)
	if got["check"] != "RUN/SIGNER_NOT_ALLOWED" {
		t.Fatalf("untrusted key: %v", got)
	}

	// Keys missing entirely → every task runs (safe fallback, not a block).
	got, _, code = g.gate(t, "", nil)
	if got["check"] != "RUN/POLICY_UNAVAILABLE" || code != 0 {
		t.Fatalf("no keys: %v %d", got, code)
	}
}

func TestGateFailsWhenInlineRunFails(t *testing.T) { // ACC-037
	g := newGateEnv(t)
	g.write(".cinema/tasks.yaml", strings.Replace(gateTasks, `argv: [sh, -c, "exit 0"]
    platform: any
    reuse: { allowed: false }`, `argv: [sh, -c, "exit 3"]
    platform: any
    reuse: { allowed: false }`, 1))
	g.git("commit", "-qam", "failing task")
	g.cinema("trust")
	g.git("push", "-q", g.bare, "HEAD:main")
	got, out, code := g.gate(t, g.keys, nil)
	if code == 0 || got["always"] != "RUN/REUSE_DISABLED" {
		t.Fatalf("failing inline task passed the gate: %v %d\n%s", got, code, out)
	}
}

func TestGateWrongRepoAndForks(t *testing.T) { // ACC-025, ACC-041
	g := newGateEnv(t)
	g.cinema("run", "--commit", "HEAD", "check")
	g.cinema("push")
	// Receipts made for Example/Repo presented to Other/Repo.
	ci := filepath.Join(t.TempDir(), "ci")
	mustRun(t, "", "git", "clone", "-q", g.bare, ci)
	ev := filepath.Join(t.TempDir(), "e.json")
	os.WriteFile(ev, []byte(`{"repository":{"full_name":"Other/Repo"}}`), 0o644)
	cmd := exec.Command(bin, "verify", "--event", ev)
	cmd.Dir = ci
	cmd.Env = append(os.Environ(), "GITHUB_REPOSITORY=Other/Repo", "CINEMA_TRUSTED_KEYS="+g.keys, "CINEMA_DATA_DIR="+t.TempDir())
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "RUN    check  (WRONG_REPOSITORY)") {
		t.Fatalf("wrong repo:\n%s", out)
	}

	// Fork PR: everything runs, whatever the notes say.
	head := g.headSHA(t)
	got, _, _ := g.gate(t, g.keys, map[string]any{
		"repository":   map[string]any{"full_name": "Example/Repo"},
		"pull_request": map[string]any{"base": map[string]any{"sha": head}, "head": map[string]any{"sha": head, "repo": map[string]any{"full_name": "attacker/Repo"}}},
	})
	if got["check"] != "RUN/FORK_PR" {
		t.Fatalf("fork: %v", got)
	}
}

func TestGateTasksComeFromBase(t *testing.T) { // ACC-041: a PR can't drop or weaken tasks
	g := newGateEnv(t)
	g.git("push", "-q", g.bare, "HEAD:main")
	base := g.headSHA(t)
	// The PR deletes every task but one trivial one.
	g.write(".cinema/tasks.yaml", "schema_version: 1\ntasks:\n  trivial: {argv: [\"true\"], platform: any}\n")
	g.git("commit", "-qam", "loosen")
	g.git("push", "-q", g.bare, "HEAD:main")
	got, _, _ := g.gate(t, "", map[string]any{
		"repository":   map[string]any{"full_name": "Example/Repo"},
		"pull_request": map[string]any{"base": map[string]any{"sha": base}, "head": map[string]any{"sha": g.headSHA(t), "repo": map[string]any{"full_name": "Example/Repo"}}},
	})
	if _, ok := got["check"]; !ok || got["trivial"] != "" {
		t.Fatalf("tasks not taken from base: %v", got)
	}
}

func (g *gateEnv) headSHA(t *testing.T) string {
	return mustRun(t, g.repo, "git", "rev-parse", "HEAD")
}

func TestGateBaseConfigFailsClosed(t *testing.T) {
	g := newGateEnv(t)
	g.git("push", "-q", g.bare, "HEAD:main")
	head := g.headSHA(t)
	pr := func(base string) map[string]any {
		return map[string]any{
			"repository":   map[string]any{"full_name": "Example/Repo"},
			"pull_request": map[string]any{"base": map[string]any{"sha": base}, "head": map[string]any{"sha": head, "repo": map[string]any{"full_name": "Example/Repo"}}},
		}
	}
	// Base commit not in the checkout (shallow clone, unknown SHA): fail, don't use the PR's config.
	if _, out, code := g.gate(t, g.keys, pr(strings.Repeat("a", 40))); code == 0 || !strings.Contains(out, "not in the checkout") {
		t.Fatalf("missing base passed: %d\n%s", code, out)
	}
	// Base exists but has no config: a PR can't bootstrap its own required tasks.
	g.git("rm", "-q", ".cinema/tasks.yaml")
	g.git("commit", "-qm", "no config")
	noCfg := g.headSHA(t)
	g.git("revert", "--no-edit", "HEAD")
	head = g.headSHA(t)
	g.git("push", "-q", g.bare, "HEAD:main")
	if _, out, code := g.gate(t, g.keys, pr(noCfg)); code == 0 || !strings.Contains(out, "base branch has no") {
		t.Fatalf("PR defined its own tasks: %d\n%s", code, out)
	}
}

func TestGateRejectsWorkspaceEvidence(t *testing.T) { // ADR 0002
	g := newGateEnv(t)
	// Opt the task into workspace evidence to get a working-tree receipt signed...
	g.write(".cinema/tasks.yaml", strings.Replace(gateTasks, "reuse: { allowed: true, max_age: 1h }\n  never", "reuse: { allowed: true, max_age: 1h, capture: workspace }\n  never", 1))
	g.cinema("trust")
	g.cinema("run", "check")
	g.git("commit", "-qam", "workspace opt-in (local only)")
	g.cinema("trust")
	g.cinema("run", "check")
	// One run covers every unpushed commit with the same tree (they differ only in
	// tasks.yaml, which ADR 0001 leaves out of fingerprints).
	if out, code := g.cinema("push"); !strings.Contains(out, " receipt(s) on ") {
		t.Fatalf("push: exit %d\n%q", code, out)
	}
	// ...but the base branch still says clean: CI rejects it.
	got, _, _ := g.gate(t, g.keys, map[string]any{
		"repository": map[string]any{"full_name": "Example/Repo"},
		"pull_request": map[string]any{"base": map[string]any{"sha": mustRun(t, g.repo, "git", "rev-parse", "HEAD~1")},
			"head": map[string]any{"sha": g.headSHA(t), "repo": map[string]any{"full_name": "Example/Repo"}}},
	})
	if got["check"] != "RUN/WORKSPACE_EVIDENCE" {
		t.Fatalf("workspace receipt under clean policy: %v", got)
	}
}

func TestGateParallelModes(t *testing.T) { // plan job → matrix → aggregate
	g := newGateEnv(t)
	g.cinema("run", "--commit", "HEAD", "check")
	g.cinema("push")
	ci := filepath.Join(t.TempDir(), "ci")
	mustRun(t, "", "git", "clone", "-q", g.bare, ci)
	ev := filepath.Join(t.TempDir(), "e.json")
	os.WriteFile(ev, []byte(`{"repository":{"full_name":"Example/Repo"}}`), 0o644)
	out := filepath.Join(t.TempDir(), "gh-output")
	run := func(args ...string) (string, int) {
		cmd := exec.Command(bin, append([]string{"verify", "--event", ev}, args...)...)
		cmd.Dir = ci
		cmd.Env = append(os.Environ(), "GITHUB_REPOSITORY=Example/Repo", "CINEMA_TRUSTED_KEYS="+g.keys,
			"GITHUB_OUTPUT="+out, "CINEMA_DATA_DIR="+t.TempDir())
		b, _ := cmd.CombinedOutput()
		return string(b), cmd.ProcessState.ExitCode()
	}
	if _, code := run(); code != 0 {
		t.Fatal("plan job failed")
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), `run_tasks=["always","never-run-locally"]`) || !strings.Contains(string(b), "reused_count=1") {
		t.Fatalf("outputs: %s", b)
	}
	if o, code := run("--execute", "--task", "always"); code != 0 || !strings.Contains(o, "always passed") {
		t.Fatalf("matrix job: %d\n%s", code, o)
	}
	if _, code := run("--execute", "--task", "nope"); code == 0 {
		t.Fatal("unknown task accepted in matrix job")
	}
	if _, code := run("--task", "always"); code != exitBadArgs {
		t.Fatal("--task without --execute accepted")
	}
}

// A task for another platform fails the gate closed: it can't be judged here.
func TestGateOtherPlatformFailsClosed(t *testing.T) {
	other := "darwin/arm64"
	if task.Platform() == other {
		other = "linux/amd64"
	}
	g := newGateEnv(t)
	g.write(".cinema/tasks.yaml", gateTasks+"  elsewhere:\n    argv: [\"true\"]\n    platform: "+other+"\n    reuse: { allowed: true, max_age: 1h }\n")
	g.git("add", "-A")
	g.git("commit", "-qm", "other platform")
	g.cinema("trust")
	g.cinema("push")
	ci := filepath.Join(t.TempDir(), "ci")
	mustRun(t, "", "git", "clone", "-q", g.bare, ci)
	ev := filepath.Join(t.TempDir(), "e.json")
	os.WriteFile(ev, []byte(`{"repository":{"full_name":"Example/Repo"}}`), 0o644)
	for _, args := range [][]string{nil, {"--execute"}} {
		cmd := exec.Command(bin, append([]string{"verify", "--event", ev}, args...)...)
		cmd.Dir = ci
		cmd.Env = append(os.Environ(), "GITHUB_REPOSITORY=Example/Repo", "CINEMA_TRUSTED_KEYS="+g.keys,
			"GITHUB_OUTPUT="+filepath.Join(t.TempDir(), "o"), "CINEMA_DATA_DIR="+t.TempDir())
		b, _ := cmd.CombinedOutput()
		if cmd.ProcessState.ExitCode() == 0 || !strings.Contains(string(b), "linux tasks only") {
			t.Fatalf("verify %v passed with a %s task:\n%s", args, other, b)
		}
	}
}
