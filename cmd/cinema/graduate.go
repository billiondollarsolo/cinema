package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
	"github.com/billiondollarsolo/cinema/internal/testreport"
)

// cmdGraduate backtests stand-ins against CI's own history (plan §11.5): each
// task runs on the default branch's last N commits, where the job it covers
// had exactly its current definition, and its result is compared with that
// commit's CI result for the job. If every one agrees, the task is graduated:
// its job may skip in CI. A pass where CI failed is a false green caught
// before it could happen.
func cmdGraduate(args []string) int {
	defer stopProbeSandboxes()
	fs := flag.NewFlagSet("graduate", flag.ContinueOnError)
	commits := fs.Int("commits", 10, "how many of the default branch's recent commits to backtest")
	min := fs.Int("min", 3, "graduate only after this many commits had a CI result to compare")
	yes := fs.Bool("yes", false, "write the graduation without asking")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: cinema graduate [--commits N] [--min N] [--yes] <task>...")
		return exitBadArgs
	}
	if !*yes && !interactive() { // ask now, not after a long backtest
		fmt.Fprintln(os.Stderr, "cinema graduate: not a terminal: rerun with --yes to write the result")
		return exitBadArgs
	}
	r, err := openRepo()
	if err != nil || r.cfg == nil {
		fmt.Fprintf(os.Stderr, "cinema graduate: %v%v\n", err, r.cfgErr)
		return exitBadArgs
	}
	defer r.close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	branch := "HEAD"
	if b, err := gitOut(r.root, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil {
		branch = b
	}
	list, err := gitOut(r.root, "rev-list", "--first-parent", "-n", fmt.Sprint(*commits), branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema graduate: %v\n", err)
		return exitBadArgs
	}
	shas := strings.Fields(list)
	graduated := map[string]task.Graduation{}
	failed := 0
	for _, id := range fs.Args() {
		t := r.cfg.Tasks[id]
		switch {
		case t == nil:
			fmt.Fprintf(os.Stderr, "cinema graduate: no task %q\n", id)
			return exitBadArgs
		case t.Covers == "" || t.JobDigest == "":
			fmt.Printf("✗ %s  covers no CI job (covers:, job_digest:): nothing to graduate\n", id)
			failed++
			continue
		case t.Event == "pull_request":
			// The default branch's history is push runs; this job differs by event.
			fmt.Printf("✗ %s  its job depends on the event: backtesting push runs doesn't prove pull_request ones\n", id)
			failed++
			continue
		}
		g, why := backtest(ctx, r, t, shas)
		if why == "" {
			g.Agreed, why = livePairs(r, t, *commits*3)
		}
		if why != "" || g.Backtested+g.Agreed < *min {
			if why == "" {
				why = fmt.Sprintf("only %d result(s) to compare with CI (need %d): try --commits, or let a few PRs run in shadow with `cinema push --run`", g.Backtested+g.Agreed, *min)
			}
			fmt.Printf("✗ %s  not graduated: %s\n", id, why)
			failed++
			continue
		}
		fmt.Printf("✓ %s  agreed with CI on %d past commit(s) (%d where CI failed) and %d PR tree(s)\n", id, g.Backtested, g.CIFailures, g.Agreed)
		graduated[id] = g
	}
	if len(graduated) == 0 {
		return exitUnsatisfied
	}
	path := filepath.Join(r.root, task.ConfigPath)
	if !*yes && !confirm(fmt.Sprintf("\nWrite the graduation of %d task(s) to %s? [y/N] ", len(graduated), task.ConfigPath)) {
		fmt.Println("Nothing written.")
		return 0
	}
	if err := writeGraduations(path, graduated); err != nil {
		fmt.Fprintf(os.Stderr, "cinema graduate: %v\n", err)
		return exitInternal
	}
	formatLikeRepo(r.root, task.ConfigPath)
	fmt.Println("Written. Commit it: once it's on the default branch, these jobs skip in CI when reused.")
	if failed > 0 {
		return exitUnsatisfied
	}
	return 0
}

// backtest runs t on each commit where its job is unchanged and has a CI
// result, and compares. why != "": a disagreement (or nothing to compare).
func backtest(ctx context.Context, r *repo, t *task.Task, shas []string) (task.Graduation, string) {
	g := task.Graduation{JobDigest: t.JobDigest, At: time.Now().UTC().Format("2006-01-02")}
	wf, _, _ := strings.Cut(t.Covers, "#")
	for _, sha := range shas {
		if ctx.Err() != nil {
			return g, "interrupted"
		}
		cr, cleanup, err := cleanCheckout(r, sha)
		if err != nil {
			cleanup()
			return g, fmt.Sprintf("checkout %s: %v", short(sha), err)
		}
		var leg *analyze.Job
		for _, legs := range candidateLegs(cr.root, wf) {
			for i := range legs {
				if legs[i].LegDigest() == t.JobDigest {
					leg = &legs[i]
				}
			}
		}
		if leg == nil {
			cleanup()
			fmt.Printf("  %s  %s: the job was defined differently then; skipped\n", t.ID, short(sha))
			continue
		}
		f := forgeFor(wf)
		job, err := f.conclusion(sha, *leg)
		ci := job.Conclusion
		if err != nil || (ci != "success" && ci != "failure") {
			cleanup()
			fmt.Printf("  %s  %s: no CI result to compare (%s%v); skipped\n", t.ID, short(sha), ci, errOrEmpty(err))
			continue
		}
		bad, _, tests := runOne(ctx, cr, t, nil, 0)
		cleanup()
		local := map[bool]string{true: "failure", false: "success"}[bad]
		if local != ci {
			if ci == "failure" {
				return g, fmt.Sprintf("on %s CI failed and the stand-in passed: it would have skipped a failing job", short(sha))
			}
			return g, fmt.Sprintf("on %s CI passed and the stand-in failed: it doesn't do what the job does here", short(sha))
		}
		if ci == "failure" {
			if why := sameFailures(tests, f, job.ID); why != "" {
				fmt.Printf("  %s  %s: both failed, not counted: %s\n", t.ID, short(sha), why)
				continue
			}
			g.CIFailures++
		}
		g.Backtested++
		fmt.Printf("  %s  %s: agrees (%s)\n", t.ID, short(sha), ci)
	}
	return g, ""
}

// livePairs compares this machine's clean runs of t with CI's result for its
// job on the same trees: the merge commits recent PR runs built (plan §11.5,
// live agreement). Read through the API; nothing runs, here or in CI. Only
// runs of the current contract, on trees where the job is t's job_digest,
// count. why != "": they disagreed.
func livePairs(r *repo, t *task.Task, limit int) (agreed int, why string) {
	contract, err := t.ContractDigest(r.root)
	if err != nil {
		return 0, ""
	}
	runs, err := r.st.TaskRuns(r.id, t.ID, 500)
	if err != nil || strings.HasPrefix(t.Covers, analyze.GitLabFile+"#") { // TODO(plan §11.7): GitLab live pairs
		return 0, ""
	}
	local := map[string]store.TaskRun{} // source digest: newest clean run
	for _, run := range runs {
		if _, seen := local[run.SourceBefore]; seen || run.Capture != "clean" || run.Contract != contract ||
			(run.State != store.StatePassed && run.State != store.StateFailed) {
			continue
		}
		local[run.SourceBefore] = run
	}
	if len(local) == 0 {
		return 0, ""
	}
	wf, _, _ := strings.Cut(t.Covers, "#")
	out, err := ghAPI(fmt.Sprintf("repos/{owner}/{repo}/actions/workflows/%s/runs?event=pull_request&status=completed&per_page=%d", wf, min(limit, 100)))
	if err != nil {
		return 0, ""
	}
	var v struct {
		WorkflowRuns []struct {
			ID      int64  `json:"id"`
			HeadSHA string `json:"head_sha"`
			PRs     []struct {
				Base struct {
					SHA string `json:"sha"`
				} `json:"base"`
			} `json:"pull_requests"`
		} `json:"workflow_runs"`
	}
	if json.Unmarshal(out, &v) != nil {
		return 0, ""
	}
	counted := map[string]bool{}
	for _, wr := range v.WorkflowRuns {
		if len(wr.PRs) == 0 { // a fork's PR: the API doesn't say which base it built on
			continue
		}
		c, err := mergeCommit(r.root, wr.PRs[0].Base.SHA, wr.HeadSHA)
		if err != nil {
			continue // not fetched here, or conflicts (CI ran no PR checks)
		}
		snap, err := snapshot.TakeCommit(r.root, c, t.ExtraInputs, t.Outputs, nil)
		if err != nil || counted[snap.Digest] {
			continue
		}
		run, ok := local[snap.Digest]
		if !ok {
			continue
		}
		passed := run.State == store.StatePassed
		cr, cleanup, err := cleanCheckout(r, c)
		if err != nil {
			cleanup()
			continue
		}
		var key string
		for _, l := range candidateLegs(cr.root, wf)[jobOf(t)] {
			if l.LegDigest() == t.JobDigest {
				key = analyzedKey(l)
			}
		}
		cleanup()
		if key == "" {
			continue // the job was defined differently on that PR
		}
		job, err := ciJobOf(wr.ID, key)
		ci := job.Conclusion
		if err != nil || (ci != "success" && ci != "failure") {
			continue // skipped (it was reused), cancelled, or unknown
		}
		if passed != (ci == "success") {
			if passed {
				return agreed, fmt.Sprintf("on PR head %s CI failed and the stand-in passed on the same tree: it would have skipped a failing job", short(wr.HeadSHA))
			}
			return agreed, fmt.Sprintf("on PR head %s CI passed and the stand-in failed on the same tree", short(wr.HeadSHA))
		}
		if !passed {
			if why := sameFailures(run.Tests, githubForge{}, job.ID); why != "" { // live pairs: GitHub only so far
				fmt.Printf("  %s  PR head %s: both failed, not counted: %s\n", t.ID, short(wr.HeadSHA), why)
				continue
			}
		}
		counted[snap.Digest] = true
		agreed++
		fmt.Printf("  %s  PR head %s: agrees (%s)\n", t.ID, short(wr.HeadSHA), ci)
	}
	return agreed, ""
}

func jobOf(t *task.Task) string {
	_, job, _ := strings.Cut(t.Covers, "#")
	return job
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return " " + err.Error()
}

// sameFailures is why a pair where both sides failed doesn't count as
// agreement ("" = the same tests failed). Exit status alone agrees too easily:
// a stand-in failing for its own reason (a missing file, a tool it lacks)
// looks like CI failing in its tests (plan §11.5 item 6). Both sides are
// parsed the same way, CI's from the job's log.
func sameFailures(local *testreport.Summary, f forge, job int64) string {
	mine, why := testreport.FailedNames(local)
	if why != "" {
		return "the stand-in's failure: " + why
	}
	log, err := f.jobLog(job)
	if err != nil {
		return fmt.Sprintf("CI's job log: %v", err)
	}
	theirs, why := testreport.FailedNames(testreport.Read(log, false, nil).Summary)
	if why != "" {
		return "CI's failure: " + why
	}
	if slices.Equal(mine, theirs) {
		return ""
	}
	only := func(a, b []string) string {
		var d []string
		for _, x := range a {
			if !slices.Contains(b, x) {
				d = append(d, x)
			}
		}
		if len(d) > 5 {
			d = append(d[:5], fmt.Sprintf("… %d more", len(d)-5))
		}
		return dash(strings.Join(d, ", "))
	}
	return fmt.Sprintf("different tests failed (only in CI: %s; only in the stand-in: %s)", only(theirs, mine), only(mine, theirs))
}

// ciJobLog is a CI job's log, read through the API (gh follows the redirect
// to the log file).
func ciJobLog(job int64) ([]byte, error) {
	out, err := ghAPI(fmt.Sprintf("repos/{owner}/{repo}/actions/jobs/%d/logs", job))
	if err != nil {
		return nil, fmt.Errorf("gh api: %v", err)
	}
	return out, nil
}

// ciJob is a job of a CI workflow run. Conclusion "none": no such job or run.
type ciJob struct {
	ID         int64
	Conclusion string
}

// ciConclusion is the job named like key in workflow wf's newest push run at
// sha. By workflow run, not check-run name: two workflows can have a job of
// the same name (cli: lint.yml and govulncheck.yml).
func ciConclusion(sha, wf, key string) (ciJob, error) {
	out, err := ghAPI(fmt.Sprintf("repos/{owner}/{repo}/actions/runs?head_sha=%s&event=push&per_page=100", sha))
	if err != nil {
		return ciJob{}, fmt.Errorf("gh api: %v", err)
	}
	var runs struct {
		WorkflowRuns []struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(out, &runs); err != nil {
		return ciJob{}, err
	}
	run := int64(0)
	for _, r := range runs.WorkflowRuns { // newest first
		if p, _, _ := strings.Cut(r.Path, "@"); p == ".github/workflows/"+wf {
			run = r.ID
			break
		}
	}
	if run == 0 {
		return ciJob{Conclusion: "none"}, nil
	}
	return ciJobOf(run, key)
}

// ciJobOf is the job named like key in workflow run id.
func ciJobOf(run int64, key string) (ciJob, error) {
	out, err := ghAPI(fmt.Sprintf("repos/{owner}/{repo}/actions/runs/%d/jobs?per_page=100", run))
	if err != nil {
		return ciJob{}, fmt.Errorf("gh api: %v", err)
	}
	var v struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return ciJob{}, err
	}
	_, want, _ := strings.Cut(key, "\x00")
	for _, c := range v.Jobs {
		if _, got, _ := strings.Cut(jobKey("", c.Name), "\x00"); got == want {
			return ciJob{c.ID, c.Conclusion}, nil
		}
	}
	return ciJob{Conclusion: "none"}, nil
}

// writeGraduations sets each task's graduated: line in tasks.yaml, textually,
// so the rest of the file (comments, formatting) stays as it is.
func writeGraduations(path string, gs map[string]task.Graduation) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil || len(doc.Content) == 0 {
		return fmt.Errorf("%s: %v", path, err)
	}
	tasks := mapValue(doc.Content[0], "tasks")
	if tasks == nil || tasks.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: no tasks", path)
	}
	lines := strings.SplitAfter(string(src), "\n")
	type ins struct {
		at      int
		text    string
		replace bool
	}
	var edits []ins
	for i := 0; i+1 < len(tasks.Content); i += 2 {
		g, ok := gs[tasks.Content[i].Value]
		v := tasks.Content[i+1]
		if !ok || v.Kind != yaml.MappingNode || len(v.Content) == 0 {
			continue
		}
		indent := strings.Repeat(" ", v.Content[0].Column-1)
		line := fmt.Sprintf("%sgraduated: {job_digest: %q, backtested: %d, ci_failures: %d, agreed: %d, at: %q}\n", indent, g.JobDigest, g.Backtested, g.CIFailures, g.Agreed, g.At)
		if k := mapKey(v, "graduated"); k != nil {
			if gv := mapValue(v, "graduated"); gv.Style&yaml.FlowStyle == 0 && gv.Kind == yaml.MappingNode {
				return fmt.Errorf("task %s: graduated: isn't on one line; edit it by hand", tasks.Content[i].Value)
			}
			edits = append(edits, ins{k.Line - 1, line, true})
			continue
		}
		// After the task's block: the line before the next task (or its comment).
		end := len(lines)
		if i+2 < len(tasks.Content) {
			next := tasks.Content[i+2]
			end = next.Line - 1
			if c := next.HeadComment; c != "" {
				end -= strings.Count(c, "\n") + 1
			}
		}
		for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		edits = append(edits, ins{end, line, false})
	}
	for i := len(edits) - 1; i >= 0; i-- { // bottom-up: earlier positions stay valid
		e := edits[i]
		if e.replace {
			lines[e.at] = e.text
		} else {
			lines = append(lines[:e.at], append([]string{e.text}, lines[e.at:]...)...)
		}
	}
	out := strings.Join(lines, "")
	cfg, err := task.Parse([]byte(out))
	if err != nil {
		return fmt.Errorf("the edited tasks.yaml doesn't parse (nothing written): %v", err)
	}
	for id := range gs {
		if t := cfg.Tasks[id]; t == nil || !t.Graduates() {
			return fmt.Errorf("task %s isn't graduated after the edit (nothing written)", id)
		}
	}
	return os.WriteFile(path, []byte(out), 0o644)
}
