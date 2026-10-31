package main

import (
	"strings"
	"testing"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/task"
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

// A tool the job uses unpinned (the runner's python3) is probed, after the
// pinned runtimes' version commands, and the draft loads.
func TestDraftProbesUnpinnedTools(t *testing.T) {
	for _, p := range []*analyze.Proposal{
		{ID: "test", Body: "(\npytest\n)", Probes: [][]string{{"python3", "--version"}}},
		{ID: "test", Node: "22.23.4", Body: "(\npytest\n)", Probes: [][]string{{"python3", "--version"}}},
	} {
		out, err := draft([]analyze.Job{{Workflow: "ci.yml", ID: "test", Verdict: analyze.Task, Proposal: p}})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := task.Parse(out)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		tc := cfg.Tasks["test"].Toolchain
		if len(tc) == 0 || strings.Join(tc[len(tc)-1].Cmd, " ") != "python3 --version" || (p.Node != "" && strings.Join(tc[0].Cmd, " ") != "node --version") {
			t.Errorf("toolchain %+v\n%s", tc, out)
		}
	}
}

// floating: and preinstalled: load as drafted, false values included (absent
// means a task from before, checked both ways).
func TestDraftPreinstalledLoads(t *testing.T) {
	p := &analyze.Proposal{ID: "test", Go: "1.26.9", Node: "24.22.2", Body: "(\nmake test\n)",
		Floating: map[string]string{"go": "1.26", "node": "lts/*"}, Preinstalled: map[string]bool{"go": true, "node": false}}
	out, err := draft([]analyze.Job{{Workflow: "ci.yml", ID: "test", Verdict: analyze.Task, Proposal: p}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := task.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	tk := cfg.Tasks["test"]
	if pre, ok := tk.Preinstalled["node"]; tk.Floating["go"] != "1.26" || !tk.Preinstalled["go"] || pre || !ok {
		t.Fatalf("floating %v preinstalled %v\n%s", tk.Floating, tk.Preinstalled, out)
	}
}

// A drafted services: block loads as task services.
func TestDraftServicesLoad(t *testing.T) {
	jobs := []analyze.Job{{Workflow: "ci.yml", ID: "db", Verdict: analyze.Task, Proposal: &analyze.Proposal{ID: "db", Body: "(\nmake test\n)",
		Services: map[string]analyze.Service{"postgres": {Image: "postgres:16", Env: map[string]string{"POSTGRES_PASSWORD": "pw"}, Ports: []string{"5432:5432"}, Options: "--health-cmd pg_isready"}}}}}
	out, err := draft(jobs)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := task.Parse(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if s := cfg.Tasks["db"].Services["postgres"]; s.Image != "postgres:16" || s.Env["POSTGRES_PASSWORD"] != "pw" || s.Options != "--health-cmd pg_isready" {
		t.Fatalf("%+v\n%s", s, out)
	}
	if strings.Contains(string(out), "{") {
		t.Fatalf("flow map in draft:\n%s", out)
	}
}

// An empty matrix value is written as "", not an empty mapping value.
func TestDraftQuotesEmptyValues(t *testing.T) {
	jobs := []analyze.Job{{Workflow: "ci.yml", ID: "test", Verdict: analyze.Task, MatrixKeys: []string{"os", "flags"},
		Combo: map[string]string{"os": "ubuntu-latest", "flags": ""}, Proposal: &analyze.Proposal{ID: "test", Body: "(\nnpm test\n)"}}}
	out, err := draft(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `flags: ""`) {
		t.Fatalf("empty value not quoted:\n%s", out)
	}
}
