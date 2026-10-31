package main

import (
	"encoding/json"
	"errors"
	"github.com/billiondollarsolo/cinema/internal/task"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// Skip-in-place: a workflow's cinema job outputs skip_<job>=true only for jobs
// whose every leg (as the workflow under test expands it) was drafted into a
// task that was reused, and exclude_<job> with the reused legs otherwise.
func TestSkipOutputs(t *testing.T) {
	g := newGateEnv(t)
	wf := `on: pull_request
jobs:
  lint:
    runs-on: ubuntu-latest
    steps: [{run: echo lint}]
  unit:
    runs-on: ubuntu-latest
    steps: [{run: echo unit}]
  build:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps: [{run: echo build}]
  test:
    strategy:
      matrix:
        os: [ubuntu-latest]
        node: ["20.x", "22.x"]
    runs-on: ubuntu-latest
    steps: [{run: echo test}]
`
	g.write(".github/workflows/ci.yml", wf)
	g.git("add", "-A")
	g.git("commit", "-qm", "ci")
	draft, _ := g.cinema("analyze", "--yaml")
	// platform: any so the test runs on any machine (not part of the leg digest)
	draft = strings.ReplaceAll(draft, "platform: linux/amd64", "platform: any")
	g.write(".cinema/tasks.yaml", draft)
	g.git("add", "-A")
	g.git("commit", "-qm", "drafted tasks")
	g.cinema("trust")
	g.cinema("run", "--commit", "HEAD", "lint", "build-ubuntu-latest", "test-20.x-ubuntu-latest")
	g.cinema("push")
	var summary string
	verify := func() string {
		ci := filepath.Join(t.TempDir(), "ci")
		mustRun(t, "", "git", "clone", "-q", g.bare, ci)
		ev := filepath.Join(t.TempDir(), "e.json")
		os.WriteFile(ev, []byte(`{"repository":{"full_name":"Example/Repo"}}`), 0o644)
		out := filepath.Join(t.TempDir(), "gh-output")
		cmd := exec.Command(bin, "verify", "--event", ev, "--skip-outputs", "ci.yml")
		cmd.Dir = ci
		cmd.Env = append(os.Environ(), "GITHUB_REPOSITORY=Example/Repo", "CINEMA_TRUSTED_KEYS="+g.keys,
			"GITHUB_OUTPUT="+out, "GITHUB_STEP_SUMMARY="+out+".md", "CINEMA_DATA_DIR="+t.TempDir())
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		md, _ := os.ReadFile(out + ".md")
		summary = string(md)
		b, _ := os.ReadFile(out)
		return string(b)
	}
	// Shadow mode: reused, but nothing graduated, so nothing skips.
	if got := verify(); got != "skip_build=false\nexclude_build=[]\nskip_lint=false\nskip_test=false\nexclude_test=[]\nskip_unit=false\n" {
		t.Fatalf("ungraduated tasks must not skip:\n%s", got)
	}
	// Graduate every task for the job digest it covers (what cinema graduate writes).
	graduated := regexp.MustCompile(`(?m)^    job_digest: (sha256:[0-9a-f]+)$`).ReplaceAllString(draft,
		"    job_digest: $1\n    graduated: {job_digest: \"$1\", backtested: 5, at: 2026-10-30}")
	g.write(".cinema/tasks.yaml", graduated)
	g.git("commit", "-qam", "graduate")
	g.cinema("trust")
	g.cinema("run", "--commit", "HEAD", "lint", "build-ubuntu-latest", "test-20.x-ubuntu-latest")
	g.cinema("push")
	// build: its ubuntu leg was reused, macOS has no task: never skip whole.
	want := "skip_build=false\nexclude_build=[{\"os\":\"ubuntu-latest\"}]\nskip_lint=true\nskip_test=false\nexclude_test=[{\"node\":\"20.x\",\"os\":\"ubuntu-latest\"}]\nskip_unit=false\n"
	if got := verify(); got != want {
		t.Fatalf("outputs:\n%s\nwant:\n%s", got, want)
	}
	if !regexp.MustCompile("\\| `lint` \\| SKIP \\| lint: passed on \\S+ \\|").MatchString(summary) || !strings.Contains(summary, "| `unit` | RUN |") {
		t.Fatalf("job summary:\n%s", summary)
	}
	// A change adds a step to lint. Its task (from the base tasks.yaml) is
	// unchanged and freshly reused, but it no longer stands for the job: run it.
	g.write(".github/workflows/ci.yml", strings.Replace(wf, "steps: [{run: echo lint}]", "steps: [{run: echo lint}, {run: ./new-check.sh}]", 1))
	g.git("add", "-A")
	g.git("commit", "-qm", "lint gains a step")
	g.cinema("run", "--commit", "HEAD", "lint")
	g.cinema("push")
	if got := verify(); !strings.Contains(got, "skip_lint=false") {
		t.Fatalf("a job whose steps changed was skipped:\n%s", got)
	}
}

// A task drafted for pull_request (its job reads the event) is no evidence for
// a push: the job may do other work there (ACC-114).
func TestGateEventMismatch(t *testing.T) {
	g := newGateEnv(t)
	g.write(".cinema/tasks.yaml", strings.Replace(gateTasks, "  check:\n", "  check:\n    event: pull_request\n", 1))
	g.git("commit", "-qam", "event")
	g.cinema("trust")
	g.cinema("run", "--commit", "HEAD", "check")
	g.git("push", "-q", g.bare, "HEAD:main")
	if out, code := g.cinema("push"); code != 0 {
		t.Fatalf("push: %s", out)
	}
	got, out, _ := g.gate(t, g.keys, nil) // a push event
	if got["check"] != "RUN/EVENT_MISMATCH" {
		t.Fatalf("push event, pull_request task: %v\n%s", got, out)
	}
}

// A failure is published and blocks reuse in CI; one pass afterwards doesn't
// lift it (retrying until green), three in a row do (ACC-115).
func TestFailurePublishedAndFlakyStreak(t *testing.T) {
	g := newGateEnv(t)
	flag := filepath.Join(t.TempDir(), "pass")
	g.write(".cinema/tasks.yaml", strings.Replace(gateTasks, `argv: [sh, -c, "test -f main.go"]`, `argv: [sh, -c, "test -f `+flag+`"]`, 1))
	g.git("commit", "-qam", "flaky task")
	g.git("push", "-q", g.bare, "HEAD:main")
	g.cinema("trust")
	g.cinema("run", "--commit", "HEAD", "check") // fails: no flag yet
	if out, code := g.cinema("push"); code != 0 {
		t.Fatalf("push: %s", out)
	}
	if got, out, _ := g.gate(t, g.keys, nil); got["check"] != "RUN/FAILED_ELSEWHERE" {
		t.Fatalf("published failure must block: %v\n%s", got, out)
	}
	os.WriteFile(flag, nil, 0o644)
	g.cinema("run", "--commit", "HEAD", "check") // passes once
	g.cinema("push")
	if got, _, _ := g.gate(t, g.keys, nil); got["check"] != "RUN/FAILED_ELSEWHERE" {
		t.Fatalf("one pass after a failure must not lift it: %v", got)
	}
	if st := g.states()["check"]; st != "failed/RUN_FAILED" {
		t.Fatalf("status must agree with push after one pass: %s", st)
	}
	g.cinema("run", "--commit", "HEAD", "check")
	g.cinema("run", "--commit", "HEAD", "check")
	g.cinema("push")
	if got, out, _ := g.gate(t, g.keys, nil); got["check"] != "REUSE/" {
		t.Fatalf("three passes in a row: %v\n%s", got, out)
	}
}

// A pin resolved from a floating spec stops matching when the spec resolves to
// a newer release, or can't be resolved (ACC-112).
func TestFloatingMoved(t *testing.T) {
	old := floatingResolve
	defer func() { floatingResolve = old }()
	tk := &task.Task{Runtime: task.Runtime{Go: "1.26.10"}, Floating: map[string]string{"go": "stable"}}
	floatingResolve = func(tool, spec string) (string, error) { return "1.26.10", nil }
	if why := floatingMoved(tk); why != "" {
		t.Fatalf("unchanged: %s", why)
	}
	floatingResolve = func(tool, spec string) (string, error) { return "1.27.0", nil }
	if why := floatingMoved(tk); !strings.Contains(why, "1.27.0") {
		t.Fatalf("moved: %q", why)
	}
	floatingResolve = func(tool, spec string) (string, error) { return "", errors.New("offline") }
	if why := floatingMoved(tk); why == "" {
		t.Fatal("unresolvable must run")
	}
	// A registered runtime; the pin carries components after "+" (rust).
	rs := &task.Task{Runtime: task.Runtime{Other: map[string]string{"rust": "1.83.0+clippy"}}, Floating: map[string]string{"rust": "stable"}}
	floatingResolve = func(tool, spec string) (string, error) { return "1.83.0", nil }
	if why := floatingMoved(rs); why != "" {
		t.Fatalf("1.83.0+clippy is 1.83.0: %s", why)
	}
	floatingResolve = func(tool, spec string) (string, error) { return "1.84.0", nil }
	if why := floatingMoved(rs); why == "" {
		t.Fatal("rust moved to 1.84.0: must run")
	}
}

// The base moved after the branch: a head receipt no longer matches the merge
// commit CI tests, the merge-result receipt does (plan §5.5, ACC-105).
func TestPushMergeResult(t *testing.T) {
	for _, mergeBase := range []string{"none", "base"} {
		g := newGateEnv(t)
		g.git("push", "-q", g.bare, "HEAD:main")
		baseBranch := mustRun(t, g.repo, "git", "rev-parse", "--abbrev-ref", "HEAD")
		g.git("checkout", "-q", "-b", "feature")
		g.write("feature.txt", "f")
		g.git("add", "-A")
		g.git("commit", "-qm", "feature")
		feature := g.headSHA(t)
		g.git("checkout", "-q", baseBranch)
		g.write("other.txt", "main moved")
		g.git("add", "-A")
		g.git("commit", "-qm", "main moves")
		g.git("push", "-q", g.bare, "HEAD:main")
		base := g.headSHA(t)
		g.git("checkout", "-q", "feature")
		arg := "--merge-base=none"
		if mergeBase == "base" {
			arg = "--merge-base=" + baseBranch
		}
		if out, code := g.cinema("push", "--run", arg); code != 0 {
			t.Fatalf("push: %s", out)
		}
		// CI: GitHub's test merge of the PR into the base.
		ci := filepath.Join(t.TempDir(), "ci")
		mustRun(t, "", "git", "clone", "-q", g.bare, ci)
		mustRun(t, ci, "git", "merge", "-q", "--no-ff", "-m", "Merge feature", "origin/feature")
		ev := filepath.Join(t.TempDir(), "e.json")
		b, _ := json.Marshal(map[string]any{"repository": map[string]any{"full_name": "Example/Repo"},
			"pull_request": map[string]any{"base": map[string]any{"sha": base}, "head": map[string]any{"sha": feature, "repo": map[string]any{"full_name": "Example/Repo"}}}})
		os.WriteFile(ev, b, 0o644)
		report := filepath.Join(t.TempDir(), "r.json")
		cmd := exec.Command(bin, "verify", "--event", ev, "--execute", "--report", report)
		cmd.Dir = ci
		cmd.Env = append(os.Environ(), "GITHUB_REPOSITORY=Example/Repo", "CINEMA_TRUSTED_KEYS="+g.keys, "CINEMA_DATA_DIR="+t.TempDir())
		out, _ := cmd.CombinedOutput()
		var rep gateReport
		rb, _ := os.ReadFile(report)
		json.Unmarshal(rb, &rep)
		got := ""
		for _, tr := range rep.Tasks {
			if tr.Task == "check" {
				got = tr.Decision + "/" + tr.Reason
			}
		}
		want := map[string]string{"none": "RUN/INPUTS_CHANGED", "base": "REUSE/"}[mergeBase]
		if got != want {
			t.Fatalf("merge-base %s: check %s, want %s\n%s", mergeBase, got, want, out)
		}
	}
}
