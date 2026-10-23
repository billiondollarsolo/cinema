package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
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
	m.Docker = exec.Command("docker", "info").Run() == nil
	m.Buildx = m.Docker && exec.Command("docker", "buildx", "version").Run() == nil
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
	case cmp.Or(j.Proposal.Platform, "linux/amd64") != m.Platform:
		return "stays in CI", "runs on " + cmp.Or(j.Proposal.Platform, "linux/amd64") + "; this machine is " + m.Platform
	}
	var need []string
	if j.Proposal.Container != "" && !m.Docker {
		need = append(need, "docker")
	}
	for _, t := range runnerTools {
		if usesTool(j.Proposal.Body, t) {
			if _, err := lookPath(t); err != nil && (j.Proposal.Container == "" || t == "docker") {
				need = append(need, t)
			}
		}
	}
	if len(need) > 0 {
		return "needs " + strings.Join(need, ", "), ""
	}
	if j.Verdict == analyze.Review {
		return "review", firstReason(j)
	}
	return "ready", ""
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

func analyzedKey(j analyze.Job) string {
	name := j.Name
	if name == j.ID && len(j.Combo) > 0 {
		var vals []string
		for _, v := range j.Combo {
			vals = append(vals, v)
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
