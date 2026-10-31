package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// glPipelineEnv is a GitLab job's view of the project (the analyzer reads it).
func glPipelineEnv(t *testing.T) {
	t.Setenv("GITLAB_CI", "true")
	t.Setenv("CI_SERVER_HOST", "gitlab.com")
	t.Setenv("CI_PROJECT_PATH", "group/proj")
	t.Setenv("CI_DEFAULT_BRANCH", "main")
}

const glSetupCI = `# the project's pipeline
stages: [build, test]

default:
  image: golang:1.26

variables:
  GOFLAGS: -mod=readonly

.base:
  stage: test
  script: [go vet ./...]

unit:
  stage: test
  script:
    - go test ./...   # a comment stays

"lint go":
  extends: .base
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
    - if: $CI_COMMIT_BRANCH

compile:
  stage: build
  script: [go build ./...]

uses-compile:
  stage: test
  extends: compile
  script: [go build -race ./...]

deploy:
  stage: test
  environment: prod
  script: [./deploy.sh]
`

func glJobs(t *testing.T, root string, src []byte) []analyze.Job {
	t.Helper()
	jobs, err := analyze.GitLab(root, src)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func TestPlanGitLab(t *testing.T) {
	glPipelineEnv(t)
	root := t.TempDir()
	src := []byte(glSetupCI)
	all := glJobs(t, root, src)
	var ready []analyze.Job
	for _, j := range all {
		if j.Proposal != nil && j.Verdict == analyze.Task {
			ready = append(ready, j)
		}
	}
	p, err := planGitLab(root, src, all, ready, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.covered, ",") != "lint go,unit,uses-compile" {
		t.Fatalf("covered %v, left %v", p.covered, p.left)
	}
	if left := strings.Join(p.left, "\n"); !strings.Contains(left, "compile: uses-compile use it by name") {
		t.Errorf("left:\n%s", left)
	}
	out := string(p.out)
	// Only the covered keys change, and the block is appended.
	if !strings.HasPrefix(out, strings.NewReplacer("\nunit:\n", "\n.cinema:unit:\n", "\n\"lint go\":\n", "\n\".cinema:lint go\":\n", "\nuses-compile:\n", "\n.cinema:uses-compile:\n").Replace(glSetupCI)) {
		t.Fatalf("the rest of the file changed:\n%s", out)
	}
	for _, want := range []string{"cinema verify --gitlab-child .cinema-child.yml --trust-branch main\n", "cinema@v0.1.0", "strategy: depend",
		"- artifact: .cinema-child.yml", "stage: \"build\"", "stage: \"test\"", "GIT_DEPTH: \"0\""} {
		if !strings.Contains(out, want) {
			t.Errorf("setup's jobs lack %q:\n%s", want, out)
		}
	}
	var v map[string]any
	if err := yaml.Unmarshal(p.out, &v); err != nil {
		t.Fatal(err)
	}
	if _, ok := v["unit"]; ok {
		t.Error("unit is still a job of the parent")
	}
	// The analyzer reads the edited file as before: same jobs, same digests,
	// and neither cinema job is a job to draft.
	after := glJobs(t, root, p.out)
	digests := func(js []analyze.Job) string {
		var b strings.Builder
		for _, j := range js {
			fmt.Fprintf(&b, "%s=%s\n", j.Name, j.LegDigest())
		}
		return b.String()
	}
	if digests(after) != digests(all) {
		t.Fatalf("analysis changed:\n%s\nwas:\n%s", digests(after), digests(all))
	}
	// Undo restores the file byte-for-byte; a rerun starts from the original.
	if got := unsetupGitLab(p.out); string(got) != glSetupCI {
		t.Fatalf("undo:\n%s", got)
	}
	if got := unsetupGitLab(src); string(got) != glSetupCI {
		t.Fatal("undo changed an untouched file")
	}
	again, err := planGitLab(root, unsetupGitLab(p.out), all, ready, "v0.1.0")
	if err != nil || string(again.out) != out {
		t.Fatalf("rerun differs: %v", err)
	}
}

func TestPlanGitLabRefuses(t *testing.T) {
	glPipelineEnv(t)
	for name, src := range map[string]string{
		"remote include": "include: [{remote: 'https://example.com/ci.yml'}]\nunit:\n  image: alpine:3.20\n  script: [true]\n",
		"own cinema job": "cinema:\n  image: alpine:3.20\n  script: [true]\nunit:\n  image: alpine:3.20\n  script: [true]\n",
	} {
		all := glJobs(t, "", []byte(src))
		var ready []analyze.Job
		for _, j := range all {
			if j.Proposal != nil {
				ready = append(ready, j)
			}
		}
		if p, err := planGitLab("", []byte(src), all, ready, "v1"); err == nil && len(p.covered) > 0 {
			t.Errorf("%s: set up anyway:\n%s", name, p.out)
		}
	}
}

// The cinema jobs run in the pipelines the project already has: without
// workflow: rules and with no job in merge request pipelines, they mustn't
// create merge request pipelines of their own (GitLab's lint warns of it).
func TestPlanGitLabPipelines(t *testing.T) {
	glPipelineEnv(t)
	for src, wantRules := range map[string]bool{
		"unit:\n  image: alpine:3.20\n  script: [make]\n": false,
		"unit:\n  image: alpine:3.20\n  script: [make]\n  rules: [{if: $CI_PIPELINE_SOURCE == \"merge_request_event\"}, {if: $CI_COMMIT_BRANCH}]\n":            true,
		"workflow:\n  rules: [{if: $CI_PIPELINE_SOURCE == \"merge_request_event\"}, {if: $CI_COMMIT_BRANCH}]\nunit:\n  image: alpine:3.20\n  script: [make]\n": false, // no job runs in MR pipelines, so there are none
	} {
		all := glJobs(t, "", []byte(src))
		p, err := planGitLab("", []byte(src), all, all, "v1")
		if err != nil || len(p.covered) != 1 {
			t.Fatalf("%v %v", err, p.left)
		}
		block := string(p.out[strings.Index(string(p.out), glBegin):])
		if got := strings.Count(block, "rules: [{when: on_success}]"); (got == 2) != wantRules || (got != 0 && got != 2) {
			t.Errorf("%s: %d rules lines:\n%s", src, got, block)
		}
	}
}

func TestGitLabEvent(t *testing.T) {
	env := func(kv ...string) func(string) (string, bool) {
		m := map[string]string{"GITLAB_CI": "true", "CI_SERVER_HOST": "gitlab.example.com", "CI_PROJECT_PATH": "Group/Sub/Proj", "CI_PROJECT_ID": "7"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	sha := func(c string) string { return strings.Repeat(c, 40) }
	e, err := gitlabEvent(env("CI_PIPELINE_SOURCE", "push"), "", "c0")
	if err != nil || e.name != "push" || e.base != "c0" || e.repo != "gitlab.example.com/group/sub/proj" || e.allRun != "" {
		t.Fatalf("push: %+v %v", e, err)
	}
	e, _ = gitlabEvent(env("CI_PIPELINE_SOURCE", "merge_request_event", "CI_MERGE_REQUEST_EVENT_TYPE", "merged_result",
		"CI_MERGE_REQUEST_TARGET_BRANCH_SHA", sha("b"), "CI_MERGE_REQUEST_SOURCE_BRANCH_SHA", sha("1"),
		"CI_MERGE_REQUEST_PROJECT_ID", "7", "CI_MERGE_REQUEST_SOURCE_PROJECT_ID", "7"), "", sha("2"))
	if e.name != "pull_request" || e.base != sha("b") || e.head != sha("1") || !e.change || e.fork {
		t.Fatalf("merged result: %+v", e)
	}
	e, _ = gitlabEvent(env("CI_PIPELINE_SOURCE", "merge_request_event", "CI_MERGE_REQUEST_EVENT_TYPE", "detached",
		"CI_MERGE_REQUEST_DIFF_BASE_SHA", sha("d"), "CI_MERGE_REQUEST_PROJECT_ID", "7", "CI_MERGE_REQUEST_SOURCE_PROJECT_ID", "9"), "", sha("3"))
	if e.base != sha("d") || e.head != sha("3") || !e.fork {
		t.Fatalf("fork MR: %+v", e)
	}
	// Project IDs that are missing (a pipeline variable can blank them) or
	// disagree with the project the job runs in count as a fork.
	for _, kv := range [][]string{{}, {"CI_MERGE_REQUEST_PROJECT_ID", "7"}, {"CI_MERGE_REQUEST_PROJECT_ID", "9", "CI_MERGE_REQUEST_SOURCE_PROJECT_ID", "9"}} {
		e, _ = gitlabEvent(env(append([]string{"CI_PIPELINE_SOURCE", "merge_request_event", "CI_MERGE_REQUEST_TARGET_BRANCH_SHA", sha("b")}, kv...)...), "", sha("4"))
		if !e.fork {
			t.Errorf("%v: not a fork", kv)
		}
	}
	e, _ = gitlabEvent(env("CI_PIPELINE_SOURCE", "merge_request_event", "CI_MERGE_REQUEST_EVENT_TYPE", "merge_train",
		"CI_MERGE_REQUEST_TARGET_BRANCH_SHA", sha("b")), "", sha("5"))
	if e.name != "merge_group" {
		t.Fatalf("merge train: %+v", e)
	}
	// The SHAs reach git's command line: anything but a commit ID is refused.
	if _, err := gitlabEvent(env("CI_PIPELINE_SOURCE", "merge_request_event", "CI_MERGE_REQUEST_TARGET_BRANCH_SHA", "--output=/tmp/x"), "", sha("6")); err == nil {
		t.Error("took an option for the base commit")
	}
	for _, src := range []string{"web", "api", "trigger", "schedule", "pipeline"} {
		if e, _ := gitlabEvent(env("CI_PIPELINE_SOURCE", src), "", "c"); e.allRun == "" {
			t.Errorf("%s: judged, but it can carry pipeline variables", src)
		}
	}
	if _, err := gitlabEvent(func(string) (string, bool) { return "", false }, "", "c"); err == nil {
		t.Error("ran outside GitLab")
	}
}

// End to end: setup's edit, a signed local run, and the cinema job in a
// fresh clone (as GitLab's runner checks out) writing the child pipeline:
// the proven job is left out, the rest run, and nothing is empty.
func TestGitLabChildPipeline(t *testing.T) {
	g := newGateEnv(t)
	g.git("remote", "set-url", "origin", "https://gitlab.com/group/proj.git")
	g.git("config", "remote.origin.pushurl", g.bare)
	glPipelineEnv(t) // the analyzer here and in the gate sees the same project
	src := `stages: [build, test]
default:
  image: alpine:3.20
unit:
  stage: test
  script: [echo unit]
lint:
  stage: test
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
  script: [echo lint]
matrix:
  stage: test
  parallel:
    matrix:
      - V: ["1", "2"]
  script: [echo $V]
other:
  stage: build
  script: [echo other]
`
	all := glJobs(t, g.repo, []byte(src))
	var ready []analyze.Job
	for _, j := range all {
		if j.ID != "other" {
			ready = append(ready, j)
		}
	}
	p, err := planGitLab(g.repo, []byte(src), all, ready, "v0.1.0")
	if err != nil || len(p.covered) != 3 {
		t.Fatalf("%v %v %v", err, p.covered, p.left)
	}
	// Tasks for unit and matrix V=1 (graduated), by hand: the gate judges
	// receipts by job_digest, whatever the task runs.
	digest := map[string]string{}
	for _, j := range all {
		digest[j.Name] = j.LegDigest()
	}
	tasks := "schema_version: 1\ntasks:\n"
	for id, name := range map[string]string{"unit": "unit", "matrix-1": "matrix: [1]"} {
		job, _, _ := strings.Cut(name, ":")
		tasks += fmt.Sprintf("  %s:\n    argv: [sh, -c, \"true\"]\n    platform: any\n    covers: \".gitlab-ci.yml#%s\"\n    job_digest: %q\n"+
			"    graduated: {job_digest: %q, backtested: 3, at: 2026-10-30}\n    reuse: { allowed: true, max_age: 1h }\n", id, job, digest[name], digest[name])
	}
	g.write(".gitlab-ci.yml", string(p.out))
	g.write(".cinema/tasks.yaml", tasks)
	g.write(GitLabTrustFile, g.keys)
	g.git("add", "-A")
	g.git("commit", "-qm", "gitlab")
	if out, code := g.cinema("trust"); code != 0 {
		t.Fatalf("trust: %s", out)
	}
	g.cinema("run", "--commit", "HEAD", "unit", "matrix-1")
	g.git("push", "-q", g.bare, "HEAD:main")
	if out, code := g.cinema("push"); code != 0 {
		t.Fatalf("push: %s", out)
	}
	trust := "main" // the cinema job's --trust-branch
	child := func(source string, extra ...string) (map[string]any, string) {
		ci := filepath.Join(t.TempDir(), "ci")
		mustRun(t, "", "git", "clone", "-q", g.bare, ci)
		args := []string{"verify", "--gitlab-child", ".cinema-child.yml"}
		if trust != "" {
			args = append(args, "--trust-branch", trust)
		}
		cmd := exec.Command(bin, args...)
		cmd.Dir = ci
		cmd.Env = append(append(os.Environ(), "CI_PIPELINE_SOURCE="+source, "CINEMA_DATA_DIR="+t.TempDir(),
			"CI_MERGE_REQUEST_TARGET_BRANCH_NAME=main", "CI_PROJECT_ID=7", "CI_MERGE_REQUEST_PROJECT_ID=7", "CI_MERGE_REQUEST_SOURCE_PROJECT_ID=7"), extra...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		b, err := os.ReadFile(filepath.Join(ci, ".cinema-child.yml"))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := yaml.Unmarshal(b, &v); err != nil {
			t.Fatalf("%v\n%s", err, b)
		}
		return v, string(out) + "\n" + string(b)
	}
	v, log := child("push")
	if _, ok := v["unit"]; ok {
		t.Errorf("the proven job runs:\n%s", log)
	}
	if _, ok := v["lint"]; ok {
		t.Errorf("lint runs only in merge request pipelines, yet it's in a push's child:\n%s", log)
	}
	m, _ := v["matrix"].(map[string]any)
	if b, _ := json.Marshal(m["parallel"]); string(b) != `{"matrix":[{"V":"2"}]}` || m["extends"] != ".cinema:matrix" {
		t.Errorf("matrix: %s\n%s", b, log)
	}
	if o, _ := v["other"].(map[string]any); fmt.Sprint(o["rules"]) != "[map[when:never]]" {
		t.Errorf("the parent's own job runs in the child too:\n%s", log)
	}
	if fmt.Sprint(v["include"]) != "[map[local:.gitlab-ci.yml]]" {
		t.Errorf("include: %v", v["include"])
	}
	v, log = child("merge_request_event")
	if l, _ := v["lint"].(map[string]any); l == nil || fmt.Sprint(l["rules"]) != "[map[when:on_success]]" {
		t.Errorf("lint in a merge request pipeline:\n%s", log)
	}
	if !regexp.MustCompile(`REUSE +unit`).MatchString(log) {
		t.Errorf("unit's receipt isn't reused in a merge request pipeline:\n%s", log)
	}
	// Another source: every task runs, so every hidden job is in the child.
	v, log = child("web")
	if _, ok := v["unit"]; !ok || !regexp.MustCompile(`pipeline source web`).MatchString(log) {
		t.Errorf("web pipeline:\n%s", log)
	}
	// The gate can't judge at all: still a child, running every covered job
	// (they're hidden in the parent; without it they'd never run).
	v, log = child("push", "CI_PROJECT_PATH=")
	if _, ok := v["unit"]; !ok || !strings.Contains(log, "every covered job runs") {
		t.Errorf("unjudgeable pipeline:\n%s", log)
	}

	// Pipeline variables can't name the trust root: CINEMA_TRUSTED_KEYS (as
	// git push -o ci.variable=… would set it) is ignored, and a trust branch
	// without the file means every task runs.
	main := mustRun(t, g.repo, "git", "rev-parse", "HEAD")
	g.git("rm", "-q", GitLabTrustFile)
	g.git("commit", "-qm", "no keys")
	g.git("push", "-q", g.bare, "HEAD:nokeys")
	g.write("side.txt", "x")
	g.git("add", "-A")
	g.git("commit", "-qm", "side")
	side := mustRun(t, g.repo, "git", "rev-parse", "HEAD")
	g.git("push", "-q", g.bare, "HEAD:side")
	g.git("reset", "-q", "--hard", main)
	trust = "nokeys"
	v, log = child("push", "CINEMA_TRUSTED_KEYS="+g.keys)
	if _, ok := v["unit"]; !ok || !strings.Contains(log, "CINEMA_TRUSTED_KEYS is ignored on GitLab") || !strings.Contains(log, "nokeys isn't the project's default branch") {
		t.Errorf("keys from the environment, trust branch not the default:\n%s", log)
	}
	trust = "" // a cinema job from before --trust-branch
	if v, log = child("push", "CINEMA_TRUSTED_KEYS="+g.keys); v["unit"] == nil || !strings.Contains(log, "--trust-branch") {
		t.Errorf("no trust branch:\n%s", log)
	}
	// tasks.yaml comes from the trust branch's tip, like the keys: a pipeline
	// variable naming another target commit changes nothing.
	trust = "main"
	if _, log = child("merge_request_event", "CI_MERGE_REQUEST_TARGET_BRANCH_SHA="+side); !regexp.MustCompile(`REUSE +unit`).MatchString(log) {
		t.Errorf("tasks.yaml from the trust branch, whatever the variables say:\n%s", log)
	}
	// Variables that would steer the gate's git (another repository or object
	// store, config from the environment) are scrubbed: the decision is the same.
	if _, log = child("push", "GIT_DIR=/nonexistent", "GIT_OBJECT_DIRECTORY=/nonexistent", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=remote.origin.url", "GIT_CONFIG_VALUE_0=/nonexistent"); !regexp.MustCompile(`REUSE +unit`).MatchString(log) {
		t.Errorf("the gate followed the job's git variables:\n%s", log)
	}
	// The scrub hides GIT_* from git only: rules still see them as GitLab does.
	t.Setenv("GIT_FLAVOR", "x")
	snapBefore := jobEnv
	scrubGitEnv()
	if v, ok := jobEnv("GIT_FLAVOR"); !ok || v != "x" || os.Getenv("GIT_FLAVOR") != "" {
		t.Errorf("rules must see the job's GIT_* variables (%q %v), git mustn't (%q)", v, ok, os.Getenv("GIT_FLAVOR"))
	}
	jobEnv = snapBefore
	os.Unsetenv("GIT_CONFIG_NOSYSTEM")
	os.Unsetenv("GIT_CONFIG_GLOBAL")
	os.Unsetenv("GIT_TERMINAL_PROMPT")
	// Variables that would redirect git (another global config through HOME or
	// XDG_CONFIG_HOME, config from the environment) don't reach the trust
	// root's git: the keys are still main's, from the real remote.
	ci := filepath.Join(t.TempDir(), "ci")
	mustRun(t, "", "git", "clone", "-q", g.bare, ci)
	bogus := t.TempDir()
	os.WriteFile(filepath.Join(bogus, ".gitconfig"), []byte("[url \"/nonexistent/\"]\n\tinsteadOf = "+g.bare+"\n"), 0o644)
	for k, v := range map[string]string{"HOME": bogus, "XDG_CONFIG_HOME": bogus, "GIT_CONFIG_COUNT": "1",
		"GIT_CONFIG_KEY_0": "remote.origin.url", "GIT_CONFIG_VALUE_0": "/nonexistent", "GIT_SSH_COMMAND": "false"} {
		t.Setenv(k, v)
	}
	if keys, _, err := gitlabTrust(ci, "origin", "main"); err != nil || !strings.Contains(keys, strings.Fields(g.keys)[1]) {
		t.Errorf("the trust root followed the job's git variables: %v %q", err, keys)
	}
}

// With every covered job proven, the child still has a job: GitLab refuses
// to create an empty one.
func TestGitLabChildNeverEmpty(t *testing.T) {
	glPipelineEnv(t)
	root := t.TempDir()
	src := "stages: [test]\n.cinema:unit:\n  image: alpine:3.20\n  script: [true]\n"
	os.WriteFile(filepath.Join(root, ".gitlab-ci.yml"), []byte(src), 0o644)
	gl, err := analyze.LoadGitLab(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := gl.Child(func(string) (string, bool) { return "", false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(c.YAML), "cinema:proven") || len(c.Runs) != 0 {
		t.Fatalf("%s", c.YAML)
	}
}

func TestGitLabForgeConclusion(t *testing.T) {
	old := glabAPI
	defer func() { glabAPI = old }()
	glabAPI = func(path string) ([]byte, error) {
		switch {
		case strings.HasPrefix(path, "projects/:id/pipelines?sha=abc"):
			return []byte(`[{"id": 11}, {"id": 10}]`), nil
		case path == "projects/:id/pipelines/11/jobs?per_page=100":
			return []byte(`[{"name": "lint", "status": "failed"}, {"name": "cinema", "status": "success"}]`), nil
		case path == "projects/:id/pipelines/11/bridges?per_page=100":
			return []byte(`[{"name": "cinema:child", "downstream_pipeline": {"id": 12}}]`), nil
		case path == "projects/:id/pipelines/12/jobs?per_page=100":
			return []byte(`[{"name": "matrix: [1]", "status": "success"}]`), nil
		}
		return []byte(`[]`), nil
	}
	f := forgeFor(analyze.GitLabFile)
	for name, want := range map[string]string{"lint": "failure", "matrix: [1]": "success", "nope": "none"} {
		if got, err := f.conclusion("abc", analyze.Job{Name: name, Workflow: analyze.GitLabFile}); err != nil || got.Conclusion != want {
			t.Errorf("%s: %+v %v, want %s", name, got, err, want)
		}
	}
	if got, _ := f.conclusion("zzz", analyze.Job{Name: "lint"}); got.Conclusion != "none" {
		t.Errorf("no pipeline: %+v", got)
	}
}

// An include cinema can't read can change any job, and no job digest covers
// its content: while there is one, the child pipeline runs every covered
// job, even one whose graduated task was reused.
func TestGitLabChildUnreadableInclude(t *testing.T) {
	glPipelineEnv(t)
	t.Setenv("CI_PIPELINE_SOURCE", "push")
	t.Setenv("CI_PIPELINE_ID", "") // no spot check
	for _, inc := range []string{"", "include: [{remote: 'https://example.com/ci.yml'}]\n"} {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, analyze.GitLabFile), []byte(inc+".cinema:unit:\n  image: alpine:3.20\n  script: [make test]\n"), 0o644)
		var d string
		for _, j := range glJobs(t, root, nil) {
			if j.ID == "unit" {
				d = j.LegDigest()
			}
		}
		if d == "" {
			t.Fatal("no digest for unit")
		}
		cfg := &task.Config{Tasks: map[string]*task.Task{"unit": {ID: "unit", Covers: analyze.GitLabFile + "#unit", JobDigest: d, Graduated: &task.Graduation{JobDigest: d}}}}
		rep := gateReport{Tasks: []gateTaskReport{{GateDecision: plan.GateDecision{Task: "unit", Decision: plan.GateReuse}}}}
		out := filepath.Join(root, "child.yml")
		if err := writeGitLabChild(cfg, rep, root, out); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(out)
		if runs := strings.Contains(string(b), `"unit":`); runs != (inc != "") {
			t.Errorf("%q: unit runs = %v:\n%s", inc, runs, b)
		}
	}
}
