package analyze

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeJavaReleases serves Adoptium's release lists (x64 has a build aarch64
// lacks yet) and Gradle's.
func fakeJavaReleases(t *testing.T) {
	old := fetch
	t.Cleanup(func() { fetch = old; releases.m = map[string][]release{} })
	releases.m = map[string][]release{}
	fetch = func(url string) ([]byte, error) {
		switch {
		case strings.Contains(url, "adoptium") && strings.Contains(url, "page=0") && strings.Contains(url, "architecture=x64"):
			return []byte(`{"versions":[{"semver":"25.0.1+8.0.LTS"},{"semver":"21.0.6+7.0.LTS"},{"semver":"21.0.5+11.0.LTS"},{"semver":"17.0.20+101"},{"semver":"17.0.20+8"},{"semver":"8.0.504+1"}]}`), nil
		case strings.Contains(url, "adoptium") && strings.Contains(url, "page=0"):
			return []byte(`{"versions":[{"semver":"25.0.1+8.0.LTS"},{"semver":"21.0.5+11.0.LTS"},{"semver":"21.0.0-beta+35"},{"semver":"17.0.20+101"},{"semver":"17.0.20+8"}]}`), nil
		case strings.Contains(url, "services.gradle.org/versions/all"):
			return []byte(`[{"version":"9.9-rc-1","rcFor":"9.9"},{"version":"9.8.2","current":true},{"version":"8.14.4"},{"version":"9.8.1"}]`), nil
		}
		return nil, os.ErrNotExist
	}
}

func javaJob(t *testing.T, root, steps string) Job {
	t.Helper()
	wf := "on: pull_request\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n" + steps + "      - run: ./mvnw -B verify\n"
	var jobs []Job
	var err error
	if root == "" {
		jobs, err = Workflow("ci.yml", []byte(wf))
	} else {
		os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o755)
		os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte(wf), 0o644)
		jobs, err = Repo(root)
	}
	if err != nil || len(jobs) != 1 {
		t.Fatalf("%v %v", err, jobs)
	}
	return jobs[0]
}

func TestSetupJavaTranslated(t *testing.T) {
	fakeJavaReleases(t)
	for _, c := range []struct {
		with, pin, review string
	}{
		{`{distribution: temurin, java-version: "21.0.5+11"}`, "temurin-21.0.5+11", ""},
		{`{distribution: temurin, java-version: "21", check-latest: true}`, "temurin-21.0.6+7", "add floating: {java: \"temurin-21\"}"},
		{`{distribution: temurin, java-version: "17.x", check-latest: true}`, "temurin-17.0.20+101", "floating"},
		{`{distribution: temurin, java-version: "21"}`, "temurin-21.0.6+7", "without check-latest: true"},
		{`{distribution: temurin, java-version: "21.0.5"}`, "", "newest build of 21.0.5"},
		{`{distribution: zulu, java-version: "21"}`, "", "distribution zulu"},
		{`{java-version: "21"}`, "", "without a distribution"},
		{`{distribution: temurin, java-version: "21.0.5+11", java-package: jre}`, "", "java-package jre"},
		{`{distribution: temurin, java-version: "21.0.5+11", architecture: x86}`, "", "architecture x86"},
		{`{distribution: temurin, java-version: "21-ea"}`, "", "EA"},
		{`{distribution: temurin, java-version: "${{ matrix.java }}"}`, "", "expression"},
		{`{distribution: temurin, java-version: "17\n21\n"}`, "", "several JDKs"},
		{`{distribution: temurin, java-version: "21.0.5+11", gpg-private-key: "${{ secrets.GPG }}"}`, "temurin-21.0.5+11", "GPG"},
		{`{distribution: temurin, java-version: "21.0.5+11", mvn-repositories: "x"}`, "temurin-21.0.5+11", "settings.xml"},
	} {
		j := javaJob(t, "", "      - uses: actions/setup-java@v5\n        with: "+c.with+"\n")
		reasons := strings.Join(j.Reasons, "; ")
		if j.Proposal == nil || j.Proposal.Runtimes["java"] != c.pin || (c.review == "") != (j.Verdict != Review) || !strings.Contains(reasons, c.review) {
			t.Errorf("%s: pin %v, %s %q", c.with, j.Proposal.Runtimes, j.Verdict, reasons)
		}
	}
	j := javaJob(t, "", "      - uses: actions/setup-java@v5\n        with: {distribution: temurin, java-version: \"21.0.5+11\"}\n")
	if !strings.Contains(j.Proposal.Body, `MAVEN_ARGS=${MAVEN_ARGS:+$MAVEN_ARGS }-ntp`) || strings.Index(j.Proposal.Body, "-ntp") > strings.Index(j.Proposal.Body, "./mvnw") {
		t.Errorf("MAVEN_ARGS step missing or after the build:\n%s", j.Proposal.Body)
	}
	// Workflow env feeds ${{ env.X }}.
	wf := "on: pull_request\nenv: {JAVA: \"21.0.5+11\"}\njobs:\n  b:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-java@v4\n        with: {distribution: temurin, java-version: \"${{ env.JAVA }}\"}\n      - run: mvn verify\n"
	if jobs, _ := Workflow("ci.yml", []byte(wf)); len(jobs) != 1 || jobs[0].Proposal.Runtimes["java"] != "temurin-21.0.5+11" {
		t.Errorf("env: %+v", jobs)
	}
}

// java-version-file is read as setup-java reads it, and its vendor wins.
func TestSetupJavaVersionFile(t *testing.T) {
	fakeJavaReleases(t)
	for _, c := range []struct {
		file, content, dist, pin, review string
	}{
		{".java-version", "21.0.5+11\n", "temurin", "temurin-21.0.5+11", ""},
		{".java-version", "temurin-21\n", "temurin", "temurin-21.0.6+7", "floating"},
		{".java-version", "1.8\n", "temurin", "temurin-8.0.504+1", "floating"},
		{".tool-versions", "nodejs 22.1.0\njava temurin-21.0.5+11\n", "", "temurin-21.0.5+11", ""},
		{".tool-versions", "java zulu-21.0.5\n", "temurin", "", "distribution zulu"},
		{".tool-versions", "java openjdk-21.0.5+11\n", "temurin", "temurin-21.0.5+11", ""}, // unknown vendor: the input's
		{".sdkmanrc", "java=21.0.5-tem\n", "", "", "newest build of 21.0.5"},
		{".sdkmanrc", "java=21.0.5-amzn\n", "temurin", "", "distribution corretto"},
		{".java-version", "", "temurin", "", "couldn't read"},
	} {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, c.file), []byte(c.content), 0o644)
		with := "{java-version-file: " + c.file
		if c.dist != "" {
			with += ", distribution: " + c.dist
		}
		j := javaJob(t, d, "      - uses: actions/setup-java@v5\n        with: "+with+", check-latest: true}\n")
		reasons := strings.Join(j.Reasons, "; ")
		if j.Proposal == nil || j.Proposal.Runtimes["java"] != c.pin || (c.review == "") != (j.Verdict != Review) || !strings.Contains(reasons, c.review) {
			t.Errorf("%s %q: pin %v, %s %q", c.file, c.content, j.Proposal.Runtimes, j.Verdict, reasons)
		}
	}
}

func TestTemurinReleases(t *testing.T) {
	fakeJavaReleases(t)
	// The union of both arches, newest first by semver then build (a rebuild's +101 beats +8).
	for spec, want := range map[string]string{"temurin-21": "temurin-21.0.6+7", "temurin-17.0": "temurin-17.0.20+101", "temurin-8": "temurin-8.0.504+1", "temurin-11": ""} {
		if got, err := Resolve("java", spec); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", spec, got, err, want)
		}
	}
	if got, _ := Resolve("gradle", "current"); got != "9.8.2" {
		t.Errorf("gradle current: %q", got)
	}
}

func TestSetupGradleTranslated(t *testing.T) {
	fakeJavaReleases(t)
	pre := "      - uses: actions/setup-java@v5\n        with: {distribution: temurin, java-version: \"21.0.5+11\"}\n"
	for _, c := range []struct {
		uses, with, pin, review, body string
	}{
		{"gradle/actions/setup-gradle@v4", "", "", "", "gradle-wrapper.jar"},
		{"gradle/actions/setup-gradle@v4", "{validate-wrappers: false}", "", "", ""},
		{"gradle/actions/setup-gradle@v4", "{gradle-version: 8.14.3}", "8.14.3", "", "gradle-wrapper.jar"},
		{"gradle/actions/setup-gradle@v4", "{gradle-version: current}", "9.8.2", "add floating: {gradle: \"current\"}", ""},
		{"gradle/actions/setup-gradle@v4", "{gradle-version: release-candidate}", "", "release candidates", ""},
		{"gradle/actions/setup-gradle@v4", "{develocity-injection-enabled: true}", "", "Develocity", ""},
		{"gradle/gradle-build-action@v2", "{arguments: build --scan, build-root-directory: app}", "", "", "./gradlew build --scan"},
		{"gradle/gradle-build-action@v2", "{arguments: \"build -Pv=$X\"}", "", "write them as a run step", ""},
		{"gradle/actions/wrapper-validation@v4", "", "", "", "gradle-wrapper.jar"},
	} {
		step := "      - uses: " + c.uses + "\n"
		if c.with != "" {
			step += "        with: " + c.with + "\n"
		}
		j := javaJob(t, "", pre+step)
		reasons := strings.Join(j.Reasons, "; ")
		if j.Proposal == nil || j.Proposal.Runtimes["gradle"] != c.pin || (c.review == "") != (j.Verdict != Review) || !strings.Contains(reasons, c.review) ||
			!strings.Contains(j.Proposal.Body, c.body) {
			t.Errorf("%s %s: pin %v, %s %q\n%s", c.uses, c.with, j.Proposal.Runtimes, j.Verdict, reasons, j.Proposal.Body)
		}
		if c.with == "{validate-wrappers: false}" && strings.Contains(j.Proposal.Body, "gradle-wrapper.jar") {
			t.Errorf("validate-wrappers: false still validates")
		}
	}
	j := javaJob(t, "", pre+"      - uses: gradle/gradle-build-action@v2\n        with: {arguments: build, build-root-directory: app}\n")
	if b := j.Proposal.Body; !strings.Contains(b, "cd app") || strings.Index(b, "gradle-wrapper.jar") > strings.Index(b, "./gradlew build") {
		t.Errorf("wrapper check must run before the build, in the repo root; the build in app:\n%s", b)
	}
}
