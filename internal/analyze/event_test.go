package analyze

import (
	"strings"
	"testing"
)

func TestEventDependentJobs(t *testing.T) {
	jobs, err := Workflow("ci.yml", []byte(`on: [push, pull_request]
jobs:
  plain:
    runs-on: ubuntu-latest
    steps: [{run: make test}]
  push-only-step:
    runs-on: ubuntu-latest
    steps:
      - run: make test
      - if: github.event_name == 'push'
        run: make integration
  reads-ref:
    runs-on: ubuntu-latest
    steps: [{run: 'echo "$GITHUB_REF"'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"plain": "", "push-only-step": "pull_request", "reads-ref": "pull_request"}
	for _, j := range jobs {
		if j.Proposal == nil || j.Proposal.Event != want[j.ID] {
			t.Errorf("%s: event %q, want %q", j.ID, j.Proposal.Event, want[j.ID])
		}
	}
}

// A pin resolved from a floating spec records the spec, so the gate can
// resolve it again; an exact version in the workflow records nothing.
func TestFloatingRecorded(t *testing.T) {
	old := fetchAction
	defer func() { fetchAction = old }()
	jobs, err := Workflow("ci.yml", []byte(`on: pull_request
jobs:
  exact:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-go@v5, with: {go-version: 1.26.10}}, {run: go test ./...}]
`))
	if err != nil || jobs[0].Proposal == nil {
		t.Fatal(err, jobs)
	}
	if f := jobs[0].Proposal.Floating; len(f) != 0 {
		t.Fatalf("exact version: floating %v", f)
	}
	for _, n := range jobs[0].Notes {
		if strings.HasPrefix(n, floatingNote) {
			t.Fatalf("internal marker leaked into notes: %q", n)
		}
	}
}

// pnpm/setup (pnpm plus a runtime) translates: the Node pin and pnpm itself.
func TestPnpmSetup(t *testing.T) {
	jobs, err := Workflow("ci.yml", []byte(`on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: pnpm/setup@a0881977cd8ea73b1ed212236c99ca10dcc80cf4
        with: {runtime: node@24.11.1, version: 11.2.0, cache: true}
      - run: pnpm test
  bun:
    runs-on: ubuntu-latest
    steps:
      - uses: pnpm/setup@a0881977cd8ea73b1ed212236c99ca10dcc80cf4
        with: {runtime: bun@1, version: 11.2.0}
      - run: pnpm test
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		switch j.ID {
		case "test":
			if j.Verdict != Task || j.Proposal.Node != "24.11.1" || !strings.Contains(j.Proposal.Body, "npm install -g pnpm@11.2.0") {
				t.Fatalf("test: %v %v %+v", j.Verdict, j.Reasons, j.Proposal)
			}
		case "bun":
			if j.Verdict != Review {
				t.Fatalf("bun runtime must be review: %v", j.Verdict)
			}
		}
	}
}
