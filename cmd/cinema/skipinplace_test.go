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

const matrixWorkflow = `on: [pull_request]
jobs:
  test:
    name: Test
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
        node: ["20.x", "22.x"]
    runs-on: ${{ matrix.os }}
    steps:
      - run: npm test
  numbers:
    strategy:
      matrix:
        node: [20, 22]
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - run: npm test
`

// A matrix with legs the machine can't run drops only its reused legs: the job
// never skips whole, since cinema can't vouch for the legs it doesn't see.
func TestPlanSkipsMatrixExclude(t *testing.T) {
	leg := func(id, os, node string) analyze.Job {
		return analyze.Job{Workflow: "ci.yml", ID: id, Verdict: analyze.Task, Combo: map[string]string{"os": os, "node": node},
			MatrixKeys: []string{"os", "node"}, Proposal: &analyze.Proposal{ID: id}}
	}
	var all, ready []analyze.Job
	for _, id := range []string{"test", "numbers"} {
		for _, os := range []string{"ubuntu-latest", "macos-latest"} {
			for _, n := range []string{"20.x", "22.x"} {
				if id == "numbers" {
					n = strings.TrimSuffix(n, ".x")
				}
				j := leg(id, os, n)
				all = append(all, j)
				if os == "ubuntu-latest" {
					ready = append(ready, j)
				}
			}
		}
	}
	p, err := planSkips("ci.yml", []byte(matrixWorkflow), all, ready, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	out := string(p.out)
	if strings.Join(p.covered, ",") != "test" || len(p.skippable) != 0 || strings.Join(p.matrix, ",") != "test" {
		t.Fatalf("covered %v skippable %v matrix %v\n%s", p.covered, p.skippable, p.matrix, out)
	}
	if !strings.Contains(strings.Join(p.left, "\n"), "numbers: 2 of 4 matrix legs are ready, and its matrix has values that aren't plain strings") {
		t.Errorf("left: %v", p.left)
	}
	for _, want := range []string{
		"        exclude: ${{ fromJSON(needs.cinema.outputs.exclude_test || '[]') }}\n        os: [ubuntu-latest, macos-latest]",
		"    if: \"${{ !cancelled() }}\"\n",
		"exclude_test: ${{ steps.verify.outputs.exclude_test }}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "skip_test") {
		t.Errorf("a partly covered job must never skip whole:\n%s", out)
	}
	before, _ := analyze.Workflow("ci.yml", []byte(matrixWorkflow))
	after, err := analyze.Workflow("ci.yml", p.out)
	if err != nil || len(after) != len(before) {
		t.Fatalf("after setup: %v, %d jobs vs %d", err, len(after), len(before))
	}
	for i := range before {
		if before[i].Verdict != after[i].Verdict || strings.Join(before[i].Reasons, ";") != strings.Join(after[i].Reasons, ";") {
			t.Errorf("%s: before %s %q, after %s %q", before[i].ID, before[i].Verdict, before[i].Reasons, after[i].Verdict, after[i].Reasons)
		}
	}
}

// unsetup gives back exactly the workflow setup started from.
func TestUnsetupRoundTrip(t *testing.T) {
	job := func(id string, combo map[string]string, keys []string) analyze.Job {
		return analyze.Job{Workflow: "ci.yml", ID: id, Combo: combo, MatrixKeys: keys, Verdict: analyze.Task, Proposal: &analyze.Proposal{ID: id}}
	}
	plain := []analyze.Job{job("build", nil, nil), job("lint", nil, nil), job("unit", nil, nil), job("e2e", nil, nil), job("report", nil, nil),
		job("matrix", map[string]string{"go": "1.26"}, nil), job("matrix", map[string]string{"go": "1.27"}, nil)}
	var mall, mready []analyze.Job
	for _, os := range []string{"ubuntu-latest", "macos-latest"} {
		for _, n := range []string{"20.x", "22.x"} {
			j := job("test", map[string]string{"os": os, "node": n}, []string{"os", "node"})
			mall = append(mall, j)
			if os == "ubuntu-latest" {
				mready = append(mready, j)
			}
		}
	}
	for name, c := range map[string]struct {
		src        string
		all, ready []analyze.Job
	}{
		"plain":  {skipWorkflow, plain, plain[:5]},
		"matrix": {matrixWorkflow, mall, mready},
	} {
		p, err := planSkips("ci.yml", []byte(c.src), c.all, c.ready, "v0.1.0")
		if err != nil || len(p.covered) == 0 {
			t.Fatalf("%s: %v %+v", name, err, p)
		}
		if got := string(unsetup(p.out)); got != c.src {
			t.Errorf("%s: unsetup didn't give back the original:\n%s", name, got)
		}
		if got := string(unsetup([]byte(c.src))); got != c.src {
			t.Errorf("%s: unsetup changed a workflow setup never touched", name)
		}
	}
}

// Review findings: each of these used to produce broken YAML, a changed
// condition, leftover edits, or a skip that tested the wrong code.
func TestPlanSkipsEdgeCases(t *testing.T) {
	ready := func(ids ...string) []analyze.Job {
		var out []analyze.Job
		for _, id := range ids {
			out = append(out, analyze.Job{Workflow: "ci.yml", ID: id, Verdict: analyze.Task, Proposal: &analyze.Proposal{ID: id}})
		}
		return out
	}
	src := `on: [pull_request]
jobs:
  colon:
    if: "!startsWith(github.head_ref, 'chore: release')"
    runs-on: ubuntu-latest
    steps: [{run: make}]
  folded:
    if: >-
      github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    steps: [{run: make}]
  continued:
    if: github.event_name == 'pull_request' &&
      github.actor != 'bot'
    runs-on: ubuntu-latest
    steps: [{run: make}]
  multineeds:
    if: github.event_name == 'pull_request'
    needs: [a,
      b]
    runs-on: ubuntu-latest
    steps: [{run: make}]
  a: {runs-on: ubuntu-latest, steps: [{run: make}]}
  b: {runs-on: ubuntu-latest, steps: [{run: make}]}
`
	all := ready("colon", "folded", "continued", "multineeds")
	p, err := planSkips("ci.yml", []byte(src), all, all, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.covered, ",") != "colon" {
		t.Errorf("covered %v, want only colon; left %v", p.covered, p.left)
	}
	var doc struct {
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(p.out, &doc); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, p.out)
	}
	if got := doc.Jobs["colon"].If; got != "${{ !cancelled() && needs.cinema.outputs.skip_colon != 'true' && (!startsWith(github.head_ref, 'chore: release')) }}" {
		t.Errorf("colon if: %s", got)
	}
	if doc.Jobs["multineeds"].If != "github.event_name == 'pull_request'" {
		t.Errorf("multineeds was left alone but its if: changed: %q", doc.Jobs["multineeds"].If)
	}
	if !strings.Contains(string(p.out), "timeout-minutes: 10") {
		t.Error("cinema job has no timeout")
	}
	// Undo gives back a valid workflow with the same condition.
	var back struct {
		Jobs map[string]struct {
			If string `yaml:"if"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(unsetup(p.out), &back); err != nil || strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(back.Jobs["colon"].If, "${{"), "}}")) != "!startsWith(github.head_ref, 'chore: release')" {
		t.Errorf("undo: %v %q\n%s", err, back.Jobs["colon"].If, unsetup(p.out))
	}

	// Older setups wrote the condition unquoted (the lab mirrors have it).
	old := "on: [pull_request]\njobs:\n  cinema:\n    runs-on: ubuntu-latest\n    steps: [{run: cinema verify --event x --skip-outputs ci.yml}]\n\n  lint:\n    needs: cinema\n    if: ${{ !cancelled() && needs.cinema.outputs.skip_lint != 'true' }}\n    runs-on: ubuntu-latest\n    steps: [{run: make lint}]\n"
	if got, want := string(unsetup([]byte(old))), "on: [pull_request]\njobs:\n  lint:\n    runs-on: ubuntu-latest\n    steps: [{run: make lint}]\n"; got != want {
		t.Errorf("undo of an unquoted condition:\n%s", got)
	}

	// A project's own job named cinema is never touched.
	own := "on: [pull_request]\njobs:\n  cinema:\n    runs-on: ubuntu-latest\n    steps: [{run: cinema verify --event x --execute}]\n  lint:\n    needs: cinema\n    runs-on: ubuntu-latest\n    steps: [{run: make lint}]\n"
	if got := string(unsetup([]byte(own))); got != own {
		t.Errorf("undo removed the project's own cinema job:\n%s", got)
	}
	if _, err := planSkips("ci.yml", []byte(own), ready("lint"), ready("lint"), "v"); err == nil {
		t.Error("planned over the project's own cinema job")
	}

	// pull_request_target checks out the base branch: never skip there.
	prt := "on: {pull_request_target: {}}\njobs:\n  lint: {runs-on: ubuntu-latest, steps: [{run: make}]}\n"
	if _, err := planSkips("ci.yml", []byte(prt), ready("lint"), ready("lint"), "v"); err == nil || !strings.Contains(err.Error(), "pull_request_target") {
		t.Errorf("pull_request_target: %v", err)
	}

	// A matrix with include: gets no exclude (exclude runs before include).
	inc := "on: [pull_request]\njobs:\n  test:\n    strategy:\n      matrix:\n        os: [ubuntu-latest, macos-latest]\n        include:\n          - os: ubuntu-latest\n            coverage: \"true\"\n    runs-on: ${{ matrix.os }}\n    steps: [{run: make}]\n"
	leg := analyze.Job{Workflow: "ci.yml", ID: "test", Verdict: analyze.Task, Combo: map[string]string{"os": "ubuntu-latest"}, MatrixKeys: []string{"os"}, Proposal: &analyze.Proposal{ID: "t"}}
	mac := leg
	mac.Combo = map[string]string{"os": "macos-latest"}
	p, err = planSkips("ci.yml", []byte(inc), []analyze.Job{leg, mac}, []analyze.Job{leg}, "v")
	if err != nil || len(p.covered) != 0 || !strings.Contains(strings.Join(p.left, " "), "extends one of its legs") {
		t.Errorf("include matrix: %v %+v", err, p)
	}
	// eslint's shape: every include entry differs from every leg on some key, so
	// it stands alone and the reused legs can be excluded.
	standalone := "on: [pull_request]\njobs:\n  test:\n    strategy:\n      matrix:\n        os: [ubuntu-latest]\n        node: [24.x, 22.x]\n        flags: [\"\"]\n        include:\n          - os: windows-latest\n            node: \"lts/*\"\n          - os: ubuntu-latest\n            node: 24.x\n            flags: \"--experimental\"\n    runs-on: ${{ matrix.os }}\n    steps: [{run: make}]\n"
	l1 := analyze.Job{Workflow: "ci.yml", ID: "test", Verdict: analyze.Task, Combo: map[string]string{"os": "ubuntu-latest", "node": "24.x", "flags": ""}, MatrixKeys: []string{"os", "node", "flags"}, Proposal: &analyze.Proposal{ID: "a"}}
	l2 := l1
	l2.Combo = map[string]string{"os": "ubuntu-latest", "node": "22.x", "flags": ""}
	win := l1
	win.Combo = map[string]string{"os": "windows-latest", "node": "lts/*"}
	p, err = planSkips("ci.yml", []byte(standalone), []analyze.Job{l1, l2, win}, []analyze.Job{l1, l2}, "v")
	if err != nil || strings.Join(p.matrix, ",") != "test" {
		t.Errorf("standalone include entries: %v covered %v left %v", err, p.covered, p.left)
	}
}
