package main

import (
	"strings"
	"testing"

	"github.com/billiondollarsolo/cinema/internal/analyze"
)

// Drafts land in the repo, where its formatter checks them (lab: eslint's
// prettier --check rewrote `{node: 24.22.1}` as `{ node: 24.22.1 }` and failed).
// Block maps and flow lists are left alone by prettier.
func TestDraftHasNoFlowMaps(t *testing.T) {
	jobs := []analyze.Job{{Workflow: "ci.yml", ID: "test", Verdict: analyze.Task,
		Proposal: &analyze.Proposal{ID: "test", Go: "1.26.10", Node: "22.23.4", Body: "(\nmake test\n)", Outputs: []string{"coverage"}}}}
	out, err := draft(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "{") {
		t.Fatalf("flow map in draft:\n%s", out)
	}
	for _, want := range []string{"cmd: [go, version]", "go: 1.26.10", "outputs: [coverage]", "allowed: true"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
