package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/billiondollarsolo/cinema/internal/analyze"
)

func TestReadiness(t *testing.T) {
	defer func(f func(string) (string, error)) { lookPath = f }(lookPath)
	lookPath = func(name string) (string, error) {
		if name == "make" {
			return "/usr/bin/make", nil
		}
		return "", errors.New("missing")
	}
	linux := machine{Platform: "linux/amd64", Docker: false}
	task := func(body, image string) analyze.Job {
		return analyze.Job{Verdict: analyze.Task, Proposal: &analyze.Proposal{Body: body, Container: image}}
	}
	for _, c := range []struct {
		j    analyze.Job
		m    machine
		want string
	}{
		{task("make test", ""), linux, "ready"},
		{task("shellcheck *.sh && make", ""), linux, "needs shellcheck"},
		{task("go test ./...", "golang:1.27"), linux, "needs docker"},
		{task("buf generate", "golang:1.27"), machine{Platform: "linux/amd64", Docker: true}, "ready"}, // the image has it
		{analyze.Job{Verdict: analyze.Skip, Reasons: []string{"runs on macos-latest"}}, linux, "stays in CI"},
		{task("make test", ""), machine{Platform: "darwin/arm64"}, "stays in CI"},
	} {
		if got, _ := readiness(c.j, c.m); got != c.want {
			t.Errorf("%+v on %s: %s, want %s", c.j.Proposal, c.m.Platform, got, c.want)
		}
	}
}

// Lab (gradle-versions-plugin, gson, from a Mac): a sandboxed task finds the
// runner image's tools and its pinned runtimes, not the host's.
func TestMissingRunnerToolsSandbox(t *testing.T) {
	defer func(f func(string) (string, error)) { lookPath = f }(lookPath)
	lookPath = func(name string) (string, error) {
		if name == "mvn" || name == "shellcheck" {
			return "/opt/homebrew/bin/" + name, nil // the Mac has them; the runner image doesn't
		}
		return "", errors.New("missing")
	}
	java := map[string]string{"java": "temurin-21.0.5+11", "gradle": "8.4"}
	for _, c := range []struct {
		script   string
		runtimes map[string]string
		sandbox  bool
		want     []string
	}{
		{"mvn verify", java, true, []string{"mvn"}},
		{"mvn verify", java, false, nil},
		{"gradle dU && java -version && make", java, true, nil},
		{"gradle dU", nil, false, []string{"gradle"}},
		{"shellcheck x.sh && jq . f", nil, true, []string{"shellcheck"}},
	} {
		if got := missingRunnerTools(c.script, c.runtimes, c.sandbox); !slices.Equal(got, c.want) {
			t.Errorf("%q (sandbox %v): %q, want %q", c.script, c.sandbox, got, c.want)
		}
	}
}

func TestJobKeyMatrixOrder(t *testing.T) {
	j := analyze.Job{Workflow: "ci.yml", ID: "test", Name: "test", Combo: map[string]string{"os": "ubuntu-latest", "node": "22.x"}}
	if analyzedKey(j) != jobKey("ci.yml", "test (ubuntu-latest, 22.x)") {
		t.Fatal("matrix job doesn't match Actions' default name")
	}
	named := analyze.Job{Workflow: "ci.yml", ID: "test", Name: "Test", Combo: map[string]string{"os": "ubuntu-latest", "node": "20.x", "flags": ""}}
	if analyzedKey(named) != jobKey("ci.yml", "Test (ubuntu-latest, 20.x)") {
		t.Fatal("named matrix job doesn't match Actions' name with values appended")
	}
	withValue := analyze.Job{Workflow: "ci.yml", ID: "test", Name: "Node 20.x", Combo: map[string]string{"node": "20.x"}}
	if analyzedKey(withValue) != jobKey("ci.yml", "Node 20.x") {
		t.Fatal("name that uses a matrix value got values appended")
	}
	n := analyze.Job{Workflow: "ci.yml", ID: "test_go", Name: "Go tests"}
	if analyzedKey(n) != jobKey("ci.yml", "Go tests") {
		t.Fatal("named job doesn't match")
	}
}

func TestRequiredLegs(t *testing.T) {
	old := ghAPI
	defer func() { ghAPI = old }()
	ghAPI = func(p string) ([]byte, error) {
		switch p {
		case "repos/{owner}/{repo}":
			return []byte(`{"default_branch":"main"}`), nil
		case "repos/{owner}/{repo}/rules/branches/main":
			return []byte(`[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"Test (20.x, ubuntu-latest)"}]}}]`), nil
		}
		return nil, errors.New("HTTP 404") // classic protection: not an admin
	}
	req, err := requiredChecks()
	if err != nil || !slices.Equal(req, []string{"Test (20.x, ubuntu-latest)"}) {
		t.Fatalf("requiredChecks = %v, %v", req, err)
	}
	leg := func(node string) analyze.Job {
		return analyze.Job{Workflow: "ci.yml", Name: "Test", Combo: map[string]string{"os": "ubuntu-latest", "node": node}}
	}
	if !requiredLeg(leg("20.x"), req) || requiredLeg(leg("22.x"), req) {
		t.Fatal("only the 20.x leg is required")
	}
	if requiredLeg(analyze.Job{Workflow: "ci.yml", Name: "Test (20.x, ubuntu-latest)"}, req) {
		t.Fatal("a plain job that skips reports its check: not blocked")
	}
}
