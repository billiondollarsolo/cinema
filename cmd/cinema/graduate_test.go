package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// The backtest graduates a stand-in that reproduces CI's results, failures
// included, and refuses one that passes where CI failed (plan §11.5).
func TestBacktest(t *testing.T) {
	e := newEnv(t)
	t.Setenv("CINEMA_DATA_DIR", e.data)
	e.write(".github/workflows/ci.yml", `on: [push, pull_request]
jobs:
  check:
    name: Check
    runs-on: ubuntu-latest
    steps: [{run: test ! -f broken}]
`)
	e.git("add", "-A")
	e.git("commit", "-qm", "ci")
	draft, _ := e.cinema("analyze", "--yaml")
	e.write(".cinema/tasks.yaml", strings.ReplaceAll(draft, "platform: linux/amd64", "platform: any"))
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks") // c1: passes
	e.write("broken", "x")
	e.git("add", "-A")
	e.git("commit", "-qm", "break") // c2: fails
	e.cinema("trust")
	shas := strings.Fields(mustRun(t, e.repo, "git", "rev-list", "--first-parent", "HEAD"))[:2] // c2, c1

	ci := map[string]string{shas[0]: "failure", shas[1]: "success"}
	old := ghAPI
	defer func() { ghAPI = old }()
	ghAPI = func(p string) ([]byte, error) {
		for i, sha := range shas {
			if strings.Contains(p, "head_sha="+sha) { // newest first: another workflow's run with a same-named job
				return json.Marshal(map[string]any{"workflow_runs": []map[string]any{
					{"id": 100 + i, "path": ".github/workflows/other.yml"}, {"id": i + 1, "path": ".github/workflows/ci.yml"}}})
			}
			if strings.Contains(p, fmt.Sprintf("runs/%d/jobs", i+1)) {
				return json.Marshal(map[string]any{"jobs": []map[string]string{{"name": "Check", "conclusion": ci[sha]}}})
			}
			if strings.Contains(p, fmt.Sprintf("runs/%d/jobs", 100+i)) {
				return json.Marshal(map[string]any{"jobs": []map[string]string{{"name": "Check", "conclusion": "cancelled"}}})
			}
		}
		return []byte(`{"workflow_runs":[]}`), nil
	}
	r, err := openRepoAt(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	tk := r.cfg.Tasks["check"]
	g, why := backtest(context.Background(), r, tk, shas)
	if why != "" || g.Backtested != 2 || g.CIFailures != 1 || g.JobDigest != tk.JobDigest {
		t.Fatalf("agreeing history: %+v %q", g, why)
	}
	ci[shas[0]] = "success" // CI passed where the stand-in fails: stricter or broken
	if _, why := backtest(context.Background(), r, tk, shas); !strings.Contains(why, "CI passed and the stand-in failed") {
		t.Fatalf("got %q", why)
	}
	ci[shas[0]], ci[shas[1]] = "failure", "failure" // CI failed where it passes: false green
	if _, why := backtest(context.Background(), r, tk, shas); !strings.Contains(why, "would have skipped a failing job") {
		t.Fatalf("got %q", why)
	}

	path := filepath.Join(e.repo, task.ConfigPath)
	before, _ := os.ReadFile(path)
	if err := writeGraduations(path, map[string]task.Graduation{"check": g}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	cfg, err := task.Parse(after)
	if err != nil || !cfg.Tasks["check"].Graduates() {
		t.Fatalf("not graduated: %v\n%s", err, after)
	}
	if !strings.Contains(string(after), strings.Split(string(before), "\n")[0]) || strings.Count(string(after), "\n") != strings.Count(string(before), "\n")+1 {
		t.Fatalf("the edit must add one line and keep the rest:\n%s", after)
	}
	g.Backtested = 7 // rerun replaces the line
	if err := writeGraduations(path, map[string]task.Graduation{"check": g}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(path)
	if strings.Count(string(again), "graduated:") != 1 || !strings.Contains(string(again), "backtested: 7") {
		t.Fatalf("rerun must replace:\n%s", again)
	}
}

// Live pairs: this machine's clean run on a PR's tree against CI's job on
// that PR, read through the API (plan §11.5).
func TestLivePairs(t *testing.T) {
	e := newEnv(t)
	t.Setenv("CINEMA_DATA_DIR", e.data)
	e.write(".github/workflows/ci.yml", "on: [pull_request]\njobs:\n  check:\n    name: Check\n    runs-on: ubuntu-latest\n    steps: [{run: test ! -f broken}]\n")
	e.git("add", "-A")
	e.git("commit", "-qm", "ci")
	draft, _ := e.cinema("analyze", "--yaml")
	e.write(".cinema/tasks.yaml", strings.ReplaceAll(draft, "platform: linux/amd64", "platform: any"))
	e.git("add", "-A")
	e.git("commit", "-qm", "tasks")
	base := strings.TrimSpace(mustRun(t, e.repo, "git", "rev-parse", "HEAD"))
	e.write("pr.txt", "x")
	e.git("add", "-A")
	e.git("commit", "-qm", "a PR") // contains base: its merge result is its own tree
	head := strings.TrimSpace(mustRun(t, e.repo, "git", "rev-parse", "HEAD"))
	e.cinema("trust")
	e.cinema("run", "--commit", head, "check") // passes

	ci := "success"
	old := ghAPI
	defer func() { ghAPI = old }()
	ghAPI = func(p string) ([]byte, error) {
		switch {
		case strings.Contains(p, "workflows/ci.yml/runs"):
			return json.Marshal(map[string]any{"workflow_runs": []map[string]any{
				{"id": 7, "head_sha": head, "pull_requests": []map[string]any{{"base": map[string]string{"sha": base}}}},
				{"id": 8, "head_sha": base, "pull_requests": []map[string]any{}}}}) // a fork's: no base, ignored
		case strings.Contains(p, "runs/7/jobs"):
			return json.Marshal(map[string]any{"jobs": []map[string]string{{"name": "Check", "conclusion": ci}}})
		}
		return []byte(`{}`), nil
	}
	r, err := openRepoAt(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	tk := r.cfg.Tasks["check"]
	if n, why := livePairs(r, tk, 10); n != 1 || why != "" {
		t.Fatalf("agreeing pair: %d %q", n, why)
	}
	ci = "failure" // CI failed on the tree the stand-in passed: a false green
	if _, why := livePairs(r, tk, 10); !strings.Contains(why, "would have skipped a failing job") {
		t.Fatalf("got %q", why)
	}
	ci = "skipped" // reused in CI: nothing to compare
	if n, why := livePairs(r, tk, 10); n != 0 || why != "" {
		t.Fatalf("skipped job counted: %d %q", n, why)
	}
}

// A job pinned through go-version-file keeps its job_digest when re-analyzed
// from a checkout (verify, graduate): the version file is read there too.
func TestCandidateLegsReadVersionFiles(t *testing.T) {
	e := newEnv(t)
	e.write("go.mod", "module x\n\ngo 1.26.1\n")
	e.write(".github/workflows/ci.yml", `on: [pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-go@v5
        with: { go-version-file: go.mod }
      - run: go test ./...
`)
	draft, _ := e.cinema("analyze", "--yaml")
	cfg, err := task.Parse([]byte(draft))
	if err != nil || cfg.Tasks["test"] == nil || cfg.Tasks["test"].Runtime.Go != "1.26.1" {
		t.Fatalf("draft: %v\n%s", err, draft)
	}
	legs := candidateLegs(e.repo, "ci.yml")["test"]
	if len(legs) != 1 || legs[0].LegDigest() != cfg.Tasks["test"].JobDigest {
		t.Fatalf("job_digest differs when re-analyzed from the checkout: %+v", legs)
	}
}
