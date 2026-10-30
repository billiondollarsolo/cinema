// Command cinema records the checks you already run (plan.md).
package main

import (
	"fmt"
	"os"
)

// Exit codes for everything except exec, which returns the child's code (plan §10.2).
const exitBadArgs = 2

const usage = `usage: cinema <command> [args]

commands:
  setup [--include-review] [--yes]                          from existing CI: what this machine can take over, then the files that do it
  debug [--commit REV] [--shell] <task>                     run a task in its runner container, then open a shell there (not recorded)
  graduate [--commits N] <task>...                          backtest stand-ins on past commits against CI's results; agreeing ones may skip their jobs
  init                                                      write a .cinema/tasks.yaml skeleton
  exec [--record-json PATH] [--timeout D] -- CMD [ARGS...]   record an arbitrary command
  run [--config FILE] [--commit REV] <task>... | --all     run tasks by name (clean checkout with --commit)
  status [--json]                                           per task: reusable / stale / missing + reason
  explain [--json] <task>                                   why a task is or isn't reusable
  log [--failed] <task | run-id>                            a recorded run's captured output
  trust [--config FILE]                                     allow the toolchain commands in .cinema/tasks.yaml
  dashboard [--port N] [--allow-host H] [--no-open]         local web page: checks, runs, logs
  keys init | export                                        this machine's signing key
  push [--remote R] [--notes-only] [--commit REV]...        sign evidence for pushed commits, push branch + notes
  verify --event FILE [--execute] [--report FILE]           the CI gate: reuse verified receipts, run the rest
  doctor [--config FILE]                                    tools and libraries failed runs needed that this machine lacks
  analyze [--yaml | --json]                                 propose tasks from .github/workflows (advisory;
                                                            check a draft: run --config draft.yaml --commit HEAD --all)
`

func main() {
	// ponytail: map + flag.FlagSet per verb; Cobra only if this stops being readable.
	cmds := map[string]func([]string) int{
		"init": cmdInit, "exec": cmdExec, "run": cmdRun, "status": cmdStatus, "explain": cmdExplain, "trust": cmdTrust, "dashboard": cmdDashboard, "keys": cmdKeys, "push": cmdPush, "verify": cmdVerify, "log": cmdLog, "analyze": cmdAnalyze, "doctor": cmdDoctor, "setup": cmdSetup, "graduate": cmdGraduate, "debug": cmdDebug,
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitBadArgs)
	}
	if f, ok := cmds[os.Args[1]]; ok {
		os.Exit(f(os.Args[2:]))
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "cinema: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(exitBadArgs)
	}
}
