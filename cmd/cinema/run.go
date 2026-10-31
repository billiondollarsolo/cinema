package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/plan"
	"github.com/billiondollarsolo/cinema/internal/record"
	"github.com/billiondollarsolo/cinema/internal/store"
	"github.com/billiondollarsolo/cinema/internal/task"
	"github.com/billiondollarsolo/cinema/internal/testreport"
)

const tmpPrefix = "cinema-run-"

func cmdRun(args []string) int {
	defer stopProbeSandboxes()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	all := fs.Bool("all", false, "run every task this machine can satisfy that isn't already reusable")
	force := fs.Bool("force", false, "with --all: rerun tasks that are already reusable")
	commit := fs.String("commit", "", "run in a clean checkout of `REV` instead of the working tree")
	cfgPath := configFlag(fs)
	jobs := fs.Int("j", 1, "with --commit: run up to `N` tasks at once, each in its own clean checkout")
	failfast := fs.Bool("failfast", false, "start no more tasks after one fails (running ones finish)")
	if err := fs.Parse(args); err != nil {
		return exitBadArgs
	}
	useConfig(*cfgPath)
	if *all == (fs.NArg() > 0) || *jobs < 1 {
		fmt.Fprintln(os.Stderr, "usage: cinema run [--config FILE] [--failfast] [--commit REV [-j N]] <task>... | --all [--force]")
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
			fmt.Fprintf(os.Stderr, "cinema run: %s has no %s: commit it, or pass --config %s to use the working tree's\n", *commit, task.ConfigPath, task.ConfigPath)
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
			if _, ok := place(t); !ok {
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
			if _, ok := place(t); !ok {
				if strings.HasPrefix(t.Platform, "linux/") {
					// Running it as something else only fails confusingly, or worse, passes.
					fmt.Fprintf(os.Stderr, "cinema run: %s needs %s, and this machine can't run it: %s\n", id, t.Platform, placeHint(t))
					return exitUnsatisfied
				}
				fmt.Fprintf(os.Stderr, "cinema run: %s needs %s; running anyway, but it won't count as evidence here\n", id, t.Platform)
			}
			todo = append(todo, t)
		}
	}

	var failed int
	if *jobs > 1 {
		failed = runParallel(ctx, r, *commit, todo, *jobs, *failfast)
	} else {
		failed = runTasks(ctx, w, todo, *failfast)
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
func runTasks(ctx context.Context, w *repo, todo []*task.Task, failfast bool) int {
	failed := 0
	for i, t := range todo {
		if ctx.Err() != nil {
			break
		}
		if failfast && failed > 0 {
			notStarted(todo[i:])
			break
		}
		bad, fatal, _ := runOne(ctx, w, t, nil, 0)
		if bad {
			failed++
		}
		if fatal {
			return failed + len(todo)
		}
	}
	return failed
}

// notStarted reports the tasks --failfast didn't start.
func notStarted(ts []*task.Task) {
	for _, t := range ts {
		fmt.Printf("- %s  not started: --failfast after a failure\n", t.ID)
	}
}

// runParallel runs todo with up to j workers, each in its own clean checkout of
// rev, so parallel tasks never see each other's tree writes. Task output goes to
// the run record (dashboard, explain), not the terminal; failures print a tail.
//
// failfast: after a failure no task starts, and those running finish: their
// results are recorded (a pass is reusable evidence), where cancelling them
// would throw the work away as "interrupted" to save only their remaining time.
func runParallel(ctx context.Context, r *repo, rev string, todo []*task.Task, j int, failfast bool) int {
	// Exclusive tasks (fixed ports, a fixed docker network) run alone, after the
	// parallel-safe ones, one at a time.
	var shared, alone []*task.Task
	for _, t := range todo {
		if t.Exclusive || len(t.Services) > 0 { // services publish fixed host ports
			alone = append(alone, t)
		} else {
			shared = append(shared, t)
		}
	}
	failed := runPool(ctx, r, rev, shared, j, failfast)
	switch {
	case failfast && failed > 0:
		notStarted(alone)
	case len(alone) > 0 && ctx.Err() == nil:
		failed += runPool(ctx, r, rev, alone, 1, failfast)
	}
	return failed
}

func runPool(ctx context.Context, r *repo, rev string, todo []*task.Task, j int, failfast bool) int {
	if len(todo) == 0 {
		return 0
	}
	sweepRunDirs()
	queue := make(chan *task.Task)
	var mu sync.Mutex
	failed := 0
	var wg sync.WaitGroup
	for slot := range min(j, len(todo)) {
		wg.Go(func() {
			cr, cleanup, err := cleanCheckout(r, rev)
			defer cleanup()
			for t := range queue {
				mu.Lock()
				stop := failfast && failed > 0 // handed out just as another task failed
				if stop {
					notStarted([]*task.Task{t})
				}
				mu.Unlock()
				if stop {
					continue
				}
				if err != nil {
					mu.Lock()
					fmt.Printf("✗ %s  no clean checkout: %v\n", t.ID, err)
					failed++
					mu.Unlock()
					continue
				}
				bad, fatal, _ := runOne(ctx, cr, t, &mu, slot)
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
	for i, t := range todo {
		if ctx.Err() != nil {
			break
		}
		mu.Lock()
		stop := failfast && failed > 0
		mu.Unlock()
		if stop {
			notStarted(todo[i:])
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
// slot is the worker's index, so parallel tasks with cpus: get different CPUs.
// tests is the run's parsed per-test results (nil: none found, or it never ran).
func runOne(ctx context.Context, w *repo, t *task.Task, printMu *sync.Mutex, slot int) (bad, fatal bool, tests *testreport.Summary) {
	say := func(f string, a ...any) {
		if printMu != nil {
			printMu.Lock()
			defer printMu.Unlock()
		}
		fmt.Printf(f, a...)
	}
	opts := record.Options{Argv: t.Argv, Dir: filepath.Join(w.root, filepath.FromSlash(t.Cwd))}
	opts.CPUs = cpuSet(t.CPUs, slot, runtime.NumCPU())
	network, stopServices, err := t.StartServices()
	if err != nil {
		say("✗ %s  %v\n", t.ID, err)
		return true, false, nil
	}
	defer stopServices()
	pl, _ := place(t)
	if pl.sandbox {
		// A runner-like container (plan §9.2). Only from a clean checkout: the
		// bind-mounted tree must be exactly the commit.
		if w.capture != "clean" {
			say("✗ %s  runs in a container only from a clean checkout: use --commit\n", t.ID)
			return true, false, nil
		}
		// ponytail: services are reachable by name from the sandbox, not on
		// localhost as on a runner host; tasks that use localhost ports fail
		// (never pass) until the sandbox shares the services' network namespace.
		bins, vars, caches, err := t.RuntimeSandbox(w.dataDir, "linux", strings.TrimPrefix(pl.platform, "linux/"))
		if err != nil {
			say("✗ %s  runtime: %v\n", t.ID, err)
			return true, false, nil
		}
		art, _ := artifactsRoot()
		cpus, mem := t.RunnerSize()
		sb, err := task.StartSandbox(ctx, task.SandboxSpec{Image: runnerImage(t.RunsOn), Platform: pl.platform, Emulated: pl.emulated,
			Root: w.root, RepoName: filepath.Base(w.id), RepoID: w.id, DataDir: w.dataDir, Bins: bins, Vars: vars, Caches: caches, Artifacts: art,
			Objects: filepath.Join(w.id, ".git", "objects"),
			CPUs:    cpus, MemoryMB: mem * 1024, Network: network})
		if err != nil {
			say("✗ %s  %v\n", t.ID, err)
			return true, false, nil
		}
		defer sb.Stop()
		// Allowlisted env inputs keep their host values, as fingerprinted. Names
		// only: docker exec takes the values from its own environment, so they
		// never appear in an argv (or the run history that records it).
		var extra []string
		for _, n := range t.Env {
			if _, ok := os.LookupEnv(n); ok {
				extra = append(extra, n)
			}
		}
		opts.Argv, opts.Env, opts.CPUs = sb.Exec(t.Cwd, t.Argv, extra), os.Environ(), nil
		c := *w
		c.where = sb.Where()
		w = &c
	}
	if t.Container != "" {
		img, err := task.PinnedImage(t.Container) // the same digest the fingerprint records
		if err != nil {
			say("✗ %s  container: %v\n", t.ID, err)
			return true, false, nil
		}
		var stop func()
		opts.Argv, stop = t.ContainerRun(w.root, img, network, opts.CPUs, pl.platform)
		c := *w
		c.where = containerWhere(t, pl)
		w = &c
		defer stop()
		opts.CPUs = nil
	}
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}
	if !pl.sandbox {
		renv, err := t.RuntimeEnv(w.dataDir)
		if err != nil {
			say("✗ %s  runtime: %v\n", t.ID, err)
			return true, false, nil
		}
		opts.Env = renv // provisioned runtime (nil: inherit)
		if opts.Env == nil {
			opts.Env = os.Environ()
		}
	}
	if w.capture == "clean" && !pl.sandbox {
		// A private TMPDIR, removed afterwards: tools killed mid-run (go's
		// /tmp/go-build* work dirs) can't leave junk behind (plan §13.1). Only in
		// cinema's own checkouts; `cinema exec` keeps the caller's environment.
		if tmp, err := os.MkdirTemp("", "cinema-task-*"); err == nil {
			defer os.RemoveAll(tmp)
			opts.Env = task.PrivateGlobals(append(opts.Env, "TMPDIR="+tmp), tmp)
		}
		// Artifacts a task rebuilds from another job (analyze/artifact.go) are
		// shared by tasks at the same commit; only clean checkouts, whose tree
		// is exactly HEAD, may use them.
		if dir, err := artifactsRoot(); err == nil {
			opts.Env = append(opts.Env, "CINEMA_ARTIFACTS="+dir)
		}
	}
	if printMu == nil {
		fmt.Fprintf(os.Stderr, "▶ %s: %s\n", t.ID, strings.Join(t.Argv, " "))
		opts.Stdout, opts.Stderr = os.Stdout, os.Stderr
	} else {
		say("▶ %s\n", t.ID)
	}
	res, _, state, src, match, tr := execRecorded(ctx, opts, w.at(t.Cwd), nil, t)
	mark, note := "✓", ""
	switch {
	case res.Start.IsZero():
		mark, note = "✗", "didn't start"
	case res.TimedOut:
		mark, note = "✗", fmt.Sprintf("timed out after %s", t.Timeout)
	case res.ExitCode != 0:
		mark, note = "✗", fmt.Sprintf("exit %d", res.ExitCode)
	case src.Before != src.After:
		mark, note = "!", "passed, but changed source files: not evidence"
		if w.capture == "clean" { // the slot was clean before the task, so this is exactly what it wrote
			if changed := changedPaths(w.root); len(changed) > 0 {
				note += fmt.Sprintf("\n    changed: %s\n    if the task is meant to write them, declare them in outputs:", strings.Join(changed, ", "))
			}
		}
	case match.TaskID == "":
		mark, note = "!", "passed, not recorded as task evidence (see warnings above)"
	case match.Origin == "tool_cache" && !t.AcceptToolCache:
		mark, note = "!", "passed from go test's cache: not fresh evidence"
	case state == "":
		mark, note = "!", "passed, not recorded"
	}
	bad = mark == "✗"
	if bad {
		note += failedTests(tr.Summary, "\n    ", 10) + missingNote(append(res.Stdout.Bytes(), res.Stderr.Bytes()...))
	}
	tail := ""
	if bad && printMu != nil {
		tail = lastLines(append(res.Stdout.Bytes(), res.Stderr.Bytes()...), 25)
	}
	say("%s %s  %s %s\n%s", mark, t.ID, res.Duration.Round(time.Millisecond), note, tail)
	if w.capture == "clean" {
		// A task that writes into the tree (a build deleting a tracked file, a
		// declared output, node_modules, dist/) must not change what the next
		// task sees: each starts like a CI job, from a fresh checkout. Keeping
		// ignored files let a task pass on another task's build (lab: eslint).
		if err := restoreTree(w.root); err != nil {
			fmt.Fprintf(os.Stderr, "cinema run: restoring the clean checkout: %v\n", err)
			return bad, true, tr.Summary // can't trust anything after this
		}
	}
	return bad, false, tr.Summary
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
// untracked and ignored files removed. Only ever called on cinema's own
// checkouts, never on a user's working tree.
func restoreTree(root string) error {
	if _, err := gitOut(root, "checkout", "-q", "--", "."); err != nil {
		return err
	}
	_, err := gitOut(root, "clean", "-ffdxq")
	return err
}

// changedPaths lists tracked changes and untracked, unignored files (the
// fingerprinted set), at most 10.
func changedPaths(root string) []string {
	// -z, untrimmed: each entry is "XY path" and X may be a space.
	out, err := exec.Command("git", "-C", root, "status", "--porcelain", "-z", "--untracked-files=no", "--no-renames").Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range strings.Split(string(out), "\x00") {
		if len(e) > 3 {
			paths = append(paths, analyze.Printable(e[3:]))
		}
	}
	if len(paths) > 10 {
		paths = append(paths[:10], fmt.Sprintf("… %d more", len(paths)-10))
	}
	return paths
}

// at is r positioned in a task's cwd, sharing the store and cache.
func (r *repo) at(cwdRel string) *repo {
	c := *r
	c.cwdRel = cwdRel
	c.snaps = map[string]string{}
	return &c
}

// cleanCheckout materializes rev with exact bytes in a clean checkout: a shared
// clone (objects borrowed via alternates, nothing written to the real repo) and
// a detached checkout. Not `git archive`: export-ignore / export-subst would
// change the bytes, and the evidence would describe different code.
//
// The checkout lives in a stable, locked slot per repo
// (<data dir>/checkouts/<repo>/<n>) instead of a fresh random dir: build
// caches keyed by source path (Go without -trimpath, most compilers) stay warm
// instead of recompiling everything and growing without bound (lab: a 77 GB Go
// cache). Between runs the slot is reset completely, ignored files included,
// so nothing from an earlier commit (node_modules, dist/, generated code) can
// leak into this one (ADR 0002). Slots unused for a week are swept.
// cleanup releases the slot and is always safe to call.
func cleanCheckout(r *repo, rev string) (*repo, func(), error) {
	noop := func() {}
	sha, err := gitOut(r.root, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return nil, noop, fmt.Errorf("unknown commit %q", rev)
	}
	// Slots live in cinema's per-user data dir (0700), never in a shared TMPDIR:
	// a predictable /tmp path could be pre-created by another user (a symlink, or
	// a .git with hooks that `git checkout` would run as us).
	root, err := checkoutsRoot()
	if err != nil {
		return nil, noop, err
	}
	h := sha256.Sum256([]byte(r.id))
	base := filepath.Join(root, hex.EncodeToString(h[:6]))
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, noop, err
	}
	lock, slot, err := lockSlot(base)
	if err != nil {
		return nil, noop, err
	}
	release := func() { syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); lock.Close() }
	dir := filepath.Join(base, strconv.Itoa(slot))
	if err := prepareSlot(r.root, dir, sha); err != nil {
		os.RemoveAll(dir) // a broken slot is rebuilt next time
		release()
		if _, serr := os.Lstat(dir); serr == nil {
			// Usually root-owned files from a container that ran as root (docker -v $PWD:...).
			err = fmt.Errorf("%w\nclean checkout %s has files this user can't remove (root-owned output of a container task?): "+
				"remove it with `sudo rm -rf %s`, and run the container with --user \"$(id -u):$(id -g)\"", err, dir, dir)
		}
		return nil, noop, err
	}
	cr, err := openRepoAt(dir)
	if err != nil {
		release()
		return nil, noop, err
	}
	cleanup := func() {
		if cr.st != nil {
			cr.st.Close()
		}
		release()
	}
	cr.id, cr.capture = r.id, "clean"
	cr.cache, cr.cachePath = nil, "" // checkout files change every run: nothing to cache
	fmt.Fprintf(os.Stderr, "cinema run: clean checkout of %s\n", sha[:12])
	return cr, cleanup, nil
}

// checkoutsRoot is <data dir>/checkouts, created 0700 and verified to be a
// real directory owned by us.
// artifactsRoot is <data dir>/artifacts (0700): rebuilt artifacts shared
// between tasks, keyed by commit. Swept after a day unused (sweepRunDirs).
func artifactsRoot() (string, error) {
	dataDir, err := store.DataDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(dataDir, "artifacts")
	return root, os.MkdirAll(root, 0o700)
}

func checkoutsRoot() (string, error) {
	dataDir, err := store.DataDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(dataDir, "checkouts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must be a directory owned by you with mode 0700", root)
	}
	return root, nil
}

// lockSlot takes the first free slot under base (one per concurrent run or -j worker).
func lockSlot(base string) (*os.File, int, error) {
	for i := range 64 {
		f, err := os.OpenFile(filepath.Join(base, strconv.Itoa(i)+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, 0, err
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return f, i, nil
		}
		f.Close()
	}
	return nil, 0, errors.New("all 64 clean-checkout slots are busy")
}

// prepareSlot leaves dir as a shared clone of root at sha with nothing else in
// it: tracked files reset, untracked and ignored files removed.
func prepareSlot(root, dir, sha string) error {
	want := filepath.Join(root, ".git", "objects")
	alt, err := os.ReadFile(filepath.Join(dir, ".git", "objects", "info", "alternates"))
	if err != nil || strings.TrimSpace(string(alt)) != want {
		os.RemoveAll(dir)
		if _, err := gitOut(root, "clone", "-q", "--shared", "--no-checkout", "--", root, dir); err != nil {
			return err
		}
	}
	if _, err := gitOut(dir, "checkout", "-q", "-f", "--detach", sha); err != nil {
		return err
	}
	if _, err := gitOut(dir, "clean", "-ffdxq"); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(dir, now, now)
}

// sweepRunDirs removes clean-checkout slots unused for a week (and not locked),
// and the per-run dirs older versions left behind (plan §13.1, CLEAN-002).
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
	if aroot, err := artifactsRoot(); err == nil {
		entries, _ := os.ReadDir(aroot)
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			age := time.Since(info.ModTime())
			partial := strings.HasSuffix(e.Name(), ".tmp") || strings.HasSuffix(e.Name(), ".lock")
			if age > 24*time.Hour || (partial && age > time.Hour) {
				os.RemoveAll(filepath.Join(aroot, e.Name()))
			}
		}
	}
	croot, err := checkoutsRoot()
	if err != nil {
		return
	}
	repos, _ := os.ReadDir(croot)
	for _, rd := range repos {
		base := filepath.Join(croot, rd.Name())
		slots, _ := os.ReadDir(base)
		for _, sl := range slots {
			if !sl.IsDir() {
				continue
			}
			info, err := sl.Info()
			if err != nil || time.Since(info.ModTime()) < 7*24*time.Hour {
				continue
			}
			f, err := os.OpenFile(filepath.Join(base, sl.Name()+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
			if err != nil {
				continue
			}
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				os.RemoveAll(filepath.Join(base, sl.Name()))
				os.Remove(f.Name())
			}
			f.Close()
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

// cpuSet is the CPUs worker slot runs a task capped at n on, out of total:
// consecutive blocks per slot, wrapping when -j × n exceeds the machine.
// nil when no cap applies.
func cpuSet(n, slot, total int) []int {
	if n <= 0 || n >= total {
		return nil
	}
	cpus := make([]int, n)
	for i := range cpus {
		cpus[i] = (slot*n + i) % total
	}
	return cpus
}
