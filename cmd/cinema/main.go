// Command cinema records the checks you already run (plan.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/record"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// Exit codes for everything except exec, which returns the child's code (plan §10.2).
const exitBadArgs = 2

const usage = `usage: cinema <command> [args]

commands:
  exec [--record-json PATH] [--timeout D] -- CMD [ARGS...]   record an arbitrary command
`

func main() {
	// ponytail: switch + flag.FlagSet per verb; Cobra only if this stops being readable.
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitBadArgs)
	}
	switch os.Args[1] {
	case "exec":
		os.Exit(cmdExec(os.Args[2:]))
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "cinema: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(exitBadArgs)
	}
}

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

	// Recording problems are reported, never fatal: the child runs and its exit
	// code is returned regardless (plan §4.1, ACC-006).
	st, dataDir, runID, recErr := beginRecording(argv)
	if st != nil {
		defer st.Close()
	}
	fp := newFingerprinter(dataDir)
	src := fp.before()
	match := matchTask(src, argv)

	res, err := record.Run(ctx, opts)
	if err != nil && res.Start.IsZero() {
		if runID != "" {
			_ = st.AbortRun(runID)
		}
		fmt.Fprintf(os.Stderr, "cinema exec: %v\n", err)
		return 127 // shell convention for "command not found / not executable"
	}
	if err != nil {
		// The row stays "recording" and becomes "incomplete": never signable.
		fmt.Fprintf(os.Stderr, "cinema exec: supervision error, run not recorded: %v\n", err)
		return res.ExitCode
	}
	if res.Leftovers {
		fmt.Fprintln(os.Stderr, "cinema exec: stopped processes the command left running")
	}
	src.After = fp.after()
	if src.Before != "" && src.After != "" && src.Before != src.After {
		fmt.Fprintln(os.Stderr, "cinema exec: the command changed source files; this run can't certify the tree")
	}
	state := ""
	if recErr == nil {
		state, recErr = st.FinishRun(runID, res, src, match)
	}
	if recErr != nil {
		fmt.Fprintf(os.Stderr, "cinema exec: run not recorded: %v\n", recErr)
	}
	if *recordJSON != "" {
		if err := writeJSON(*recordJSON, runID, state, src, match, res); err != nil {
			fmt.Fprintf(os.Stderr, "cinema exec: --record-json: %v\n", err)
		}
	}
	if st != nil {
		if _, err := st.Maintain(false); err != nil {
			fmt.Fprintf(os.Stderr, "cinema exec: cleanup: %v\n", err)
		}
	}
	return res.ExitCode
}

func beginRecording(argv []string) (st *store.Store, dataDir, runID string, err error) {
	if dataDir, err = store.DataDir(); err != nil {
		return nil, "", "", err
	}
	if st, err = store.Open(dataDir); err != nil {
		return nil, dataDir, "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return st, dataDir, "", err
	}
	runID, err = st.BeginRun(argv, cwd)
	return st, dataDir, runID, err
}

// matchTask finds the task this exact argv+cwd is, and captures its contract
// and environment digests now, while the toolchain is the one the run uses.
// Problems are reported; the run is then history only, never task evidence.
func matchTask(src store.Source, argv []string) store.Match {
	if src.RepoRoot == "" {
		return store.Match{}
	}
	cfg, err := task.Load(src.RepoRoot)
	if errors.Is(err, task.ErrNoConfig) {
		return store.Match{}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema exec: %s is invalid (POLICY_INVALID): %v\n", task.ConfigPath, err)
		return store.Match{}
	}
	t := cfg.Match(argv, src.CwdRel)
	if t == nil {
		return store.Match{}
	}
	m := store.Match{TaskID: t.ID}
	if m.ContractDigest, err = t.ContractDigest(src.RepoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "cinema exec: task %s: %v\n", t.ID, err)
		return store.Match{}
	}
	env, err := t.EnvFingerprint(src.RepoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema exec: task %s: environment: %v\n", t.ID, err)
		return store.Match{}
	}
	m.EnvDigest = env.Digest
	if t.VCSContext == "exact" {
		m.VCS = headCommit(src.RepoRoot)
	}
	return m
}

// headCommit is HEAD's SHA for a clean tree, or "dirty": a dirty tree isn't any
// commit, so vcs_context: exact tasks can't reuse it (plan §5.3).
func headCommit(root string) string {
	status, err := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=normal").Output()
	if err != nil || len(status) > 0 {
		return "dirty"
	}
	head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return "dirty"
	}
	return strings.TrimSpace(string(head))
}

// fingerprinter snapshots the enclosing git repo before and after a run.
// Failures are reported and leave the fingerprint empty (never eligible).
type fingerprinter struct {
	root, cwdRel, cachePath string
	cache                   *snapshot.Cache
}

func newFingerprinter(dataDir string) *fingerprinter {
	cwd, err := os.Getwd()
	if err != nil {
		return &fingerprinter{}
	}
	root, err := snapshot.RepoRoot(cwd)
	if err != nil {
		return &fingerprinter{} // not in a git repo: history only
	}
	// RepoRoot resolves symlinks (macOS /tmp → /private/tmp); match it.
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		fmt.Fprintf(os.Stderr, "cinema exec: can't place %s inside repo %s\n", cwd, root)
		return &fingerprinter{}
	}
	f := &fingerprinter{root: root, cwdRel: filepath.ToSlash(rel)}
	if dataDir != "" {
		f.cachePath = snapshot.CachePath(dataDir, root)
		f.cache = snapshot.LoadCache(f.cachePath)
	}
	return f
}

func (f *fingerprinter) before() store.Source {
	return store.Source{RepoRoot: f.root, CwdRel: f.cwdRel, Before: f.take()}
}

func (f *fingerprinter) after() string {
	d := f.take()
	if f.cachePath != "" {
		_ = f.cache.Save(f.cachePath) // a lost cache only costs re-hashing
	}
	return d
}

func (f *fingerprinter) take() string {
	if f.root == "" {
		return ""
	}
	s, err := snapshot.Take(f.root, nil, f.cache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema exec: fingerprint: %v\n", err)
		return ""
	}
	return s.Digest
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
