package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/billiondollarsolo/cinema/internal/evidence"
	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/record"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
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
	if err := fs.Parse(args); err != nil || *eventPath == "" {
		fmt.Fprintln(os.Stderr, "usage: cinema verify --event FILE [--execute] [--report FILE]")
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

	allRun := ""
	keys, kerr := evidence.ParseTrustedKeys(os.Getenv("CINEMA_TRUSTED_KEYS"))
	switch {
	case policyNote != "":
		allRun = plan.PolicyUnavailable
	case fork:
		allRun = plan.ForkPR
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
	for _, id := range ids {
		t := cfg.Tasks[id]
		var d plan.GateDecision
		if allRun != "" {
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
			}
		}
		rep.Tasks = append(rep.Tasks, gateTaskReport{GateDecision: d})
	}

	printGatePlan(rep)
	if !*execute {
		writeGateReport(*reportPath, rep, start)
		return 0
	}
	for i := range rep.Tasks {
		tr := &rep.Tasks[i]
		if tr.Decision != plan.GateRun {
			continue
		}
		t := cfg.Tasks[tr.Task]
		fmt.Printf("::group::RUN %s: %s\n", t.ID, strings.Join(t.Argv, " "))
		res, err := record.Run(context.Background(), record.Options{Argv: t.Argv,
			Dir: filepath.Join(root, filepath.FromSlash(t.Cwd)), Stdout: os.Stdout, Stderr: os.Stderr})
		fmt.Println("::endgroup::")
		tr.Executed = true
		tr.ExitCode, tr.DurationMS = res.ExitCode, res.Duration.Milliseconds()
		tr.Passed = err == nil && res.ExitCode == 0 && res.Signal == "" && !res.Canceled
		executed[tr.Task] = tr.Passed
		mark := "passed"
		if !tr.Passed {
			mark = fmt.Sprintf("FAILED (exit %d)", res.ExitCode)
		}
		fmt.Printf("%s %s in %s\n", t.ID, mark, res.Duration.Round(time.Millisecond))
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

// baseConfig loads tasks.yaml as of base. If base has none (CInema is being
// introduced by this change), the candidate's tasks are used and all of them
// run: nothing can be reused until the config exists on the base branch.
func baseConfig(root, base, candidate string) (*task.Config, string, error) {
	b, err := gitOut(root, "show", base+":"+task.ConfigPath)
	if err == nil {
		cfg, err := task.Parse([]byte(b + "\n"))
		return cfg, "", err
	}
	cfg, err := task.Load(root)
	if err != nil {
		return nil, "", err
	}
	return cfg, "no " + task.ConfigPath + " on the base branch yet: every task runs", nil
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
	env, err := t.EnvFingerprint(root)
	if err != nil {
		return exp, err
	}
	exp.Env = env.Digest
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
