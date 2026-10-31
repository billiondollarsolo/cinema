package analyze

import (
	"slices"
	"strings"
	"testing"
)

// Jobs that use a runtime with no setup action get what the runner really
// has: Python is the image's apt python3 (probed, never pinned from
// .python-version); Java and .NET are the image's preinstalled builds (review).
func TestRuntimesWithoutSetup(t *testing.T) {
	fakeReleases(t)
	jobs := repoJobs(t, map[string]string{
		".python-version": "3.12.7\n",
		".github/workflows/ci.yml": `on: pull_request
jobs:
  py:
    runs-on: ubuntu-24.04
    steps: [{run: pip install -r requirements.txt}, {run: python -m pytest}]
  py-pinned:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-python@v5, with: {python-version: 3.12.7}}, {run: pytest}]
  py-container:
    runs-on: ubuntu-latest
    container: python:3.12
    steps: [{run: pytest}]
  java:
    runs-on: ubuntu-latest
    steps: [{run: ./gradlew test}]
  java-setup:
    runs-on: ubuntu-latest
    steps: [{uses: actions/setup-java@v5, with: {distribution: zulu, java-version: 21}}, {run: mvn -B verify}]
  dotnet:
    runs-on: ubuntu-latest
    steps: [{run: dotnet test}]
  bun:
    runs-on: ubuntu-latest
    steps: [{run: bun test}]
`})
	py := jobs["py"]
	if py.Verdict != Task || len(py.Proposal.Runtimes) != 0 || !slices.EqualFunc(py.Proposal.Probes, [][]string{{"python3", "--version"}}, slices.Equal) {
		t.Errorf("py: %s %v %v %v", py.Verdict, py.Reasons, py.Proposal.Runtimes, py.Proposal.Probes)
	}
	if p := jobs["py-pinned"].Proposal; p.Runtimes["python"] != "3.12.7" || len(p.Probes) != 0 {
		t.Errorf("py-pinned: %v %v", p.Runtimes, p.Probes)
	}
	if p := jobs["py-container"].Proposal; len(p.Probes) != 0 {
		t.Errorf("py-container: the image is the toolchain: %v", p.Probes)
	}
	for id, want := range map[string]string{"java": "runner image's default JDK", "dotnet": "preinstalled SDKs"} {
		if j := jobs[id]; j.Verdict != Review || !strings.Contains(strings.Join(j.Reasons, "; "), want) {
			t.Errorf("%s: %s %v", id, j.Verdict, j.Reasons)
		}
	}
	// setup-java ran (and went to review on its own): no second reason.
	if r := strings.Join(jobs["java-setup"].Reasons, "; "); strings.Contains(r, "isn't set up") || !strings.Contains(r, "zulu") {
		t.Errorf("java-setup: %s", r)
	}
	if b := jobs["bun"]; b.Verdict != Task || b.Proposal.Probes != nil || b.Proposal.Runtimes != nil {
		t.Errorf("bun (not on the runner: fails in CI too): %s %v %+v", b.Verdict, b.Reasons, b.Proposal)
	}
}
