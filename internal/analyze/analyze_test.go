package analyze

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ci = `
on: [push, pull_request]
env: {CI: "true", TOKEN: "${{ secrets.T }}"}
jobs:
  test:
    strategy:
      matrix:
        os: [ubuntu-latest, windows-latest]
        node: [20.19.0, 22.x]
        exclude: [{os: windows-latest, node: 22.x}]
        include: [{os: ubuntu-latest, node: 20.19.0, flags: --strict}]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-node@v4
        with: {node-version: "${{ matrix.node }}"}
      - run: npm ci
      - run: |
          npm test ${{ matrix.flags }}
        working-directory: pkg
  lint:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/setup-go@v5
        with: {go-version: 1.26.10}
      - run: make lint
  db:
    runs-on: ubuntu-latest
    services: {postgres: {image: postgres}}
    steps: [{run: make itest}]
  analyze:
    runs-on: ubuntu-latest
    steps: [{uses: github/codeql-action/analyze@v3}]
`

func TestWorkflow(t *testing.T) {
	old := fetch
	defer func() { fetch = old }()
	fetch = func(string) ([]byte, error) { return nil, errors.New("offline") } // ranges stay unresolved
	releases.m = map[string][]release{}
	jobs, err := Workflow("ci.yml", []byte(ci))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Job{}
	for _, j := range jobs {
		key := j.ID
		if j.Combo != nil {
			key += "/" + j.Combo["os"] + "/" + j.Combo["node"]
		}
		got[key] = j
	}
	want := map[string]string{
		"analyze": Skip, "db": Skip, "lint": Review, // lint: workflow env TOKEN is a secret
		"test/ubuntu-latest/20.19.0": Review, "test/ubuntu-latest/22.x": Review, "test/windows-latest/20.19.0": Skip,
	}
	if len(got) != len(want) {
		t.Fatalf("jobs: %v", keys(got))
	}
	for k, v := range want {
		if got[k].Verdict != v {
			t.Errorf("%s: %s %v, want %s", k, got[k].Verdict, got[k].Reasons, v)
		}
	}
	p := got["test/ubuntu-latest/20.19.0"].Proposal
	if p == nil || p.Node != "20.19.0" || p.ID != "test-strict-20.19.0-ubuntu-latest" {
		t.Fatalf("proposal: %+v", p)
	}
	for _, s := range []string{"export CI=true", "(\n  npm ci\n)", "cd pkg", "npm test --strict"} {
		if !strings.Contains(p.Body, s) {
			t.Errorf("script lacks %q:\n%s", s, p.Body)
		}
	}
	if strings.Contains(p.Body, "secrets") {
		t.Errorf("secret leaked into the script:\n%s", p.Body)
	}
	if r := got["test/ubuntu-latest/22.x"]; r.Proposal.Node != "" || !strings.Contains(strings.Join(r.Reasons, ";"), `"22.x" isn't exact`) ||
		strings.Contains(r.Proposal.Body, "matrix.") || strings.Contains(strings.Join(r.Reasons, ";"), "expression") {
		t.Errorf("22.x: node=%q reasons=%v", r.Proposal.Node, r.Reasons)
	}
	if got["lint"].Proposal.Go != "1.26.10" {
		t.Errorf("lint go: %+v", got["lint"].Proposal)
	}
}

func TestNotCI(t *testing.T) {
	jobs, _ := Workflow("rel.yml", []byte("on: {release: {}}\njobs: {x: {runs-on: ubuntu-latest, steps: [{run: make}]}}\n"))
	if len(jobs) != 0 {
		t.Fatalf("release workflow analyzed: %v", jobs)
	}
}

func keys(m map[string]Job) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestEvalIf(t *testing.T) {
	combo := map[string]string{"node": "14", "os": "ubuntu-latest", "COV": "true"}
	for src, want := range map[string]bool{
		"${{ matrix.node == '14' || matrix.node == '16' }}":    true,
		"matrix.node != '14' && matrix.os != 'windows-latest'": false,
		"${{ !matrix.COV }}": false,
		"matrix.missing":     false,
		"(matrix.node == '18' || matrix.os == 'UBUNTU-LATEST')": true,
		"success() && matrix.node == 14":                        true,
		"failure()":                                             false,
	} {
		if v, ok := evalIf(src, combo); !ok || v != want {
			t.Errorf("%s: %v ok=%v, want %v", src, v, ok, want)
		}
	}
	for _, src := range []string{"github.event_name == 'push'", "steps.x.outputs.y", "contains(matrix.os, 'ubuntu')", "matrix.node ="} {
		if _, ok := evalIf(src, combo); ok {
			t.Errorf("%s: evaluated, want unsupported", src)
		}
	}
}

func TestVersionFile(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "go.mod"), []byte("module x\n\ngo 1.26.0\n\ntoolchain go1.26.10\n"), 0o644)
	os.WriteFile(filepath.Join(d, ".nvmrc"), []byte("v22.23.4\n"), 0o644)
	if v := versionFromFile(filepath.Join(d, "go.mod")); v != "1.26.10" {
		t.Errorf("go.mod: %q", v)
	}
	if v := versionFromFile(filepath.Join(d, ".nvmrc")); v != "22.23.4" {
		t.Errorf(".nvmrc: %q", v)
	}
}

func TestResolve(t *testing.T) {
	old := fetch
	defer func() { fetch = old }()
	fetch = func(url string) ([]byte, error) {
		if strings.Contains(url, "nodejs") {
			return []byte(`[{"version":"v26.1.0","lts":false},{"version":"v24.22.1","lts":"Krypton"},{"version":"v22.23.4","lts":"Jod"},{"version":"v22.23.3","lts":"Jod"}]`), nil
		}
		return []byte(`[{"version":"go1.27rc1","stable":false},{"version":"go1.26.10","stable":true},{"version":"go1.26.9","stable":true}]`), nil
	}
	releases.m = map[string][]release{}
	for spec, want := range map[string]string{"22": "22.23.4", "22.x": "22.23.4", "lts/*": "24.22.1", "22.23.3": "22.23.3", "21": ""} {
		if got, _ := resolve("Node", spec); got != want {
			t.Errorf("node %s: %q, want %q", spec, got, want)
		}
	}
	for spec, want := range map[string]string{"1.26": "1.26.10", "stable": "1.26.10", "1.27": ""} {
		if got, _ := resolve("Go", spec); got != want {
			t.Errorf("go %s: %q, want %q", spec, got, want)
		}
	}
}

func TestPrintable(t *testing.T) {
	if got := Printable("a\nb\x1b[31mc d‮e"); strings.ContainsAny(got, "\n\x1b ‮") {
		t.Fatalf("%q", got)
	}
}

func TestVersionFileUntrusted(t *testing.T) {
	d := t.TempDir()
	os.Symlink("/dev/zero", filepath.Join(d, ".nvmrc"))
	if v := versionFromFile(filepath.Join(d, ".nvmrc")); v != "" {
		t.Fatalf("followed a symlink: %q", v)
	}
	if v := exactVersion(d, "", `^x$`, "Node", "../../etc/passwd", func(string) {}, func(string) {}); v != "" {
		t.Fatalf("read outside the repo: %q", v)
	}
}
