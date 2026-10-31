package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/billiondollarsolo/cinema/internal/testreport"
)

// cmdLog prints a recorded run's captured output: the latest run of a task, or
// a run by ID. The dashboard shows the same; this is for terminals and agents.
func cmdLog(args []string) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	failed := fs.Bool("failed", false, "with a task: the latest failed run, not the latest run")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: cinema log [--failed] <task | run-id>")
		return exitBadArgs
	}
	r, err := openRepo()
	defer r.close()
	if err != nil || r.stErr != nil {
		fmt.Fprintf(os.Stderr, "cinema log: %v\n", firstErr(err, r.stErr))
		return exitBadArgs
	}
	id := fs.Arg(0)
	if !strings.HasPrefix(id, "run_") {
		runs, err := r.st.TaskRuns(r.id, id, 200)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cinema log: %v\n", err)
			return exitInternal
		}
		id = ""
		for _, run := range runs {
			if !*failed || run.State != "passed" {
				id = run.ID
				break
			}
		}
		if id == "" {
			fmt.Fprintf(os.Stderr, "cinema log: no matching run of %q\n", fs.Arg(0))
			return exitBadArgs
		}
	}
	run, stdout, stderr, err := r.st.Run(r.id, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema log: no run %s in this repo\n", id)
		return exitBadArgs
	}
	fmt.Fprintf(os.Stderr, "%s  %s  %s, exit %d, %s ago\n", run.ID, strings.Join(run.Argv, " "), run.State,
		run.ExitCode, time.Since(run.Started).Round(time.Second))
	if s := run.Tests; s != nil { // first: in a long log it's what you're looking for
		fmt.Fprintf(os.Stderr, "tests: %d passed, %d failed, %d skipped%s\n", s.Passed, s.Failed, s.Skipped,
			failedTests(s, "\n", len(s.Failures)))
	}
	os.Stdout.Write(stdout)
	if len(stderr) > 0 {
		fmt.Fprintln(os.Stderr, "--- stderr")
		os.Stderr.Write(stderr)
	}
	if run.StdoutTruncated || run.StderrTruncated {
		fmt.Fprintln(os.Stderr, "(output was over the capture limit: the middle was dropped)")
	}
	if len(stdout) == 0 && len(stderr) == 0 && run.StdoutTotal+run.StderrTotal > 0 {
		fmt.Fprintln(os.Stderr, "(output expired: logs of passing runs are kept 7 days, failing 30)")
	}
	return 0
}

// failedTests lists s's failures, at most n, each line starting with sep
// ("" when none). Parsed from the output: advisory, never evidence.
func failedTests(s *testreport.Summary, sep string, n int) string {
	if s == nil || s.Failed == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sfailed tests:", sep)
	for i, f := range s.Failures {
		if i == n {
			fmt.Fprintf(&b, "%s  … %d more (cinema log)", sep, s.Failed-n)
			break
		}
		d := ""
		if f.DurationMS > 0 {
			d = fmt.Sprintf(" (%s)", time.Duration(f.DurationMS)*time.Millisecond)
		}
		fmt.Fprintf(&b, "%s  %s%s", sep, f.Name, d)
	}
	if s.Partial != "" {
		fmt.Fprintf(&b, "%s  (maybe not all: %s)", sep, s.Partial)
	}
	return b.String()
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
