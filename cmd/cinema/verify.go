package main

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/billiondollarsolo/cinema/internal/analyze"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/billiondollarsolo/cinema/internal/evidence"
	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/record"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
	"github.com/billiondollarsolo/cinema/internal/transport"
)

// ghEvent is the subset of a GitHub Actions event payload the gate reads.
type ghEvent struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *struct {
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	MergeGroup *struct {
		BaseSHA string `json:"base_sha"`
		HeadSHA string `json:"head_sha"`
	} `json:"merge_group"`
}

type gateTaskReport struct {
	plan.GateDecision
	Executed   bool  `json:"executed"`
	Passed     bool  `json:"passed"`
	ExitCode   int   `json:"exit_code,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`
}

type gateReport struct {
	SchemaVersion int              `json:"schema_version"`
	Repository    string           `json:"repository_id"`
	Candidate     string           `json:"candidate"`
	Base          string           `json:"base"`
	Note          string           `json:"note,omitempty"` // why every task ran, when that's policy-wide
	Tasks         []gateTaskReport `json:"tasks"`
	Result        string           `json:"result"` // pass | fail | plan
	Receipts      int              `json:"receipts_seen"`
	DurationMS    int64            `json:"duration_ms"`
}

func cmdVerify(args []string) int {
	start := time.Now()
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	eventPath := fs.String("event", os.Getenv("GITHUB_EVENT_PATH"), "GitHub event payload `FILE`")
	execute := fs.Bool("execute", false, "run every task that isn't reused, then pass/fail the gate")
	reportPath := fs.String("report", "", "write gate-report.json to `FILE`")
	remote := fs.String("remote", "origin", "fetch evidence from `REMOTE`")
	only := fs.String("task", "", "with --execute: run just `ID` (one job of a parallel matrix)")
	skipFor := fs.String("skip-outputs", "", "judge only tasks that cover jobs of workflow `FILE` (covers: FILE#job) and output skip_<job>=true for each job whose tasks are all reused")
	if err := fs.Parse(args); err != nil || *eventPath == "" || (*only != "" && !*execute) || (*skipFor != "" && *execute) {
		fmt.Fprintln(os.Stderr, "usage: cinema verify --event FILE [--execute [--task ID]] [--report FILE]")
		return exitBadArgs
	}
	fail := func(f string, a ...any) int {
		fmt.Fprintf(os.Stderr, "cinema verify: "+f+"\n", a...)
		return exitUnsatisfied
	}
	raw, err := os.ReadFile(*eventPath)
	if err != nil {
		return fail("%v", err)
	}
	var ev ghEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fail("event: %v", err)
	}
	cwd, _ := os.Getwd()
	root, err := snapshot.RepoRoot(cwd)
	if err != nil {
		return fail("%v", err)
	}
	candidate, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return fail("%v", err)
	}

	rep := gateReport{SchemaVersion: 1, Candidate: candidate, Result: "plan"}
	base, head, fork := candidate, candidate, false
	switch {
	case ev.PullRequest != nil:
		base, head = ev.PullRequest.Base.SHA, ev.PullRequest.Head.SHA
		fork = ev.PullRequest.Head.Repo == nil || !strings.EqualFold(ev.PullRequest.Head.Repo.FullName, ev.Repository.FullName)
	case ev.MergeGroup != nil:
		base, head = ev.MergeGroup.BaseSHA, ev.MergeGroup.HeadSHA
	}
	rep.Base = base
	host := "github.com"
	if u := os.Getenv("GITHUB_SERVER_URL"); u != "" {
		host = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	}
	repoName := os.Getenv("GITHUB_REPOSITORY")
	if repoName == "" {
		repoName = ev.Repository.FullName
	}
	rep.Repository = strings.ToLower(host + "/" + repoName)

	// Required tasks come from the BASE branch: a PR can't loosen its own requirements.
	cfg, policyNote, err := baseConfig(root, base, candidate)
	if err != nil {
		rep.Result = "fail"
		rep.Note = plan.PolicyInvalid + ": " + err.Error()
		writeGateReport(*reportPath, rep, start)
		return fail("%s", rep.Note)
	}
	rep.Note = policyNote

	// Matrix job: run one task from the base config, nothing else. Running a task
	// is always safe; the plan job already decided it isn't reused.
	if *only != "" {
		t := cfg.Tasks[*only]
		if t == nil {
			return fail("no task %q in the base branch's %s", *only, task.ConfigPath)
		}
		tr := gateTaskReport{GateDecision: plan.GateDecision{Task: t.ID, Decision: plan.GateRun}}
		executeGateTask(root, t, &tr)
		rep.Tasks = []gateTaskReport{tr}
		rep.Result = map[bool]string{true: "pass", false: "fail"}[tr.Passed]
		writeGateReport(*reportPath, rep, start)
		if !tr.Passed {
			return exitUnsatisfied
		}
		return 0
	}

	allRun := ""
	keys, kerr := evidence.ParseTrustedKeys(os.Getenv("CINEMA_TRUSTED_KEYS"))
	switch {
	case policyNote != "":
		allRun = plan.PolicyUnavailable
	case fork:
		allRun = plan.ForkPR
	case ev.PullRequest != nil && candidate == base && head != base:
		// The checkout is the base, not the PR (pull_request_target): receipts
		// for it say nothing about the code the PR's jobs test.
		allRun = plan.PolicyUnavailable
		rep.Note = "checked out the base commit, not the pull request: every task runs"
	case kerr != nil || len(keys) == 0:
		allRun = plan.PolicyUnavailable
		rep.Note = "CINEMA_TRUSTED_KEYS missing or invalid: every task runs"
	}

	var stmts []evidence.VerifiedStatement
	var rejected []string
	if allRun == "" {
		if err := transport.Fetch(root, *remote, os.Getenv("CINEMA_FETCH_TOKEN")); err != nil {
			allRun = plan.TransportUnavailable
			rep.Note = "evidence fetch failed: " + err.Error()
		} else {
			stmts, rejected, rep.Receipts = collectReceipts(root, keys, candidate, base, head)
		}
	}

	ids := sortedIDs(cfg)
	executed := map[string]bool{}
	// Tasks for another platform can't be judged on this runner (below).
	elsewhere := map[string][]string{}
	for _, id := range ids {
		t := cfg.Tasks[id]
		if *skipFor != "" && !strings.HasPrefix(t.Covers, *skipFor+"#") {
			continue // another workflow's job: its own cinema job judges it
		}
		if t.Platform != task.PlatformAny && t.Platform != task.Platform() {
			elsewhere[t.Platform] = append(elsewhere[t.Platform], id)
			continue
		}
		var d plan.GateDecision
		if t.Event != "" && t.Event != eventName(ev) && allRun == "" {
			d = plan.GateDecision{Task: id, Decision: plan.GateRun, Reason: plan.EventMismatch}
		} else if why := floatingMoved(t); why != "" && allRun == "" {
			d = plan.GateDecision{Task: id, Decision: plan.GateRun, Reason: plan.EnvironmentMismatch}
			fmt.Fprintf(os.Stderr, "cinema verify: %s: %s\n", id, why)
		} else if allRun != "" {
			d = plan.GateDecision{Task: id, Decision: plan.GateRun, Reason: allRun}
			if !t.Reuse.Allowed {
				d.Reason = plan.ReuseDisabled
			}
		} else {
			exp, err := expectedFor(root, t, rep.Repository, candidate)
			if err != nil {
				d = plan.GateDecision{Task: id, Decision: plan.GateRun, Reason: taskError}
				fmt.Fprintf(os.Stderr, "cinema verify: %s: %v\n", id, err)
			} else {
				d = plan.GateTask(t, exp, stmts, rejected)
				if d.Reason == plan.EnvironmentMismatch { // compare with `cinema explain <task>` where the receipt was made
					fmt.Fprintf(os.Stderr, "cinema verify: %s: this runner's environment: %s\n", id, strings.Join(exp.EnvInputs, "; "))
				}
			}
		}
		rep.Tasks = append(rep.Tasks, gateTaskReport{GateDecision: d})
	}

	printGatePlan(rep)
	if len(elsewhere) > 0 {
		// A receipt's environment can only be checked on its own platform, and the
		// gate runs on Linux: fail closed rather than run them here or skip them.
		for p, ids := range elsewhere {
			fmt.Printf("gate: FAIL (tasks %s are for %s; cinema supports linux tasks only for now)\n", strings.Join(ids, ", "), p)
		}
		return exitUnsatisfied
	}
	if *skipFor != "" {
		writeSkipOutputs(cfg, rep, root, *skipFor) // writes its own per-job summary
		writeGateReport(*reportPath, rep, start)
		return 0
	}
	if !*execute {
		// Plan job of the parallel gate: hand the RUN list to a matrix (one job per task).
		var run []string
		for _, tr := range rep.Tasks {
			if tr.Decision == plan.GateRun {
				run = append(run, tr.Task)
			}
		}
		writeGitHubOutputs(run, len(rep.Tasks)-len(run))
		writeGateReport(*reportPath, rep, start)
		writeStepSummary(rep)
		return 0
	}
	for i := range rep.Tasks {
		tr := &rep.Tasks[i]
		if tr.Decision != plan.GateRun {
			continue
		}
		executeGateTask(root, cfg.Tasks[tr.Task], tr)
		executed[tr.Task] = tr.Passed
	}
	decisions := make([]plan.GateDecision, len(rep.Tasks))
	for i, tr := range rep.Tasks {
		decisions[i] = tr.GateDecision
	}
	rep.Result = "fail"
	if plan.GatePasses(decisions, executed) {
		rep.Result = "pass"
	}
	writeGateReport(*reportPath, rep, start)
	writeStepSummary(rep)
	fmt.Printf("gate: %s\n", strings.ToUpper(rep.Result))
	if rep.Result != "pass" {
		return exitUnsatisfied
	}
	return 0
}

// executeGateTask runs one task's exact argv from its cwd and records the result.
func executeGateTask(root string, t *task.Task, tr *gateTaskReport) {
	fmt.Printf("::group::RUN %s: %s\n", t.ID, strings.Join(t.Argv, " "))
	dataDir, _ := store.DataDir()
	env, err := t.RuntimeEnv(dataDir) // the same pinned runtime the evidence machine used
	if err != nil {
		fmt.Println("::endgroup::")
		fmt.Printf("%s FAILED: runtime: %v\n", t.ID, err)
		tr.Executed, tr.ExitCode = true, 1
		return
	}
	ctx := context.Background()
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}
	if env == nil {
		env = os.Environ()
	}
	if tmp, err := os.MkdirTemp("", "cinema-task-*"); err == nil {
		defer os.RemoveAll(tmp)
		env = task.PrivateGlobals(env, tmp)
	}
	network, stopServices, err := t.StartServices()
	if err != nil {
		fmt.Println("::endgroup::")
		fmt.Printf("%s FAILED: %v\n", t.ID, err)
		tr.Executed, tr.ExitCode = true, 1
		return
	}
	defer stopServices()
	argv := t.Argv
	if t.Container != "" {
		img, err := task.PinnedImage(t.Container)
		if err != nil {
			fmt.Println("::endgroup::")
			fmt.Printf("%s FAILED: container: %v\n", t.ID, err)
			tr.Executed, tr.ExitCode = true, 1
			return
		}
		var stop func()
		argv, stop = t.ContainerRun(root, img, network, nil, t.Platform)
		defer stop()
	}
	res, err := record.Run(ctx, record.Options{Argv: argv, Env: env,
		Dir: filepath.Join(root, filepath.FromSlash(t.Cwd)), Stdout: os.Stdout, Stderr: os.Stderr})
	fmt.Println("::endgroup::")
	tr.Executed = true
	tr.ExitCode, tr.DurationMS = res.ExitCode, res.Duration.Milliseconds()
	tr.Passed = err == nil && res.ExitCode == 0 && res.Signal == "" && !res.Canceled
	mark := "passed"
	if !tr.Passed {
		mark = fmt.Sprintf("FAILED (exit %d)", res.ExitCode)
	}
	fmt.Printf("%s %s in %s\n", t.ID, mark, res.Duration.Round(time.Millisecond))
}

// writeGitHubOutputs sets run_tasks (a JSON array for a matrix) and counts.
// Task IDs match ^[a-z0-9._-]+$, so they're safe in workflow expressions.
func writeGitHubOutputs(run []string, reused int) {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return
	}
	if run == nil {
		run = []string{}
	}
	b, _ := json.Marshal(run)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema verify: GITHUB_OUTPUT: %v\n", err)
		return
	}
	fmt.Fprintf(f, "run_tasks=%s\nrun_count=%d\nreused_count=%d\n", b, len(run), reused)
	f.Close()
}

// baseConfig loads tasks.yaml as of base. Fail closed: a base commit missing
// from the checkout is an error (never fall back to the candidate's config,
// which the PR controls). Only when base positively has no config AND the
// candidate is the base itself (a push introducing CInema) are the candidate's
// tasks used, and then every one of them runs.
func baseConfig(root, base, candidate string) (*task.Config, string, error) {
	if _, err := gitOut(root, "cat-file", "-e", base+"^{commit}"); err != nil {
		return nil, "", fmt.Errorf("base commit %s is not in the checkout (use actions/checkout with fetch-depth: 0)", short(base))
	}
	listed, err := gitOut(root, "ls-tree", "--name-only", base, "--", task.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	if listed != "" {
		b, err := gitOut(root, "show", base+":"+task.ConfigPath)
		if err != nil {
			return nil, "", err
		}
		cfg, err := task.Parse([]byte(b + "\n"))
		return cfg, "", err
	}
	if base != candidate {
		return nil, "", fmt.Errorf("the base branch has no %s: merge it there first; a change can't define its own required tasks", task.ConfigPath)
	}
	cfg, err := task.Load(root)
	if err != nil {
		return nil, "", err
	}
	return cfg, "no " + task.ConfigPath + " before this commit: every task runs", nil
}

// collectReceipts verifies envelopes from every cinema notes ref on the
// candidate, the PR head, and the PR's commits (bounded).
func collectReceipts(root string, keys []ed25519.PublicKey, candidate, base, head string) ([]evidence.VerifiedStatement, []string, int) {
	commits := []string{candidate}
	if head != candidate {
		commits = append(commits, head)
	}
	if base != candidate && base != "" {
		if out, err := gitOut(root, "rev-list", "--max-count=50", base+".."+head); err == nil {
			commits = append(commits, strings.Fields(out)...)
		}
	}
	refs, _ := transport.Refs(root)
	var stmts []evidence.VerifiedStatement
	var rejected []string
	seen := map[string]bool{}
	n := 0
	for _, ref := range refs {
		for _, c := range commits {
			if seen[ref+c] {
				continue
			}
			seen[ref+c] = true
			envs, err := transport.Read(root, ref, c)
			if err != nil {
				rejected = append(rejected, plan.Malformed)
				continue
			}
			for _, raw := range envs {
				n++
				vs, err := evidence.Verify(raw, keys)
				switch {
				case err == nil:
					stmts = append(stmts, vs)
				case errors.Is(err, evidence.ErrSignatureInvalid):
					rejected = append(rejected, plan.SignatureInvalid)
				case errors.Is(err, evidence.ErrSignerNotAllowed):
					rejected = append(rejected, plan.SignerNotAllowed)
				default:
					rejected = append(rejected, plan.Malformed)
				}
			}
		}
	}
	return stmts, rejected, n
}

func expectedFor(root string, t *task.Task, repoID, candidate string) (plan.Expected, error) {
	exp := plan.Expected{RepositoryID: repoID, Now: time.Now()}
	var err error
	if exp.Contract, err = t.ContractDigest(root); err != nil {
		return exp, err
	}
	dataDir, err := store.DataDir()
	if err != nil {
		return exp, err
	}
	renv, err := t.RuntimeEnv(dataDir)
	if err != nil {
		return exp, err
	}
	env, err := t.EnvFingerprint(root, renv)
	if err != nil {
		return exp, err
	}
	exp.Env, exp.EnvInputs = env.Digest, env.Inputs
	s, err := snapshot.Take(root, t.ExtraInputs, t.Outputs, nil)
	if err != nil {
		return exp, err
	}
	exp.Source = s.Digest
	if t.VCSContext == "exact" {
		exp.VCS = candidate
	}
	return exp, nil
}

func printGatePlan(rep gateReport) {
	if rep.Note != "" {
		fmt.Println("note:", rep.Note)
	}
	fmt.Printf("candidate %s, %d receipt(s) seen\n", short(rep.Candidate), rep.Receipts)
	for _, t := range rep.Tasks {
		line := fmt.Sprintf("  %-6s %s", t.Decision, t.Task)
		if t.Reason != "" {
			line += "  (" + t.Reason + ")"
		}
		if t.Signer != "" {
			line += "  run " + t.RunID + " signed by " + t.Signer
		}
		fmt.Println(line)
	}
}

func writeGateReport(path string, rep gateReport, start time.Time) {
	if path == "" {
		return
	}
	rep.DurationMS = time.Since(start).Milliseconds()
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "cinema verify: report: %v\n", err)
	}
}

// writeStepSummary renders the decision table into the job summary, if any.
func writeStepSummary(rep gateReport) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### cinema gate: %s\n\n| Task | Decision | Reason | Ran |\n|---|---|---|---|\n", rep.Result)
	reused := 0
	for _, t := range rep.Tasks {
		ran := ""
		if t.Executed {
			ran = fmt.Sprintf("%v, %s", map[bool]string{true: "passed", false: "failed"}[t.Passed], time.Duration(t.DurationMS)*time.Millisecond)
		}
		if t.Decision == plan.GateReuse {
			reused++
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", t.Task, t.Decision, t.Reason, ran)
	}
	fmt.Fprintf(&b, "\n%d of %d tasks reused from verified receipts.\n", reused, len(rep.Tasks))
	if rep.Note != "" {
		fmt.Fprintf(&b, "\n%s\n", rep.Note)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err == nil {
		f.WriteString(b.String())
		f.Close()
	}
}

// writeSkipOutputs decides, for each job of workflow wf that tasks cover,
// whether it skips whole (skip_<job>=true) or drops reused matrix legs
// (exclude_<job>). It judges the workflow as checked out (the code under test),
// not tasks.yaml: a leg counts as reused only if a reused task was drafted from
// exactly that leg (job_digest), and a job skips whole only if every leg it
// expands to is reused. A changed step, if:, runner or a new leg means it runs.
func writeSkipOutputs(cfg *task.Config, rep gateReport, root, wf string) {
	reused := map[string]bool{} // job_digest of every reused, graduated task
	shadow := map[string]bool{} // reused, but not graduated: would skip
	jobsCovered := map[string]bool{}
	// Spot check (plan §11.5): one run in 20, picked by the run's own ID,
	// skips nothing, so graduated stand-ins keep being compared with CI.
	spot := false
	// Hashed, not id%20: runs created together (one push, several workflows)
	// get ids a multiple of 20 apart, and would all be spot checks or none.
	if id := os.Getenv("GITHUB_RUN_ID"); id != "" && sha256.Sum256([]byte(id))[0]%20 == 0 {
		spot = true
		fmt.Println("  spot check: this run skips nothing, so CI's results can be compared with cinema's")
	}
	why := map[string][]string{} // job: why its tasks weren't reused, or who proved them
	for _, tr := range rep.Tasks {
		t := cfg.Tasks[tr.Task]
		_, job, _ := strings.Cut(t.Covers, "#")
		jobsCovered[job] = true
		if tr.Decision == plan.GateReuse {
			why[job] = append(why[job], fmt.Sprintf("%s: passed on %s", tr.Task, cmp.Or(tr.Signer, "a trusted machine")))
		} else {
			why[job] = append(why[job], fmt.Sprintf("%s: %s", tr.Task, cmp.Or(tr.Reason, tr.Decision)))
		}
		if tr.Decision == plan.GateReuse && t.JobDigest != "" {
			if t.Graduates() && !spot {
				reused[t.JobDigest] = true
			} else if !t.Graduates() {
				shadow[job] = true
			}
		}
	}
	legs := candidateLegs(root, wf)
	var lines, summary strings.Builder
	for _, job := range sortedSet(jobsCovered) {
		all := len(legs[job]) > 0
		var excl []map[string]string
		isMatrix := false
		for _, l := range legs[job] {
			isMatrix = isMatrix || len(l.MatrixKeys) > 0
			ok := reused[l.LegDigest()]
			all = all && ok
			if leg := legOf(l); ok && leg != nil {
				excl = append(excl, leg)
			}
		}
		state := map[bool]string{true: "SKIP", false: "RUN"}[all]
		switch {
		case all:
			excl = nil // the job skips whole
		case len(excl) > 0:
			state = fmt.Sprintf("RUN (%d reused leg(s) excluded)", len(excl))
		}
		if shadow[job] && !all {
			state += " (shadow: reused, but not graduated; run cinema graduate)"
		}
		fmt.Printf("  %-6s %s\n", state, job)
		fmt.Fprintf(&summary, "| `%s` | %s | %s |\n", job, state, strings.Join(why[job], "<br>"))
		fmt.Fprintf(&lines, "skip_%s=%t\n", job, all)
		if isMatrix {
			if excl == nil {
				excl = []map[string]string{}
			}
			b, _ := json.Marshal(excl)
			fmt.Fprintf(&lines, "exclude_%s=%s\n", job, b)
		}
	}
	head := "## cinema: " + wf + "\n\nSKIP: proven by a signed local run, so this job didn't use a runner. RUN: it ran as usual.\n"
	if spot {
		head += "\nSpot check: this run skips nothing, so CI's results can be compared with cinema's.\n"
	}
	appendTo(os.Getenv("GITHUB_OUTPUT"), lines.String())
	appendTo(os.Getenv("GITHUB_STEP_SUMMARY"), head+"\n| Job | In CI | Why |\n|---|---|---|\n"+summary.String())
}

// appendTo appends s to the file at path (GitHub's step files); "" does nothing.
func appendTo(path, s string) {
	if path == "" {
		return
	}
	if f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
		f.WriteString(s)
		f.Close()
	}
}

// candidateLegs is every leg (matrix combination) of each job of workflow wf
// as checked out. A job whose matrix can't be expanded yields a leg without a
// proposal, which no task matches: it never skips. Analyzed exactly as setup
// drafts (the whole repo: version files, repo-wide task ids), so an unchanged
// job has the job_digest setup wrote.
func candidateLegs(root, wf string) map[string][]analyze.Job {
	out := map[string][]analyze.Job{}
	jobs, err := analyze.Repo(root)
	if err != nil {
		return out
	}
	for _, j := range jobs {
		if j.Workflow == filepath.Base(wf) {
			out[j.ID] = append(out[j.ID], j)
		}
	}
	return out
}

// eventName is the triggering event: GitHub's own name when set, else what
// the payload says.
func eventName(ev ghEvent) string {
	if n := os.Getenv("GITHUB_EVENT_NAME"); n != "" {
		return n
	}
	switch {
	case ev.PullRequest != nil:
		return "pull_request"
	case ev.MergeGroup != nil:
		return "merge_group"
	}
	return "push"
}

// floatingMoved resolves a task's floating: specs as CI's setup actions would
// today. A pin that isn't today's release, or a spec that can't be resolved,
// means the job runs: the receipt describes yesterday's toolchain (plan §11.2).
func floatingMoved(t *task.Task) string {
	for tool, spec := range t.Floating {
		pin := t.Runtime.Get(tool)
		got, err := floatingResolve(cmp.Or(map[string]string{"go": "Go", "node": "Node"}[tool], tool), spec)
		switch {
		case err != nil:
			return fmt.Sprintf("can't resolve %s %q today (%v): running it", tool, spec, err)
		case got != pin && !strings.HasPrefix(pin, got+"+"): // 1.83.0+clippy is 1.83.0 with components
			return fmt.Sprintf("%s %q is %s today; the task pins %s: running it (rerun cinema setup to pin the new release)", tool, spec, got, pin)
		}
	}
	return ""
}

var floatingResolve = analyze.Resolve // a var for tests
