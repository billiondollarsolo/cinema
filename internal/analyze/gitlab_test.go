package analyze

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// glFixture writes files into a temp repo (gitlab.com/group/proj, default
// branch main) and analyzes its .gitlab-ci.yml.
func glFixture(t *testing.T, files map[string]string) map[string]Job {
	t.Helper()
	old := gitlabRepo
	t.Cleanup(func() { gitlabRepo = old })
	gitlabRepo = func(string) GitLabRepo {
		return GitLabRepo{Host: "gitlab.com", Path: "group/proj", DefaultBranch: "main"}
	}
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := GitLab(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Job{}
	for _, j := range jobs {
		got[j.Name] = j
	}
	return got
}

func reasons(j Job) string { return strings.Join(j.Reasons, " | ") }

const glCI = `
stages: [build, test, deploy]
variables:
  GOFLAGS: -mod=readonly
  DB_URL: postgres://u:p@postgres/$DB_NAME
  DB_NAME: app
default:
  image: golang:1.26
  before_script: [go version]
  tags: [saas-linux-small-amd64]
include:
  - local: ci/lint.yml
.test-base:
  stage: test
  variables: {CGO_ENABLED: "0"}
  script: [go test ./...]
  after_script: [echo done]
unit:
  extends: .test-base
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
    - if: $CI_COMMIT_BRANCH
integration:
  extends: [.test-base]
  services: [postgres:16]
  script:
    - !reference [.setup, script]
    - go test -tags integration ./...
.setup:
  script: [make db]
matrix:
  stage: test
  image: node:$NODE
  parallel:
    matrix:
      - NODE: ["20", "22"]
        OS: [linux]
  script: [npm ci, npm test]
split:
  parallel: 3
  script: [./run-shard.sh]
deploy:
  stage: deploy
  environment: production
  script: [./deploy.sh]
release-tag:
  rules: [{if: $CI_COMMIT_TAG}]
  script: [make release]
manual:
  when: manual
  script: [make bench]
mr-only:
  only: [merge_requests]
  script: [make check]
uses-secret:
  script: ['curl -H "token: $API_TOKEN" https://example.com']
uses-project-var:
  script: [./check.sh --level $CHECK_LEVEL]
changes:
  rules: [{changes: [go.mod]}]
  script: [go mod tidy -diff]
remote-trigger:
  trigger: {project: group/other}
alpine:
  image: {name: alpine:3.20, entrypoint: [""]}
  script: [apk add make, make]
  tags: [my-runner]
gpu:
  tags: [saas-linux-medium-amd64-gpu-standard]
  script: [nvidia-smi]
needs-build:
  stage: test
  needs: [build]
  script: [ls dist]
`

const glLint = `
lint:
  stage: test
  script: [golangci-lint run]
  timeout: 1h 30m
`

func TestGitLabAnalyze(t *testing.T) {
	got := glFixture(t, map[string]string{".gitlab-ci.yml": glCI, "ci/lint.yml": glLint})
	cases := []struct {
		name, verdict, reason string
	}{
		{"unit", Task, ""},
		{"integration", Task, ""},
		{"matrix: [20, linux]", Task, ""},
		{"matrix: [22, linux]", Task, ""},
		{"split 2/3", Task, ""},
		{"lint", Task, ""},
		{"deploy", Skip, "environment"},
		{"release-tag", Skip, "doesn't run in merge request or branch pipelines"},
		{"manual", Skip, "manual job"},
		{"mr-only", Task, ""},
		{"uses-secret", Skip, "uses a secret ($API_TOKEN"},
		{"uses-project-var", Review, "$CHECK_LEVEL"},
		{"changes", Review, "changes:"},
		{"remote-trigger", Skip, "downstream pipeline"},
		{"alpine", Review, "tagged my-runner"},
		{"gpu", Skip, "GPU"},
		{"needs-build", Review, "needs build"},
	}
	for _, c := range cases {
		j, ok := got[c.name]
		if !ok {
			t.Errorf("%s: not analyzed (have %v)", c.name, slices.Sorted(func(yield func(string) bool) {
				for k := range got {
					if !yield(k) {
						return
					}
				}
			}))
			continue
		}
		if j.Verdict != c.verdict || !strings.Contains(reasons(j), c.reason) {
			t.Errorf("%s: %s (%s), want %s (%s)", c.name, j.Verdict, reasons(j), c.verdict, c.reason)
		}
	}
	for _, n := range []string{".test-base", ".setup"} {
		if _, ok := got[n]; ok {
			t.Errorf("hidden job %s analyzed", n)
		}
	}

	unit := got["unit"].Proposal
	if unit == nil {
		t.Fatal("unit: no proposal")
	}
	if unit.Container != "golang:1.26" || unit.CPUs != 2 || unit.MemoryGB != 8 || unit.TimeoutMinutes != 60 {
		t.Errorf("unit: %+v", unit)
	}
	for _, want := range []string{"export CGO_ENABLED=0\n", "export GOFLAGS=-mod=readonly\n", "go version\ngo test ./...", "CI_JOB_NAME=unit"} {
		if !strings.Contains(unit.Body, want) {
			t.Errorf("unit body lacks %q:\n%s", want, unit.Body)
		}
	}
	if strings.Contains(unit.Body, "echo done") {
		t.Errorf("after_script is in the task:\n%s", unit.Body)
	}
	if unit.Event != "" {
		t.Errorf("unit: event %q, but its scripts don't depend on it", unit.Event)
	}
	// Variables referencing variables are expanded, as GitLab does, in any order.
	if !strings.Contains(unit.Body, "export DB_URL=postgres://u:p@postgres/app\n") {
		t.Errorf("DB_URL not expanded:\n%s", unit.Body)
	}
	integ := got["integration"].Proposal
	if !strings.Contains(integ.Body, "make db\ngo test -tags integration ./...") {
		t.Errorf("!reference not inlined:\n%s", integ.Body)
	}
	pg, ok := integ.Services["postgres"]
	if !ok || pg.Image != "postgres:16" || pg.Env["DB_NAME"] != "app" || pg.Env["DB_URL"] != "postgres://u:p@postgres/app" {
		t.Errorf("services: %+v", integ.Services)
	}
	m20 := got["matrix: [20, linux]"]
	if m20.Proposal.Container != "node:20" || m20.Combo["NODE"] != "20" || !slices.Equal(m20.MatrixKeys, []string{"NODE", "OS"}) ||
		m20.Proposal.ID != "matrix-20-linux" {
		t.Errorf("matrix leg: %+v %+v", m20, m20.Proposal)
	}
	if !strings.Contains(got["split 2/3"].Proposal.Body, "export CI_NODE_INDEX=2 CI_NODE_TOTAL=3") {
		t.Errorf("parallel: %s", got["split 2/3"].Proposal.Body)
	}
	if got["lint"].Proposal.TimeoutMinutes != 90 {
		t.Errorf("lint timeout %d", got["lint"].Proposal.TimeoutMinutes)
	}
	if got["alpine"].Proposal == nil || got["alpine"].Proposal.Container != "alpine:3.20" {
		t.Errorf("alpine: %+v", got["alpine"].Proposal)
	}
	if got["unit"].LegDigest() == got["integration"].LegDigest() || got["matrix: [20, linux]"].LegDigest() == got["matrix: [22, linux]"].LegDigest() {
		t.Error("distinct jobs share a digest")
	}
}

// The job digest covers the job's resolved definition: a change in a template
// it extends, a default:, a global variable or an included file is a new job;
// a change to another job isn't.
func TestGitLabDigest(t *testing.T) {
	base := map[string]string{".gitlab-ci.yml": glCI, "ci/lint.yml": glLint}
	d0 := glFixture(t, base)
	edit := func(file, old, new string) map[string]Job {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		if !strings.Contains(m[file], old) {
			t.Fatalf("%q not in %s", old, file)
		}
		m[file] = strings.Replace(m[file], old, new, 1)
		return glFixture(t, m)
	}
	same := func(a, b map[string]Job, job string) bool { return a[job].LegDigest() == b[job].LegDigest() }
	if d1 := glFixture(t, base); !same(d0, d1, "unit") {
		t.Error("digest isn't stable")
	}
	for _, c := range []struct {
		file, old, new, job string
		changes             bool
	}{
		{".gitlab-ci.yml", "CGO_ENABLED: \"0\"", "CGO_ENABLED: \"1\"", "unit", true},               // the template it extends
		{".gitlab-ci.yml", "before_script: [go version]", "before_script: [go env]", "unit", true}, // default:
		{".gitlab-ci.yml", "GOFLAGS: -mod=readonly", "GOFLAGS: -mod=mod", "unit", true},            // a global variable
		{".gitlab-ci.yml", "- if: $CI_COMMIT_BRANCH\n", "- if: $CI_COMMIT_BRANCH\n      when: on_success\n", "unit", true},
		{".gitlab-ci.yml", "script: [make bench]", "script: [make bench2]", "unit", false}, // another job
		{"ci/lint.yml", "golangci-lint run", "golangci-lint run --fix", "lint", true},      // an included file
		{"ci/lint.yml", "golangci-lint run", "golangci-lint run --fix", "unit", false},
		{".gitlab-ci.yml", "script: [make db]", "script: [make db2]", "integration", true}, // a !reference target
	} {
		if d := edit(c.file, c.old, c.new); same(d0, d, c.job) == c.changes {
			t.Errorf("%s → %s: %s's digest changed=%v, want %v", c.old, c.new, c.job, !c.changes, c.changes)
		}
	}
	// Setup hides a covered job (.cinema:unit); it's still the same job.
	hidden := edit(".gitlab-ci.yml", "\nunit:\n", "\n.cinema:unit:\n")
	if !same(d0, hidden, "unit") {
		t.Error("a job hidden by setup reads differently")
	}
}

func TestGitLabRulesAndEvents(t *testing.T) {
	got := glFixture(t, map[string]string{".gitlab-ci.yml": `
image: alpine:3.20
workflow:
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
    - if: $CI_COMMIT_BRANCH && $CI_OPEN_MERGE_REQUESTS
      when: never
    - if: $CI_COMMIT_BRANCH
plain:
  script: [make test]
reads-branch:
  script: [echo $CI_COMMIT_REF_NAME]
default-branch-only:
  rules: [{if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH}]
  script: [make publish-docs]
not-main:
  rules: [{if: $CI_COMMIT_BRANCH != "main"}]
  script: [make test]
mr-vars:
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      variables: {MODE: quick}
    - when: on_success
  script: [make test MODE=$MODE]
project-var:
  rules: [{if: $RUN_SLOW == "1"}]
  script: [make slow]
regex:
  rules: [{if: '$CI_MERGE_REQUEST_LABELS =~ /slow/'}]
  script: [make slow]
except-main:
  except: [main]
  script: [make test]
only-vars:
  only:
    variables: [$CI_PIPELINE_SOURCE == "push"]
  script: [make test]
reads-commit:
  script: [echo $CI_COMMIT_SHA > sha.txt]
token:
  script: [git clone https://gitlab-ci-token:$CI_JOB_TOKEN@gitlab.com/group/dep.git]
exists-yes:
  rules: [{exists: [Makefile]}]
  script: [make]
exists-no:
  rules: [{exists: [Cargo.toml]}]
  script: [cargo test]
`, "Makefile": "all:\n"})
	cases := []struct{ name, verdict, reason, event string }{
		{"plain", Task, "", ""},
		{"reads-branch", Review, "CI_COMMIT_REF_NAME", "push"}, // no rules: branch pipelines only
		{"default-branch-only", Skip, "doesn't run in merge request or branch", ""},
		{"not-main", Task, "", ""},
		{"mr-vars", Task, "", "pull_request"}, // the rules set different variables per event
		{"project-var", Review, "can't evaluate", ""},
		{"regex", Review, "can't evaluate", ""},
		{"except-main", Task, "", ""},
		{"only-vars", Task, "", ""},
		{"reads-commit", Review, "CI_COMMIT_SHA", ""},
		{"token", Skip, "CI credentials", ""},
		{"exists-yes", Task, "", ""},
		{"exists-no", Skip, "doesn't run", ""},
	}
	for _, c := range cases {
		j := got[c.name]
		ev := ""
		if j.Proposal != nil {
			ev = j.Proposal.Event
		}
		if j.Verdict != c.verdict || !strings.Contains(reasons(j), c.reason) || ev != c.event {
			t.Errorf("%s: %s (%s) event %q, want %s (%s) event %q", c.name, j.Verdict, reasons(j), ev, c.verdict, c.reason, c.event)
		}
	}
	if b := got["mr-vars"].Proposal.Body; !strings.Contains(b, "export MODE=quick") || !strings.Contains(b, "export CI_PIPELINE_SOURCE=merge_request_event") {
		t.Errorf("mr-vars body:\n%s", b)
	}
}

func TestGitLabIncludesAndErrors(t *testing.T) {
	got := glFixture(t, map[string]string{".gitlab-ci.yml": `
include:
  - template: Security/SAST.gitlab-ci.yml
  - local: ci/*.yml
image: alpine:3.20
cycle-a: {extends: cycle-b, script: [a]}
cycle-b: {extends: cycle-a, script: [b]}
missing: {extends: .nope, script: [c]}
badref:
  script: [!reference [.nope, script]]
`, "ci/a.yml": "from-a:\n  image: alpine:3.20\n  script: [make a]\n", "ci/b.yml": "from-b:\n  image: alpine:3.20\n  script: [make b]\n"})
	for _, n := range []string{"from-a", "from-b"} {
		if j := got[n]; j.Verdict != Review || !strings.Contains(reasons(j), "includes template") {
			t.Errorf("%s: %s (%s)", n, j.Verdict, reasons(j))
		}
	}
	for _, n := range []string{"cycle-a", "missing", "badref"} {
		if j := got[n]; j.Verdict != Review || !strings.Contains(reasons(j), "can't be resolved") || j.Proposal != nil {
			t.Errorf("%s: %s (%s)", n, j.Verdict, reasons(j))
		}
	}
}

func TestGitLabMergeSemantics(t *testing.T) {
	got := glFixture(t, map[string]string{".gitlab-ci.yml": `
include: [{local: base.yml}]
.a: &a
  image: alpine:3.20
  variables: {X: a, Y: a}
  script: [echo a]
.b:
  extends: .a
  variables: {Y: b, Z: b}
job:
  extends: .b
  <<: *a
  variables: {Z: job}
override:
  script: [echo main]
`, "base.yml": "override:\n  image: alpine:3.19\n  script: [echo base]\n  variables: {FROM_BASE: \"1\"}\n"})
	b := got["job"].Proposal.Body
	for _, want := range []string{"export X=a\n", "export Y=b\n", "export Z=job\n", "echo a"} {
		if !strings.Contains(b, want) {
			t.Errorf("job lacks %q:\n%s", want, b)
		}
	}
	o := got["override"].Proposal
	if o.Container != "alpine:3.19" || !strings.Contains(o.Body, "echo main") || strings.Contains(o.Body, "echo base") || !strings.Contains(o.Body, "FROM_BASE=1") {
		t.Errorf("include merge: %s\n%s", o.Container, o.Body)
	}
}

// GitLab hands every job the artifacts of earlier stages unless it says
// otherwise (needs:, dependencies:).
func TestGitLabArtifacts(t *testing.T) {
	got := glFixture(t, map[string]string{".gitlab-ci.yml": `
image: alpine:3.20
build:
  stage: build
  script: [make build]
  artifacts: {paths: [dist/, "*.log"]}
test:
  script: [ls dist]
test-isolated:
  dependencies: []
  script: [make test]
test-needs-nothing:
  needs: []
  script: [make test]
test-order-only:
  needs: [{job: build, artifacts: false}]
  script: [make test]
`})
	if !slices.Equal(got["build"].Proposal.Outputs, []string{"dist"}) {
		t.Errorf("build outputs %v", got["build"].Proposal.Outputs)
	}
	if j := got["test"]; j.Verdict != Review || !strings.Contains(reasons(j), "earlier-stage jobs build") {
		t.Errorf("test: %s (%s)", j.Verdict, reasons(j))
	}
	for _, n := range []string{"test-isolated", "test-needs-nothing", "test-order-only"} {
		if j := got[n]; j.Verdict != Task {
			t.Errorf("%s: %s (%s)", n, j.Verdict, reasons(j))
		}
	}
}

func TestGitLabDuration(t *testing.T) {
	for in, want := range map[string]int{"1h 30m": 90, "90 minutes": 90, "3600": 60, "2 hours and 10 minutes": 130, "45s": 1, "1d": 1440} {
		if got, ok := glDuration(in); !ok || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, ok, want)
		}
	}
	if _, ok := glDuration("soon"); ok {
		t.Error("soon parsed")
	}
}

func TestGitLabExpr(t *testing.T) {
	sc := glScenario{source: "push", defaultBranch: "main"}
	env := sc.env(map[string]glVal{"MODE": knownVal("fast")})
	for expr, want := range map[string]tri{
		`$CI_PIPELINE_SOURCE == "push"`:                        triTrue,
		`$CI_PIPELINE_SOURCE == "push" && $MODE == "slow"`:     triFalse,
		`$CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH`:              triFalse,
		`$CI_COMMIT_BRANCH != "main"`:                          triTrue,
		`$CI_COMMIT_BRANCH == "develop"`:                       triUnknown,
		`$CI_COMMIT_TAG`:                                       triFalse,
		`$CI_COMMIT_TAG == null`:                               triTrue,
		`$MODE =~ /^FA/i`:                                      triTrue,
		`$MODE !~ /^fa/`:                                       triFalse,
		`($CI_MERGE_REQUEST_ID || $MODE) && $MODE != "x"`:      triTrue,
		`$SOMETHING_FROM_SETTINGS == "1" || $MODE == "fast"`:   triTrue,
		`$SOMETHING_FROM_SETTINGS == "1" && $MODE == "fast"`:   triUnknown,
		`$CI_PIPELINE_SOURCE == "push" garbage`:                triUnknown,
		`$CI_COMMIT_MESSAGE =~ /\[skip tests\]/`:               triUnknown,
		`$CI_COMMIT_BRANCH && $CI_OPEN_MERGE_REQUESTS == null`: triTrue,
	} {
		if got := glExpr(expr, env); got != want {
			t.Errorf("%s: %d, want %d", expr, got, want)
		}
	}
}

// A .gitlab-ci.yml cinema can't read never breaks the GitHub side of a repo.
func TestRepoWithBrokenGitLabFile(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o755)
	os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte("on: [pull_request]\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps: [{run: make}]\n"), 0o644)
	os.WriteFile(filepath.Join(root, GitLabFile), []byte("include: [{local: missing.yml}]\n"), 0o644)
	jobs, err := Repo(root)
	if err != nil || len(jobs) != 2 || jobs[0].Proposal == nil || jobs[1].Verdict != Review {
		t.Fatalf("%v %+v", err, jobs)
	}
}
