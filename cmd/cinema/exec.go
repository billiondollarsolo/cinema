package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/billiondollarsolo/cinema/internal/record"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
)

func cmdExec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	recordJSON := fs.String("record-json", "", "write run metadata to `PATH` (never to stdout)")
	timeout := fs.Duration("timeout", 0, "terminate the process group after `D` (0 = none)")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, "cinema exec: missing command (use: cinema exec -- CMD ARGS...)\n")
		return exitBadArgs
	}
	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	opts := record.Options{Argv: argv, Stdout: os.Stdout, Stderr: os.Stderr}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		opts.Stdin = os.Stdin // pipes and files pass through; a terminal does not (pipe mode, plan §4.1)
	}
	res, runID, state, src, match := execRecorded(ctx, opts)
	if res.Start.IsZero() {
		return 127 // shell convention for "command not found / not executable"
	}
	if *recordJSON != "" {
		if err := writeJSON(*recordJSON, runID, state, src, match, res); err != nil {
			fmt.Fprintf(os.Stderr, "cinema exec: --record-json: %v\n", err)
		}
	}
	return res.ExitCode
}

// execRecorded runs argv under supervision and records it. Recording problems
// are reported on stderr and never change the child's behavior or exit code
// (plan §4.1, ACC-006). A zero res.Start means the child never started.
func execRecorded(ctx context.Context, opts record.Options) (res record.Result, runID, state string, src store.Source, match store.Match) {
	warn := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "cinema exec: "+format+"\n", a...) }
	r, repoErr := openRepo()
	defer r.close()

	recErr := r.stErr
	if recErr == nil {
		cwd, _ := os.Getwd()
		runID, recErr = r.st.BeginRun(opts.Argv, cwd)
	}

	var t *task.Task
	switch {
	case repoErr != nil && !errors.Is(repoErr, snapshot.ErrNotRepo):
		warn("%v", repoErr)
	case r.root == "":
		// outside a git repo: history only
	case r.cfgErr != nil && !errors.Is(r.cfgErr, task.ErrNoConfig):
		warn("%s is invalid (POLICY_INVALID): %v", task.ConfigPath, r.cfgErr)
	case r.cfg != nil:
		t = r.cfg.Match(opts.Argv, r.cwdRel)
	}
	if r.root != "" {
		src = store.Source{RepoRoot: r.root, CwdRel: r.cwdRel}
		var extra []string
		if t != nil {
			extra = t.ExtraInputs
		}
		var err error
		if src.Before, err = r.source(extra); err != nil {
			warn("fingerprint: %v", err)
		}
		if t != nil && src.Before != "" {
			// Capture contract + environment now: the toolchain the run actually uses.
			cur, env, err := r.current(t)
			if err != nil {
				warn("task %s: %v; this run is history only", t.ID, err)
			} else {
				match = store.Match{TaskID: t.ID, ContractDigest: cur.Contract, EnvDigest: cur.Env, VCS: cur.VCS, EnvInputs: env.Inputs}
			}
		}
	}

	res, err := record.Run(ctx, opts)
	if err != nil && res.Start.IsZero() {
		if runID != "" {
			_ = r.st.AbortRun(runID)
		}
		warn("%v", err)
		return res, "", "", src, match
	}
	if err != nil {
		// The row stays "recording" and becomes "incomplete": never signable.
		warn("supervision error, run not recorded: %v", err)
		return res, runID, "", src, match
	}
	if res.Leftovers {
		warn("stopped processes the command left running")
	}
	if r.root != "" {
		r.fresh()
		var extra []string
		if t != nil {
			extra = t.ExtraInputs
		}
		if src.After, err = r.source(extra); err != nil {
			warn("fingerprint: %v", err)
		}
		if src.Before != "" && src.After != "" && src.Before != src.After {
			warn("the command changed source files; this run can't certify the tree")
		}
	}
	if recErr == nil {
		state, recErr = r.st.FinishRun(runID, res, src, match)
	}
	if recErr != nil {
		warn("run not recorded: %v", recErr)
	}
	if r.st != nil {
		if _, err := r.st.Maintain(false); err != nil {
			warn("cleanup: %v", err)
		}
	}
	return res, runID, state, src, match
}

func writeJSON(path, runID, state string, src store.Source, m store.Match, res record.Result) error {
	b, err := json.MarshalIndent(struct {
		SchemaVersion int    `json:"schema_version"`
		RunID         string `json:"run_id,omitempty"` // empty when the run wasn't recorded
		State         string `json:"state,omitempty"`
		RepoRoot      string `json:"repo_root,omitempty"`
		CwdRel        string `json:"cwd_rel,omitempty"`
		SourceBefore  string `json:"source_before,omitempty"`
		SourceAfter   string `json:"source_after,omitempty"`
		TaskID        string `json:"task_id,omitempty"` // exact argv+cwd match in .cinema/tasks.yaml
		record.Result
		DurationMS int64 `json:"duration_ms"`
	}{1, runID, state, src.RepoRoot, src.CwdRel, src.Before, src.After, m.TaskID, res, res.Duration.Milliseconds()}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
