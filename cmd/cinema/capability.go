package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// machine is what this box can take over from CI (the capability report).
type machine struct {
	Platform string `json:"platform"`
	CPUs     int    `json:"cpus"`
	Docker   bool   `json:"docker"`
	Buildx   bool   `json:"buildx"`
}

func probeMachine() machine {
	m := machine{Platform: task.Platform(), CPUs: runtime.NumCPU()}
	dockerCmd := func(args ...string) *exec.Cmd {
		c := exec.Command("docker", args...)
		c.Env = append(os.Environ(), task.DockerEnv()...)
		return c
	}
	m.Docker = dockerCmd("info").Run() == nil
	m.Buildx = m.Docker && dockerCmd("buildx", "version").Run() == nil
	return m
}

// runnerTools are tools on GitHub's runner image that CI scripts call directly
// and evidence machines often lack (lab: shellcheck, yq, xvfb, unzip).
var runnerTools = []string{"shellcheck", "yq", "jq", "xvfb-run", "unzip", "zip", "protoc", "buf", "make", "gcc", "g++", "cmake",
	"python3", "pip3", "ruby", "bundle", "java", "mvn", "gradle", "cargo", "kubectl", "kind", "helm", "terraform"}

var lookPath = exec.LookPath // a var for tests

// readiness says whether this machine can produce evidence for j now, and if
// not, what's missing. Category is one of: ready, needs <x>, review, stays in CI.
func readiness(j analyze.Job, m machine) (category, detail string) {
	switch {
	case j.Verdict == analyze.Skip:
		return "stays in CI", firstReason(j)
	case j.Proposal == nil:
		return "review", firstReason(j)
	}
	plat := "linux/amd64" // drafts are (ARM runners stay in CI)
	pl, ok := place(&task.Task{Platform: plat, Container: j.Proposal.Container})
	if !ok && m.Platform != plat {
		return "stays in CI", "the job is " + plat + "; this machine is " + m.Platform + ": " + placeHint(&task.Task{Platform: plat, Container: j.Proposal.Container})
	}
	var need []string
	if j.Proposal.Container != "" && !m.Docker {
		need = append(need, "docker")
	}
	if j.Proposal.Container == "" { // a job container's image has its own tools
		need = append(need, missingRunnerTools(j.Proposal.Body, j.Proposal.Runtimes, ok && pl.sandbox)...)
	}
	if len(need) > 0 {
		return "needs " + strings.Join(need, ", "), ""
	}
	if j.Verdict == analyze.Review {
		return "review", firstReason(j)
	}
	return "ready", ""
}

// runnerImageTools are the runnerTools cinema's runner image has (runner/Dockerfile).
var runnerImageTools = []string{"jq", "unzip", "zip", "make", "gcc", "g++", "python3", "mvn"}

// runtimeTools are runnerTools a pinned runtime puts on the task's PATH.
var runtimeTools = map[string]string{"java": "java", "gradle": "gradle", "mvn": "maven", "cargo": "rust", "python3": "python", "pip3": "python"}

// missingRunnerTools: the runner tools script calls that the task won't find.
// A task in a sandbox finds what the runner image has, not this machine's
// tools; a pinned runtime brings its own (lab: gradle-versions-plugin showed
// "needs gradle" with Gradle pinned; gson "needs mvn" whatever the host has).
func missingRunnerTools(script string, runtimes map[string]string, sandbox bool) []string {
	var need []string
	for _, t := range runnerTools {
		if !usesTool(script, t) || runtimes[runtimeTools[t]] != "" {
			continue
		}
		if sandbox {
			if !slices.Contains(runnerImageTools, t) {
				need = append(need, t)
			}
		} else if _, err := lookPath(t); err != nil {
			need = append(need, t)
		}
	}
	return need
}

func usesTool(script, tool string) bool {
	return regexp.MustCompile(`(^|[\s;&|(])` + regexp.QuoteMeta(tool) + `(\s|$)`).MatchString(script)
}

func firstReason(j analyze.Job) string {
	if len(j.Reasons) == 0 {
		return ""
	}
	return j.Reasons[0]
}

// jobMinutes is each job's median billed minutes (per job, rounded up, as
// GitHub bills) over the repo's recent PR runs, keyed by workflow file and
// display name. It uses the gh CLI; nil if that isn't available.
func jobMinutes(runs int) (map[string]float64, error) {
	out, err := exec.Command("gh", "api", fmt.Sprintf("repos/{owner}/{repo}/actions/runs?event=pull_request&status=completed&per_page=%d", runs)).Output()
	if err != nil {
		return nil, fmt.Errorf("gh api: %v (is gh installed and logged in?)", err)
	}
	var rs struct {
		Runs []struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, err
	}
	mins := map[string][]float64{}
	for _, r := range rs.Runs {
		out, err := exec.Command("gh", "api", fmt.Sprintf("repos/{owner}/{repo}/actions/runs/%d/jobs?per_page=100", r.ID)).Output()
		if err != nil {
			continue
		}
		var js struct {
			Jobs []struct {
				Name        string `json:"name"`
				Conclusion  string `json:"conclusion"`
				StartedAt   string `json:"started_at"`
				CompletedAt string `json:"completed_at"`
			} `json:"jobs"`
		}
		if json.Unmarshal(out, &js) != nil {
			continue
		}
		for _, j := range js.Jobs {
			d, ok := seconds(j.StartedAt, j.CompletedAt)
			if !ok || j.Conclusion == "skipped" || j.Conclusion == "cancelled" {
				continue
			}
			k := jobKey(path.Base(r.Path), j.Name)
			mins[k] = append(mins[k], math.Ceil(d/60))
		}
	}
	med := map[string]float64{}
	for k, v := range mins {
		sort.Float64s(v)
		med[k] = v[len(v)/2]
	}
	return med, nil
}

// jobKey matches an analyzed job to Actions' job names: a matrix job without a
// name: shows as "id (v1, v2)" in matrix order, which a map can't keep, so the
// values are compared as a set.
func jobKey(workflow, name string) string {
	if base, vals, ok := strings.Cut(name, " ("); ok && strings.HasSuffix(vals, ")") {
		parts := strings.Split(strings.TrimSuffix(vals, ")"), ", ")
		slices.Sort(parts)
		name = base + " (" + strings.Join(parts, ", ") + ")"
	}
	return workflow + "\x00" + name
}

// Actions appends a matrix leg's values to the job name unless the name
// already uses one (name: "Test" → "Test (ubuntu-latest, 20.x)").
func analyzedKey(j analyze.Job) string {
	name := j.Name
	usesValue := false
	for _, v := range j.Combo {
		usesValue = usesValue || (v != "" && strings.Contains(name, v))
	}
	if len(j.Combo) > 0 && !usesValue {
		var vals []string
		for _, v := range j.Combo {
			if v != "" { // empty values (a key only some legs set) aren't shown
				vals = append(vals, v)
			}
		}
		name += " (" + strings.Join(vals, ", ") + ")"
	}
	return jobKey(j.Workflow, name)
}

func seconds(start, end string) (float64, bool) {
	a, err1 := time.Parse(time.RFC3339, start)
	b, err2 := time.Parse(time.RFC3339, end)
	if err1 != nil || err2 != nil || b.Before(a) {
		return 0, false
	}
	return b.Sub(a).Seconds(), true
}

type catMin struct {
	cat string
	min *float64
}

// printCapability is the summary under analyze's table: what this machine can
// take over now, what each missing piece would add, and what stays in CI.
func printCapability(m machine, jobs []catMin, priced bool) {
	yes := map[bool]string{true: "yes", false: "no"}
	fmt.Printf("\nThis machine: %s, %d CPUs, docker %s, buildx %s\n\n", m.Platform, m.CPUs, yes[m.Docker], yes[m.Buildx])
	type agg struct {
		n        int
		min      float64
		unpriced int
	}
	by := map[string]*agg{}
	for _, j := range jobs {
		a := by[j.cat]
		if a == nil {
			a = &agg{}
			by[j.cat] = a
		}
		a.n++
		if j.min != nil {
			a.min += *j.min
		} else {
			a.unpriced++
		}
	}
	cats := make([]string, 0, len(by))
	for c := range by {
		cats = append(cats, c)
	}
	rank := func(c string) int {
		switch {
		case c == "ready":
			return 0
		case strings.HasPrefix(c, "needs "):
			return 1
		case c == "review":
			return 2
		}
		return 3
	}
	sort.Slice(cats, func(i, k int) bool {
		if rank(cats[i]) != rank(cats[k]) {
			return rank(cats[i]) < rank(cats[k])
		}
		return cats[i] < cats[k]
	})
	hint := map[string]string{"ready": "evidence can come from this machine now", "review": "drafted, with open issues (see WHY)", "stays in CI": "other OS, secrets, publishing, …"}
	for _, c := range cats {
		a := by[c]
		price := ""
		if priced {
			price = fmt.Sprintf("%4.0f min/PR", a.min)
			if a.unpriced > 0 {
				price += fmt.Sprintf(" (+%d unpriced)", a.unpriced)
			}
		}
		h := hint[c]
		if strings.HasPrefix(c, "needs ") {
			h = "install it to run these here"
		}
		fmt.Printf("  %-22s %3d jobs  %s  %s\n", c, a.n, price, h)
	}
	if !priced {
		fmt.Println("\n  add --minutes to price these from your recent PR runs (uses gh)")
	}
}

var ghAPI = func(path string) ([]byte, error) { return exec.Command("gh", "api", path).Output() } // a var for tests

// requiredChecks are the status checks the default branch requires, from its
// rulesets (readable by anyone who can read the repo) and classic branch
// protection (admins only; skipped if not readable). An error means gh can't
// say, not that nothing is required.
func requiredChecks() ([]string, error) {
	out, err := ghAPI("repos/{owner}/{repo}")
	if err != nil {
		return nil, fmt.Errorf("gh api: %v (is gh installed and logged in?)", err)
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(out, &repo); err != nil || repo.DefaultBranch == "" {
		return nil, fmt.Errorf("gh api: no default branch")
	}
	b := url.PathEscape(repo.DefaultBranch)
	var req []string
	out, err = ghAPI("repos/{owner}/{repo}/rules/branches/" + b)
	if err != nil {
		return nil, fmt.Errorf("gh api rules: %v", err)
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if json.Unmarshal(out, &rules) == nil {
		for _, r := range rules {
			if r.Type == "required_status_checks" {
				for _, c := range r.Parameters.Checks {
					req = append(req, c.Context)
				}
			}
		}
	}
	if out, err := ghAPI("repos/{owner}/{repo}/branches/" + b + "/protection/required_status_checks"); err == nil {
		var classic struct {
			Contexts []string `json:"contexts"`
		}
		if json.Unmarshal(out, &classic) == nil {
			req = append(req, classic.Contexts...)
		}
	}
	return req, nil
}

// requiredLeg reports whether j is a matrix leg that a required check names.
// An excluded leg (or a matrix job skipped whole, which reports one check under
// the job's name) never reports that check, so the PR could never merge.
func requiredLeg(j analyze.Job, required []string) bool {
	if len(j.Combo) == 0 {
		return false // a skipped job reports its check as passed
	}
	_, name, _ := strings.Cut(analyzedKey(j), "\x00")
	for _, r := range required {
		if _, rn, _ := strings.Cut(jobKey("", r), "\x00"); rn == name {
			return true
		}
	}
	return false
}

// repoPrivate reports whether the repo is private (its runners are smaller).
func repoPrivate() (bool, error) {
	out, err := ghAPI("repos/{owner}/{repo}")
	if err != nil {
		return false, fmt.Errorf("gh api: %v", err)
	}
	var r struct {
		Private *bool `json:"private"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Private == nil {
		return false, fmt.Errorf("gh api: no visibility")
	}
	return *r.Private, nil
}
