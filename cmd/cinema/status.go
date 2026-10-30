package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
)

const (
	exitUnsatisfied = 1
	exitInternal    = 8
)

// Extra local states/reasons beyond package plan's, for things only the CLI knows.
const (
	stateUnknown    = "unknown" // can't evaluate; see reason
	configUntrusted = "CONFIG_UNTRUSTED"
	taskError       = "TASK_ERROR"
)

type taskStatus struct {
	plan.Decision
	Platform string      `json:"platform"`
	Local    bool        `json:"satisfiable_here"` // this machine can produce evidence for it
	Detail   string      `json:"detail,omitempty"`
	LastRun  *runSummary `json:"last_run,omitempty"`
}

type runSummary struct {
	ID         string    `json:"id"`
	State      string    `json:"state"`
	Started    time.Time `json:"started_at"`
	ExitCode   int       `json:"exit_code"`
	DurationMS int64     `json:"duration_ms"`
}

// envelope is the one JSON object --json writes to stdout (plan §10.3).
func envelope(op string, data any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"schema_version": 1, "operation": op, "data": data})
}

// loadForReport opens the repo and config for status/explain, reporting fatal problems.
func loadForReport(cmd string, asJSON bool) (*repo, int) {
	r, err := openRepo()
	fail := func(code int, reason, msg string) (*repo, int) {
		if asJSON {
			_ = envelope(cmd, map[string]string{"error": msg, "reason_code": reason})
		} else {
			fmt.Fprintf(os.Stderr, "cinema %s: %s\n", cmd, msg)
		}
		return r, code
	}
	switch {
	case err != nil:
		return fail(exitBadArgs, "", err.Error())
	case r.stErr != nil:
		return fail(exitInternal, "", "local store: "+r.stErr.Error())
	case errors.Is(r.cfgErr, task.ErrNoConfig):
		return fail(exitBadArgs, plan.PolicyUnavailable, "no "+task.ConfigPath+" (run `cinema init`)")
	case r.cfgErr != nil:
		return fail(exitBadArgs, plan.PolicyInvalid, r.cfgErr.Error())
	}
	return r, 0
}

func decide(r *repo, t *task.Task) (taskStatus, plan.Current, []store.TaskRun) {
	ts := taskStatus{Platform: t.Platform, Local: placeable(t)}
	ts.Task = t.ID
	runs, err := r.st.TaskRuns(r.id, t.ID, 50)
	if err != nil {
		ts.State, ts.Reason, ts.Detail = stateUnknown, taskError, err.Error()
		return ts, plan.Current{}, nil
	}
	if len(runs) > 0 {
		ts.LastRun = &runSummary{runs[0].ID, runs[0].State, runs[0].Started, runs[0].ExitCode, runs[0].DurationMS}
	}
	cur, _, err := r.current(t)
	switch {
	case errors.Is(err, errUntrusted):
		ts.State, ts.Reason, ts.Detail = stateUnknown, configUntrusted, err.Error()
		return ts, cur, runs
	case err != nil:
		ts.State, ts.Reason, ts.Detail = stateUnknown, taskError, err.Error()
		return ts, cur, runs
	}
	ts.Decision = plan.Local(t, cur, toPlanRuns(runs))
	if !ts.Local && ts.State != plan.Always {
		ts.Detail = "needs a " + t.Platform + " machine (this is " + task.Platform() + ")"
	}
	return ts, cur, runs
}

func cmdStatus(args []string) int {
	defer stopProbeSandboxes()
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "write one JSON envelope to stdout")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	r, code := loadForReport("status", *asJSON)
	defer r.close()
	if code != 0 {
		return code
	}
	head, dirty := headState(r.root)
	var out []taskStatus
	for _, id := range sortedIDs(r.cfg) {
		ts, _, _ := decide(r, r.cfg.Tasks[id])
		out = append(out, ts)
	}

	// 0 iff every task this machine can produce evidence for is reusable.
	code = 0
	for _, ts := range out {
		if ts.Local && ts.State != plan.Reusable && ts.State != plan.Always {
			code = exitUnsatisfied
		}
	}
	if *asJSON {
		_ = envelope("status", map[string]any{"repo": r.root, "head": head, "dirty": dirty,
			"platform": task.Platform(), "tasks": out})
		return code
	}
	fmt.Printf("%s  %s%s  %s\n\n", r.root, short(head), map[bool]string{true: " (dirty)"}[dirty], task.Platform())
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tSTATE\tREASON\tPLATFORM\tLAST RUN")
	for _, ts := range out {
		last := "-"
		if ts.LastRun != nil {
			last = fmt.Sprintf("%s ago, %s", ago(ts.LastRun.Started), ts.LastRun.State)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ts.Task, ts.State, dash(ts.Reason), ts.Platform, last)
	}
	w.Flush()
	for _, ts := range out {
		if ts.Detail != "" {
			fmt.Printf("\n%s: %s", ts.Task, ts.Detail)
		}
	}
	fmt.Println()
	if code != 0 {
		fmt.Println("\nrun `cinema explain <task>` for details")
	}
	return code
}

func cmdExplain(args []string) int {
	defer stopProbeSandboxes()
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "write one JSON envelope to stdout")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cinema explain [--json] <task>")
		return exitBadArgs
	}
	r, code := loadForReport("explain", *asJSON)
	defer r.close()
	if code != 0 {
		return code
	}
	t := r.cfg.Tasks[fs.Arg(0)]
	if t == nil {
		fmt.Fprintf(os.Stderr, "cinema explain: no task %q (have: %s)\n", fs.Arg(0), strings.Join(sortedIDs(r.cfg), ", "))
		return exitBadArgs
	}
	ts, cur, runs := decide(r, t)
	_, env, _ := r.current(t)
	var ref *store.TaskRun // the run the decision is about
	for i := range runs {
		if runs[i].ID == ts.RunID {
			ref = &runs[i]
		}
	}
	hint := hintFor(ts, t)
	code = 0
	if ts.Local && ts.State != plan.Reusable && ts.State != plan.Always {
		code = exitUnsatisfied
	}
	if *asJSON {
		data := map[string]any{"task": ts, "current": cur, "current_env_inputs": env.Inputs, "hint": hint}
		if ref != nil {
			data["run"] = ref
		}
		_ = envelope("explain", data)
		return code
	}

	fmt.Printf("%s: %s", t.ID, ts.State)
	if ts.Reason != "" {
		fmt.Printf(" (%s)", ts.Reason)
	}
	fmt.Printf("\n  argv      %s\n  cwd       %s\n  platform  %s\n", strings.Join(t.Argv, " "), t.Cwd, t.Platform)
	if t.Container != "" {
		fmt.Printf("  container %s (%s)\n", t.Container, firstWith(env.Inputs, "container "))
	}
	if t.Reuse.Allowed {
		fmt.Printf("  max_age   %s\n", t.Reuse.MaxAge)
	}
	if ts.Detail != "" {
		fmt.Printf("\n  %s\n", ts.Detail)
	}
	if ref != nil {
		fmt.Printf("\nrun %s, %s ago, exit %d, %s\n", ref.ID, ago(ref.Started), ref.ExitCode, time.Duration(ref.DurationMS)*time.Millisecond)
		check := func(name, was, now string) {
			mark := "✓"
			if was != now {
				mark = "✗"
			}
			fmt.Printf("  %s %-11s %s\n", mark, name, short(strings.TrimPrefix(was, "sha256:")))
		}
		check("task", ref.Contract, cur.Contract)
		check("environment", ref.Env, cur.Env)
		check("source", ref.SourceBefore, cur.Source)
		if ref.SourceAfter != ref.SourceBefore {
			fmt.Println("  ✗ the run changed source files while running")
		}
		if ref.Env != cur.Env && cur.Env != "" {
			fmt.Println("\n  environment then → now:")
			printEnvDiff(ref.EnvInputs, env.Inputs)
		}
	}
	if hint != "" {
		fmt.Printf("\n%s\n", hint)
	}
	return code
}

func printEnvDiff(then, now []string) {
	for _, s := range now {
		if !slices.Contains(then, s) {
			fmt.Printf("    + %s\n", s)
		}
	}
	for _, s := range then {
		if !slices.Contains(now, s) {
			fmt.Printf("    - %s\n", s)
		}
	}
}

func hintFor(ts taskStatus, t *task.Task) string {
	runIt := "run it:  cinema exec -- " + strings.Join(t.Argv, " ")
	if t.Cwd != "." {
		runIt = "run it from " + t.Cwd + "/:  cinema exec -- " + strings.Join(t.Argv, " ")
	}
	switch {
	case ts.Reason == configUntrusted:
		return "review " + task.ConfigPath + ", then: cinema trust"
	case !ts.Local && ts.State != plan.Always:
		return "produce it on a " + t.Platform + " machine (plan §9), or CI will run it"
	case ts.Reason == plan.SourceChangedDuringRun:
		return "the command modified files; commit or discard the changes, then " + runIt
	case ts.State == plan.Reusable || ts.State == plan.Always:
		return ""
	}
	return runIt
}

func cmdTrust(args []string) int {
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	useConfig(*cfgPath)
	r, code := loadForReport("trust", false)
	defer r.close()
	if code != 0 {
		return code
	}
	var cmds []string
	for _, id := range sortedIDs(r.cfg) {
		for _, tc := range r.cfg.Tasks[id].Toolchain {
			if len(tc.Cmd) > 0 {
				cmds = append(cmds, fmt.Sprintf("  %s: %s", id, strings.Join(tc.Cmd, " ")))
			}
		}
	}
	if err := r.st.TrustConfig(r.id, r.cfg.Digest); err != nil {
		fmt.Fprintf(os.Stderr, "cinema trust: %v\n", err)
		return exitInternal
	}
	fmt.Printf("trusted %s (%s)\n", task.ConfigPath, short(strings.TrimPrefix(r.cfg.Digest, "sha256:")))
	if len(cmds) > 0 {
		fmt.Printf("cinema may now run these toolchain commands:\n%s\n", strings.Join(cmds, "\n"))
	}
	fmt.Println("any change to the file needs `cinema trust` again")
	return 0
}

func sortedIDs(c *task.Config) []string {
	ids := make([]string, 0, len(c.Tasks))
	for id := range c.Tasks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "-"
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String()
	case d < time.Hour:
		return d.Round(time.Minute).String()
	case d < 48*time.Hour:
		return d.Round(time.Hour).String()
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// firstWith is the first of ss with prefix p, trimmed, or "unresolved".
func firstWith(ss []string, p string) string {
	for _, x := range ss {
		if v, ok := strings.CutPrefix(x, p); ok {
			return v
		}
	}
	return "unresolved"
}

func placeable(t *task.Task) bool { _, ok := place(t); return ok }
