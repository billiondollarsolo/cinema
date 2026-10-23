package main

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
)

const skipWorkflow = `name: CI
on: [pull_request]

jobs:
  # build first
  build:
    runs-on: ubuntu-latest
    steps:
      - run: make build

  lint:
    runs-on: ubuntu-latest
    steps:
      - run: make lint

  unit:
    needs: [build]
    runs-on: ubuntu-latest
    steps:
      - run: make test

  e2e:
    needs:
      - build
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    steps:
      - run: make e2e

  report:
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: make report

  matrix:
    strategy: {matrix: {go: ["1.26", "1.27"]}}
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
`

func TestPlanSkips(t *testing.T) {
	job := func(id string, combo map[string]string) analyze.Job {
		return analyze.Job{Workflow: "ci.yml", ID: id, Combo: combo, Verdict: analyze.Task, Proposal: &analyze.Proposal{ID: id}}
	}
	all := []analyze.Job{job("build", nil), job("lint", nil), job("unit", nil), job("e2e", nil), job("report", nil),
		job("matrix", map[string]string{"go": "1.26"}), job("matrix", map[string]string{"go": "1.27"})}
	ready := []analyze.Job{all[0], all[1], all[2], all[3], all[4], all[5]} // one matrix leg only
	p, err := planSkips("ci.yml", []byte(skipWorkflow), all, ready, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.covered, ",") != "e2e,lint,unit" {
		t.Errorf("covered %v, want e2e, lint, unit", p.covered)
	}
	left := strings.Join(p.left, "\n")
	for _, want := range []string{"build: other jobs need it", "report: its if: uses status functions", "matrix: 1 of 2 matrix legs"} {
		if !strings.Contains(left, want) {
			t.Errorf("left alone: missing %q in\n%s", want, left)
		}
	}
	out := string(p.out)
	// Nothing of the original is lost or reordered: its lines (minus the two it
	// rewrites) appear in order, and every extra line is one cinema adds.
	orig := strings.ReplaceAll(skipWorkflow, "    needs: [build]\n", "")
	orig = strings.ReplaceAll(orig, "    if: github.event_name == 'pull_request'\n", "")
	want := strings.SplitAfter(orig, "\n")
	i := 0
	cinemaBlock := false
	for _, l := range strings.SplitAfter(out, "\n") {
		if i < len(want) && l == want[i] && !cinemaBlock {
			i++
			continue
		}
		switch {
		case l == "  cinema:\n":
			cinemaBlock = true
		case cinemaBlock && (strings.HasPrefix(l, "    ") || l == "\n"):
		case strings.Contains(l, "needs.cinema.outputs") || strings.Contains(l, "needs: cinema") ||
			strings.Contains(l, "needs: [cinema") || l == "      - cinema\n":
		default:
			if cinemaBlock && i < len(want) && l == want[i] {
				cinemaBlock = false
				i++
				continue
			}
			t.Errorf("unexpected line %q", l)
		}
	}
	if i != len(want) {
		t.Errorf("original line %d %q missing or out of order in:\n%s", i, want[i], out)
	}
	var doc struct {
		Jobs map[string]struct {
			Needs any    `yaml:"needs"`
			If    string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(p.out, &doc); err != nil {
		t.Fatalf("edited workflow isn't YAML: %v\n%s", err, out)
	}
	for id, want := range map[string]string{
		"lint": "${{ !cancelled() && needs.cinema.outputs.skip_lint != 'true' }}",
		"unit": "${{ !cancelled() && needs.cinema.outputs.skip_unit != 'true' && needs.build.result == 'success' }}",
		"e2e":  "${{ !cancelled() && needs.cinema.outputs.skip_e2e != 'true' && needs.build.result == 'success' && (github.event_name == 'pull_request') }}",
	} {
		if got := doc.Jobs[id].If; got != want {
			t.Errorf("%s if:\n got %s\nwant %s", id, got, want)
		}
	}
	if n := doc.Jobs["unit"].Needs; !strings.Contains(strings.Join(toStrings(n), ","), "cinema,build") {
		t.Errorf("unit needs %v", n)
	}
	if n := toStrings(doc.Jobs["e2e"].Needs); strings.Join(n, ",") != "cinema,build" {
		t.Errorf("e2e needs %v", n)
	}
	if !strings.Contains(out, "--skip-outputs ci.yml") || !strings.Contains(out, "cinema@v0.1.0") {
		t.Errorf("cinema job:\n%s", out)
	}
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, e.(string))
		}
		return out
	}
	return nil
}

// After setup edits a workflow, analyze still sees the jobs as written: the
// cinema job is cinema's own, and the skip conditions aren't the job's.
func TestAnalyzeAfterSkipInPlace(t *testing.T) {
	job := func(id string) analyze.Job {
		return analyze.Job{Workflow: "ci.yml", ID: id, Verdict: analyze.Task, Proposal: &analyze.Proposal{ID: id}}
	}
	all := []analyze.Job{job("build"), job("lint"), job("unit"), job("e2e"), job("report"), job("matrix"), job("matrix")}
	p, err := planSkips("ci.yml", []byte(skipWorkflow), all, all[:5], "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := analyze.Workflow("ci.yml", []byte(skipWorkflow))
	after, err := analyze.Workflow("ci.yml", p.out)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d jobs after setup, %d before", len(after), len(before))
	}
	for i := range before {
		if before[i].ID != after[i].ID || before[i].Verdict != after[i].Verdict || strings.Join(before[i].Reasons, ";") != strings.Join(after[i].Reasons, ";") {
			t.Errorf("%s: before %s %q, after %s %q", before[i].ID, before[i].Verdict, before[i].Reasons, after[i].Verdict, after[i].Reasons)
		}
	}
}
