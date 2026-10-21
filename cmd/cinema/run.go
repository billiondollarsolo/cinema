package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/record"
	"github.com/billiondollarsolo/cinema/internal/task"
)

const tmpPrefix = "cinema-run-"

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	all := fs.Bool("all", false, "run every task this machine can satisfy that isn't already reusable")
	force := fs.Bool("force", false, "with --all: rerun tasks that are already reusable")
	commit := fs.String("commit", "", "run in a clean checkout of `REV` instead of the working tree")
	jobs := fs.Int("j", 1, "with --commit: run up to `N` tasks at once, each in its own clean checkout")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	if *all == (fs.NArg() > 0) || *jobs < 1 {
		fmt.Fprintln(os.Stderr, "usage: cinema run [--commit REV [-j N]] <task>... | --all [--force]")
		return exitBadArgs
	}
	if *jobs > 1 && *commit == "" {
		fmt.Fprintln(os.Stderr, "cinema run: -j needs --commit: parallel tasks get their own clean checkouts, never your working tree")
		return exitBadArgs
	}
	r, code := loadForReport("run", false)
	defer r.close()
	if code != 0 {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Where tasks run: the working tree, or a clean checkout of a commit.
	w := r
	if *commit != "" {
		sweepRunDirs()
		cr, cleanup, err := cleanCheckout(r, *commit)
		defer cleanup()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cinema run: %v\n", err)
			return exitBadArgs
		}
		w = cr
		switch {
		case errors.Is(w.cfgErr, task.ErrNoConfig):
			fmt.Fprintf(os.Stderr, "cinema run: %s has no %s\n", *commit, task.ConfigPath)
			return exitBadArgs
		case w.cfgErr != nil:
			fmt.Fprintf(os.Stderr, "cinema run: %s at %s: %v\n", task.ConfigPath, *commit, w.cfgErr)
			return exitBadArgs
		}
	}

	var todo []*task.Task
	if *all {
		for _, id := range sortedIDs(w.cfg) {
			t := w.cfg.Tasks[id]
			if t.Platform != task.PlatformAny && t.Platform != task.Platform() {
				continue // can't produce evidence here
			}
			if !t.Reuse.Allowed {
				continue // always runs in CI; local result wouldn't count
			}
			if !*force {
				if ts, _, _ := decide(w, t); ts.State == plan.Reusable {
					fmt.Printf("- %s  already reusable\n", t.ID)
					continue
				}
			}
			todo = append(todo, t)
		}
	} else {
		for _, id := range fs.Args() {
			t := w.cfg.Tasks[id]
			if t == nil {
				fmt.Fprintf(os.Stderr, "cinema run: no task %q (have: %s)\n", id, strings.Join(sortedIDs(w.cfg), ", "))
				return exitBadArgs
			}
			if t.Platform != task.PlatformAny && t.Platform != task.Platform() {
				fmt.Fprintf(os.Stderr, "cinema run: %s needs %s; running anyway, but it won't count as evidence here\n", id, t.Platform)
			}
			todo = append(todo, t)
		}
	}

	var failed int
	if *jobs > 1 {
		failed = runParallel(ctx, r, *commit, todo, *jobs)
	} else {
		failed = runTasks(ctx, w, todo)
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "cinema run: interrupted")
		return exitUnsatisfied
	}
	if len(todo) == 0 {
		fmt.Println("nothing to run")
	}
	if failed > 0 {
		return exitUnsatisfied
	}
	return 0
}

// runTasks runs each task's exact argv from its cwd in w, recording every run,
// and prints one result line per task. Returns how many failed.
func runTasks(ctx context.Context, w *repo, todo []*task.Task) int {
	failed := 0
	for _, t := range todo {
		if ctx.Err() != nil {
			break
		}
		bad, fatal := runOne(ctx, w, t, nil)
		if bad {
			failed++
		}
		if fatal {
			return failed + len(todo)
		}
	}
	return failed
}

// runParallel runs todo with up to j workers, each in its own clean checkout of
// rev, so parallel tasks never see each other's tree writes. Task output goes to
// the run record (dashboard, explain), not the terminal; failures print a tail.
func runParallel(ctx context.Context, r *repo, rev string, todo []*task.Task, j int) int {
	sweepRunDirs()
	queue := make(chan *task.Task)
	var mu sync.Mutex
	failed := 0
	var wg sync.WaitGroup
	for range min(j, len(todo)) {
		wg.Go(func() {
			cr, cleanup, err := cleanCheckout(r, rev)
			defer cleanup()
			for t := range queue {
				if err != nil {
					mu.Lock()
					fmt.Printf("✗ %s  no clean checkout: %v\n", t.ID, err)
					failed++
					mu.Unlock()
					continue
				}
				bad, fatal := runOne(ctx, cr, t, &mu)
				mu.Lock()
				if bad {
					failed++
				}
				mu.Unlock()
				if fatal {
					err = errors.New("checkout could not be restored")
				}
			}
		})
	}
	for _, t := range todo {
		if ctx.Err() != nil {
			break
		}
		queue <- t
	}
	close(queue)
	wg.Wait()
	return failed
}

// runOne runs a single task in w. printMu non-nil means parallel mode: output is
// captured only, and lines are printed under the lock. fatal: w can't be reused.
func runOne(ctx context.Context, w *repo, t *task.Task, printMu *sync.Mutex) (bad, fatal bool) {
	say := func(f string, a ...any) {
		if printMu != nil {
			printMu.Lock()
			defer printMu.Unlock()
		}
		fmt.Printf(f, a...)
	}
	opts := record.Options{Argv: t.Argv, Dir: filepath.Join(w.root, filepath.FromSlash(t.Cwd))}
	if printMu == nil {
		fmt.Fprintf(os.Stderr, "▶ %s: %s\n", t.ID, strings.Join(t.Argv, " "))
		opts.Stdout, opts.Stderr = os.Stdout, os.Stderr
	} else {
		say("▶ %s\n", t.ID)
	}
	res, _, state, src, match := execRecorded(ctx, opts, w.at(t.Cwd), nil)
	mark, note := "✓", ""
	switch {
	case res.Start.IsZero():
		mark, note = "✗", "didn't start"
	case res.ExitCode != 0:
		mark, note = "✗", fmt.Sprintf("exit %d", res.ExitCode)
	case src.Before != src.After:
		mark, note = "!", "passed, but changed source files: not evidence"
	case match.TaskID == "":
		mark, note = "!", "passed, not recorded as task evidence (see warnings above)"
	case match.Origin == "tool_cache" && !t.AcceptToolCache:
		mark, note = "!", "passed from go test's cache: not fresh evidence"
	case state == "":
		mark, note = "!", "passed, not recorded"
	}
	bad = mark == "✗"
	tail := ""
	if bad && printMu != nil {
		tail = lastLines(append(res.Stdout.Bytes(), res.Stderr.Bytes()...), 25)
	}
	say("%s %s  %s %s\n%s", mark, t.ID, res.Duration.Round(time.Millisecond), note, tail)
	if w.capture == "clean" {
		// A task that writes into the tree (a build deleting a tracked file, a
		// declared output) must not change what the next task sees. Ignored
		// files (dependency installs, build caches) are kept.
		if err := restoreTree(w.root); err != nil {
			fmt.Fprintf(os.Stderr, "cinema run: restoring the clean checkout: %v\n", err)
			return bad, true // can't trust anything after this
		}
	}
	return bad, false
}

// lastLines returns the final n lines of b, indented, for a failure summary.
func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "    " + strings.Join(lines, "\n    ") + "\n"
}

// restoreTree resets a clean checkout to its commit: tracked files restored,
// untracked unignored files removed, ignored files kept. Only ever called on
// cinema's own temp checkouts, never on a user's working tree.
func restoreTree(root string) error {
	if _, err := gitOut(root, "checkout", "-q", "--", "."); err != nil {
		return err
	}
	_, err := gitOut(root, "clean", "-fdq")
	return err
}

// at is r positioned in a task's cwd, sharing the store and cache.
func (r *repo) at(cwdRel string) *repo {
	c := *r
	c.cwdRel = cwdRel
	c.snaps = map[string]string{}
	return &c
}

// cleanCheckout materializes rev in a temp dir with exact bytes: a shared clone
// (objects borrowed via alternates, nothing written to the real repo) plus a
// detached checkout. Not `git archive`: export-ignore / export-subst attributes
// would change the bytes, and the evidence would describe different code.
// cleanup is always safe to call and removes the dir (plan §13.1).
func cleanCheckout(r *repo, rev string) (*repo, func(), error) {
	noop := func() {}
	sha, err := gitOut(r.root, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return nil, noop, fmt.Errorf("unknown commit %q", rev)
	}
	dir, err := os.MkdirTemp("", tmpPrefix+strconv.Itoa(os.Getpid())+"-*")
	if err != nil {
		return nil, noop, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	if _, err := gitOut(r.root, "clone", "-q", "--shared", "--no-checkout", "--", r.root, dir); err != nil {
		return nil, cleanup, err
	}
	if _, err := gitOut(dir, "checkout", "-q", "--detach", sha); err != nil {
		return nil, cleanup, err
	}
	cr, err := openRepoAt(dir)
	if err != nil {
		return nil, cleanup, err
	}
	if cr.st != nil {
		inner := cleanup
		cleanup = func() { cr.st.Close(); inner() }
	}
	cr.id, cr.capture = r.id, "clean"
	cr.cache, cr.cachePath = nil, "" // a cache keyed on a temp dir would be junk
	fmt.Fprintf(os.Stderr, "cinema run: clean checkout of %s\n", sha[:12])
	return cr, cleanup, nil
}

// sweepRunDirs removes clean-checkout dirs left by a cinema that died
// (plan §13.1, CLEAN-002): owner PID gone and older than a minute.
func sweepRunDirs() {
	tmp := os.TempDir()
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), tmpPrefix)
		if !ok || !e.IsDir() {
			continue
		}
		pidStr, _, _ := strings.Cut(rest, "-")
		pid, err := strconv.Atoi(pidStr)
		info, ierr := e.Info()
		if err != nil || ierr != nil || time.Since(info.ModTime()) < time.Minute {
			continue
		}
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	}
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}
