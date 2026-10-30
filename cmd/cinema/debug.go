package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// cmdDebug runs a task in its runner-like container on a clean checkout of a
// commit, then opens a shell in that container with the tree as the task left
// it (plan §9.2). Nothing is recorded: a debug session is never evidence.
func cmdDebug(args []string) int {
	fs := flag.NewFlagSet("debug", flag.ContinueOnError)
	rev := fs.String("commit", "HEAD", "debug on a clean checkout of `REV`")
	noRun := fs.Bool("shell", false, "open the shell without running the task first")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cinema debug [--commit REV] [--shell] <task>")
		return exitBadArgs
	}
	r, err := openRepo()
	if err != nil || r.cfg == nil {
		fmt.Fprintf(os.Stderr, "cinema debug: %v%v\n", err, r.cfgErr)
		return exitBadArgs
	}
	defer r.close()
	t := r.cfg.Tasks[fs.Arg(0)]
	if t == nil {
		fmt.Fprintf(os.Stderr, "cinema debug: no task %q\n", fs.Arg(0))
		return exitBadArgs
	}
	pl, ok := place(t)
	if !ok || !pl.sandbox {
		fmt.Fprintf(os.Stderr, "cinema debug: %s doesn't run in a runner container here (%s)\n", t.ID, placeHint(t))
		return exitBadArgs
	}
	cr, cleanup, err := cleanCheckout(r, *rev)
	defer cleanup()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema debug: %v\n", err)
		return exitInternal
	}
	bins, err := t.RuntimeBins(r.dataDir, "linux", strings.TrimPrefix(pl.platform, "linux/"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema debug: runtime: %v\n", err)
		return exitInternal
	}
	cpus, mem := t.RunnerSize()
	art, _ := artifactsRoot()
	sb, err := task.StartSandbox(context.Background(), task.SandboxSpec{Image: loadUserConfig().Image, Platform: pl.platform, Emulated: pl.emulated,
		Root: cr.root, RepoName: filepath.Base(cr.id), RepoID: cr.id, DataDir: r.dataDir, Bins: bins, Artifacts: art,
		Objects: filepath.Join(cr.id, ".git", "objects"), CPUs: cpus, MemoryMB: mem * 1024})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema debug: %v\n", err)
		return exitInternal
	}
	defer sb.Stop()
	attach := func(argv []string) error {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	if !*noRun {
		fmt.Fprintf(os.Stderr, "▶ %s (debug: not recorded)\n", t.ID)
		if err := attach(sb.Exec(t.Cwd, t.Argv, nil)); err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", t.ID, err)
		} else {
			fmt.Fprintf(os.Stderr, "✓ %s passed\n", t.ID)
		}
	}
	fmt.Fprintf(os.Stderr, "shell in %s's container (%s, %s); exit to remove it\n", t.ID, sb.Platform, sb.Workdir)
	fi, _ := os.Stdin.Stat()
	_ = attach(sb.Shell(t.Cwd, fi != nil && fi.Mode()&os.ModeCharDevice != 0))
	return 0
}
