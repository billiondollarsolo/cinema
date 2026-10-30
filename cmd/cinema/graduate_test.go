package main

import (
	"context"
	"encoding/json"
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
		for sha, c := range ci {
			if strings.Contains(p, sha) {
				return json.Marshal(map[string]any{"check_runs": []map[string]string{{"name": "Check", "conclusion": c}}})
			}
		}
		return []byte(`{"check_runs":[]}`), nil
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
