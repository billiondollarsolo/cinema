package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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
	r, repoErr := openRepo()
	defer r.close()
	res, runID, state, src, match := execRecorded(ctx, opts, r, repoErr, nil)
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
//
// want is the task being run (`cinema run`); nil means find it by argv (`cinema exec`).
func execRecorded(ctx context.Context, opts record.Options, r *repo, repoErr error, want *task.Task) (res record.Result, runID, state string, src store.Source, match store.Match) {
	warn := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "cinema: "+format+"\n", a...) }
	recErr := r.stErr
	if recErr == nil {
		cwd := opts.Dir
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
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
	case want != nil:
		t = want
	case r.cfg != nil:
		t = r.cfg.Match(opts.Argv, r.cwdRel)
	}
	if r.root != "" {
		src = store.Source{RepoRoot: r.id, CwdRel: r.cwdRel, Capture: r.capture}
		var extra, outputs []string
		if t != nil {
			extra, outputs = t.ExtraInputs, t.Outputs
		}
		var err error
		if src.Before, err = r.source(extra, outputs); err != nil {
			warn("fingerprint: %v", err)
		}
		if t != nil && t.Runtime.Go+t.Runtime.Node != "" && opts.Env == nil {
			// The task pins a runtime that cinema provisions; a bare exec runs with
			// whatever is installed, so its fingerprint wouldn't describe the run.
			warn("task %s pins a runtime; run it with `cinema run %s` to make evidence (this run is history only)", t.ID, t.ID)
			t = nil
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

	// tasks.yaml is outside the source fingerprint (ADR 0001), so watch it here:
	// a command that rewrites the file defining what's required (a repo-wide
	// formatter, say) must not count as evidence (lab finding: vite's oxfmt).
	cfgBefore := configHash(r.root)
	detector := &task.CacheDetector{}
	opts.Watch = detector
	res, err := record.Run(ctx, opts)
	detector.Done()
	if match.TaskID != "" {
		match.Origin = "fresh"
		if detector.Cached {
			match.Origin = "tool_cache"
			if t != nil && !t.AcceptToolCache {
				warn("go test replayed cached results; this run can't count as a fresh %s (add -count=1)", t.ID)
			}
		}
	}
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
		var extra, outputs []string
		if t != nil {
			extra, outputs = t.ExtraInputs, t.Outputs
		}
		if src.After, err = r.source(extra, outputs); err != nil {
			warn("fingerprint: %v", err)
		}
		if configHash(r.root) != cfgBefore {
			warn("the command changed %s; this run can't count as evidence", task.ConfigPath)
			src.After = "" // never equal to Before: not evidence (plan: incomplete record)
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

// configHash is the content hash of tasks.yaml ("" if absent or unreadable).
func configHash(root string) string {
	if root == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(root, task.ConfigPath))
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
