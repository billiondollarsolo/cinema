// Command cinema records the checks you already run (plan.md).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/billiondollarsolo/cinema/internal/record"
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

	res, err := record.Run(ctx, opts)
	if err != nil && res.Start.IsZero() {
		fmt.Fprintf(os.Stderr, "cinema exec: %v\n", err)
		return 127 // shell convention for "command not found / not executable"
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema exec: supervision error, run not recorded: %v\n", err)
		return res.ExitCode
	}
	if res.Leftovers {
		fmt.Fprintln(os.Stderr, "cinema exec: stopped processes the command left running")
	}
	if *recordJSON != "" {
		if err := writeJSON(*recordJSON, res); err != nil {
			// Recording failure never changes the child's exit code (plan §4.1).
			fmt.Fprintf(os.Stderr, "cinema exec: run not recorded: %v\n", err)
		}
	}
	return res.ExitCode
}

func writeJSON(path string, res record.Result) error {
	b, err := json.MarshalIndent(struct {
		SchemaVersion int `json:"schema_version"`
		record.Result
		DurationMS int64 `json:"duration_ms"`
	}{1, res, res.Duration.Milliseconds()}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
