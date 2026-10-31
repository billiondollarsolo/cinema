package analyze

import (
	"slices"
	"strings"
	"testing"
)

// Lab (gson): `mvn` without the wrapper is the runner image's Maven. It's
// probed, not pinned: a pin would be provisioned on the CI runner too, and the
// probe there would see cinema's Maven instead of the job's.
func TestJVMBuildToolsWithoutWrapper(t *testing.T) {
	fakeJavaReleases(t)
	java := "{uses: actions/setup-java@v5, with: {distribution: temurin, java-version: \"21.0.5+11\"}}"
	jobs := repoJobs(t, map[string]string{".github/workflows/ci.yml": `on: pull_request
jobs:
  mvn:
    runs-on: ubuntu-24.04
    steps: [` + java + `, {run: mvn -B verify}]
  mvnw:
    runs-on: ubuntu-latest
    steps: [` + java + `, {run: ./mvnw -B verify && cat gradle/libs.versions.toml}]
  setup-maven:
    runs-on: ubuntu-latest
    steps: [` + java + `, {uses: stCarolas/setup-maven@v5, with: {maven-version: 3.9.9}}, {run: mvn verify}]
  setup-maven-default:
    runs-on: ubuntu-latest
    steps: [` + java + `, {uses: stCarolas/setup-maven@v5}, {run: mvn verify}]
  setup-maven-range:
    runs-on: ubuntu-latest
    steps: [` + java + `, {uses: stCarolas/setup-maven@v5, with: {maven-version: 3.x}}, {run: mvn verify}]
  gradle:
    runs-on: ubuntu-latest
    steps: [` + java + `, {run: gradle build}]
  gradle-pinned:
    runs-on: ubuntu-latest
    steps: [` + java + `, {uses: gradle/actions/setup-gradle@v4, with: {gradle-version: 8.14.3, validate-wrappers: false}}, {run: gradle build}]
  ant:
    runs-on: ubuntu-latest
    steps: [` + java + `, {run: ant test}]
  sbt:
    runs-on: ubuntu-22.04
    steps: [` + java + `, {run: sbt test}]
`})
	mvnProbe := []string{"mvn", "--version"}
	if j := jobs["mvn"]; j.Verdict != Task || j.Proposal.Runtimes["maven"] != "" || !slices.ContainsFunc(j.Proposal.Probes, func(p []string) bool { return slices.Equal(p, mvnProbe) }) ||
		!strings.Contains(strings.Join(j.Notes, "; "), "runner image's mvn") {
		t.Errorf("mvn: %s %v %v %v %v", j.Verdict, j.Reasons, j.Proposal.Runtimes, j.Proposal.Probes, j.Notes)
	}
	if j := jobs["mvnw"]; j.Verdict != Task || len(j.Proposal.Probes) != 0 {
		t.Errorf("mvnw: %s %v %v", j.Verdict, j.Reasons, j.Proposal.Probes)
	}
	for id, want := range map[string]string{"setup-maven": "3.9.9", "setup-maven-default": "3.8.2"} {
		// The pin's own probe comes from the runtime kind (cmd/cinema): none added here.
		if j := jobs[id]; j.Verdict != Task || j.Proposal.Runtimes["maven"] != want || len(j.Proposal.Probes) != 0 {
			t.Errorf("%s: %s %v %v %v", id, j.Verdict, j.Reasons, j.Proposal.Runtimes, j.Proposal.Probes)
		}
	}
	if j := jobs["gradle-pinned"]; j.Verdict != Task || j.Proposal.Runtimes["gradle"] != "8.14.3" {
		t.Errorf("gradle-pinned: %s %v %v", j.Verdict, j.Reasons, j.Proposal.Runtimes)
	}
	for id, want := range map[string]string{"setup-maven-range": "exact Maven release", "gradle": "Gradle isn't set up", "ant": "apt ant", "sbt": "sbt launcher"} {
		if j := jobs[id]; j.Verdict != Review || !strings.Contains(strings.Join(j.Reasons, "; "), want) {
			t.Errorf("%s: %s %v", id, j.Verdict, j.Reasons)
		}
	}
}
