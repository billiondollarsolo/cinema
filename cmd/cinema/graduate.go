package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/task"
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
		if why != "" || g.Backtested < *min {
			if why == "" {
				why = fmt.Sprintf("only %d commit(s) had a CI result to compare (need %d): try --commits", g.Backtested, *min)
			}
			fmt.Printf("✗ %s  not graduated: %s\n", id, why)
			failed++
			continue
		}
		fmt.Printf("✓ %s  agreed with CI on %d commit(s) (%d where CI failed)\n", id, g.Backtested, g.CIFailures)
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
		ci, err := ciConclusion(sha, analyzedKey(*leg))
		if err != nil || (ci != "success" && ci != "failure") {
			cleanup()
			fmt.Printf("  %s  %s: no CI result to compare (%s%v); skipped\n", t.ID, short(sha), ci, errOrEmpty(err))
			continue
		}
		bad, _ := runOne(ctx, cr, t, nil, 0)
		cleanup()
		local := map[bool]string{true: "failure", false: "success"}[bad]
		if local != ci {
			if ci == "failure" {
				return g, fmt.Sprintf("on %s CI failed and the stand-in passed: it would have skipped a failing job", short(sha))
			}
			return g, fmt.Sprintf("on %s CI passed and the stand-in failed: it doesn't do what the job does here", short(sha))
		}
		g.Backtested++
		if ci == "failure" {
			g.CIFailures++
		}
		fmt.Printf("  %s  %s: agrees (%s)\n", t.ID, short(sha), ci)
	}
	return g, ""
}

func errOrEmpty(err error) string {
	if err == nil {
		return ""
	}
	return " " + err.Error()
}

// ciConclusion is the conclusion of the check run named like the job at sha.
func ciConclusion(sha, key string) (string, error) {
	out, err := ghAPI(fmt.Sprintf("repos/{owner}/{repo}/commits/%s/check-runs?per_page=100", sha))
	if err != nil {
		return "", fmt.Errorf("gh api: %v", err)
	}
	var v struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	_, want, _ := strings.Cut(key, "\x00")
	for _, c := range v.CheckRuns {
		if _, got, _ := strings.Cut(jobKey("", c.Name), "\x00"); got == want {
			return c.Conclusion, nil
		}
	}
	return "none", nil
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
		line := fmt.Sprintf("%sgraduated: {job_digest: %q, backtested: %d, ci_failures: %d, at: %q}\n", indent, g.JobDigest, g.Backtested, g.CIFailures, g.At)
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
