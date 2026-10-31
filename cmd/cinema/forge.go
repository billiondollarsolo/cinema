package main

// The forge edges (plan §11.7): what cinema reads from where CI runs. The
// core (tasks, runs, receipts in git notes, the gate's judgment) is the same
// for every forge. What differs, and where it lives:
//
//   - reading CI:      analyze.Repo (GitHub workflows) and analyze.GitLab
//   - skipping:        skip-in-place outputs (skipinplace.go) / child pipeline (gitlab.go)
//   - the run's facts: ciEvent from githubEvent / gitlabEvent (verify.go, gitlab.go)
//   - trusted keys:    CINEMA_TRUSTED_KEYS in the job's environment, from a
//     repository variable (GitHub) or a CI/CD variable (GitLab)
//   - the CI API:      forge, below (graduate's backtest)
//
// Live pairs, job logs, minutes and required checks are GitHub-only so far
// (plan §11.7 lists what's left).

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"

	"github.com/billiondollarsolo/cinema/internal/analyze"
)

// forge reads CI's results.
type forge interface {
	// conclusion is CI's result for job leg j in the newest push run at
	// sha: "success", "failure", "none" (no such run or job), or another
	// state (skipped, cancelled, manual…), which is never compared.
	conclusion(sha string, j analyze.Job) (ciJob, error)
	jobLog(id int64) ([]byte, error) // a job's log, for which tests failed
}

// forgeFor is the forge of the CI file a job comes from.
func forgeFor(workflow string) forge {
	if workflow == analyze.GitLabFile {
		return gitlabForge{}
	}
	return githubForge{}
}

type githubForge struct{}

func (githubForge) conclusion(sha string, j analyze.Job) (ciJob, error) {
	return ciConclusion(sha, j.Workflow, analyzedKey(j))
}

func (githubForge) jobLog(id int64) ([]byte, error) { return ciJobLog(id) }

func (gitlabForge) jobLog(id int64) ([]byte, error) {
	out, err := glabAPI(fmt.Sprintf("projects/:id/jobs/%d/trace", id))
	if err != nil {
		return nil, fmt.Errorf("glab api: %v", err)
	}
	return out, nil
}

type gitlabForge struct{}

var glabAPI = func(path string) ([]byte, error) { return exec.Command("glab", "api", path).Output() } // a var for tests

// conclusion finds the newest push pipeline at sha and the job named like
// j.Name in it, or in the child pipeline cinema's trigger job started (where
// covered jobs run once setup moved them).
func (gitlabForge) conclusion(sha string, j analyze.Job) (ciJob, error) {
	out, err := glabAPI(fmt.Sprintf("projects/:id/pipelines?sha=%s&source=push&order_by=id&sort=desc&per_page=20", url.QueryEscape(sha)))
	if err != nil {
		return ciJob{}, fmt.Errorf("glab api: %v (is glab installed and logged in?)", err)
	}
	var pipes []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(out, &pipes); err != nil {
		return ciJob{}, err
	}
	if len(pipes) == 0 {
		return ciJob{Conclusion: "none"}, nil
	}
	pid := pipes[0].ID
	job, err := glJobStatus(pid, j.Name)
	if err != nil || job.Conclusion != "none" {
		return job, err
	}
	out, err = glabAPI(fmt.Sprintf("projects/:id/pipelines/%d/bridges?per_page=100", pid))
	if err != nil {
		return ciJob{}, fmt.Errorf("glab api: %v", err)
	}
	var bridges []struct {
		Name       string `json:"name"`
		Downstream *struct {
			ID int64 `json:"id"`
		} `json:"downstream_pipeline"`
	}
	if err := json.Unmarshal(out, &bridges); err != nil {
		return ciJob{}, err
	}
	for _, b := range bridges {
		if b.Name == "cinema:child" && b.Downstream != nil {
			return glJobStatus(b.Downstream.ID, j.Name)
		}
	}
	return ciJob{Conclusion: "none"}, nil
}

// glJobStatus is the status of the job named name in pipeline pid, as
// success / failure / its GitLab status; "none" if there's no such job.
func glJobStatus(pid int64, name string) (ciJob, error) {
	out, err := glabAPI(fmt.Sprintf("projects/:id/pipelines/%d/jobs?per_page=100", pid))
	if err != nil {
		return ciJob{}, fmt.Errorf("glab api: %v", err)
	}
	var jobs []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &jobs); err != nil {
		return ciJob{}, err
	}
	for _, x := range jobs {
		if x.Name == name {
			c := map[string]string{"success": "success", "failed": "failure"}[x.Status]
			return ciJob{ID: x.ID, Conclusion: cmp.Or(c, x.Status)}, nil
		}
	}
	return ciJob{Conclusion: "none"}, nil
}
