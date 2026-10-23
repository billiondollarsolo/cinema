package main

import (
	"errors"
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
			t.Errorf("%q on %s: %s, want %s", c.j.Proposal, c.m.Platform, got, c.want)
		}
	}
}

func TestJobKeyMatrixOrder(t *testing.T) {
	j := analyze.Job{Workflow: "ci.yml", ID: "test", Name: "test", Combo: map[string]string{"os": "ubuntu-latest", "node": "22.x"}}
	if analyzedKey(j) != jobKey("ci.yml", "test (ubuntu-latest, 22.x)") {
		t.Fatal("matrix job doesn't match Actions' default name")
	}
	n := analyze.Job{Workflow: "ci.yml", ID: "test_go", Name: "Go tests"}
	if analyzedKey(n) != jobKey("ci.yml", "Go tests") {
		t.Fatal("named job doesn't match")
	}
}
