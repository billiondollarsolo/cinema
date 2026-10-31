# Commands

`cinema <command> -h` prints a command's flags. Reporting commands take `--json`: exactly one JSON envelope on stdout, progress on stderr, stable task ids and reason codes, for coding agents.

## From existing CI

| Command | What it does |
|---|---|
| `cinema analyze [--yaml \| --json] [--minutes]` | Reads `.github/workflows` and proposes a task per Linux job and matrix leg, with a verdict and reason for each, and what this machine can take over. `--yaml` prints a tasks.yaml draft; `--minutes` prices jobs from recent PR runs (needs `gh`). Writes nothing. |
| `cinema setup [--include-review] [--minutes] [--yes]` | The guided path: capability report, then the tasks and workflow edits that make covered jobs skip themselves. Shows the change and asks first. `--include-review` also takes jobs with open issues. |
| `cinema setup --undo [--yes]` | Removes setup's workflow edits; `tasks.yaml` stays. |
| `cinema init` | Writes a `.cinema/tasks.yaml` skeleton from `go.mod` / `package.json`, for writing tasks by hand. |

## Running

| Command | What it does |
|---|---|
| `cinema run <task>... \| --all [--commit REV] [-j N] [--failfast] [--config FILE] [--force]` | Runs tasks. With `--commit`, each in its own clean checkout of `REV`: the only runs that count as CI evidence. `--all` runs every task this machine can satisfy that isn't already reusable (`--force` reruns those too). `-j` runs several at once. `--failfast` starts no task after one fails; tasks already running finish and are recorded (a pass is still evidence), and the rest print as not started. A failed task's line lists its failed tests. `--config` uses a draft instead of `.cinema/tasks.yaml`. |
| `cinema exec [--timeout D] [--record-json PATH] -- CMD [ARGS...]` | Runs and records any command in your working tree. Feeds local status; matches a task only on exact argv and cwd. Returns the command's own exit code. |
| `cinema trust [--config FILE]` | Approves the toolchain commands of this exact tasks.yaml. |
| `cinema doctor [--config FILE]` | Tools and libraries that failed runs needed and this machine lacks. |
| `cinema debug [--commit REV] [--shell] <task>` | Runs a task in its runner container on a clean checkout, then opens a shell there with the tree as the task left it. Never recorded. |
| `cinema graduate [--commits N] [--min N] [--yes] <task>...` | Compares tasks with CI before their jobs may skip: backtests them on the default branch's recent commits, and adds live pairs (this machine's clean runs on recent PR trees against CI's result for the same job, read through the API). Writes `graduated:` for the ones that agree every time, once there are `--min` results (default 3). A pair where both failed counts only if the same tests failed, CI's read from the job's log: different failed tests, or a failure with no parseable tests on either side, isn't counted (and says why). Not a terminal: needs `--yes`. |
| `cinema hook install \| uninstall` | A pre-push hook that warns (never blocks) when a plain `git push` goes out without evidence. |

## Seeing what's what

| Command | What it does |
|---|---|
| `cinema status [--json]` | Per task: reusable / stale / missing for HEAD, with a reason code. With `--json`, the last run's test counts and failed tests (`last_run.tests`). |
| `cinema explain <task> [--json]` | Why a task is or isn't reusable: what changed, and the run's test counts and failed tests. |
| `cinema log [--failed] <task \| run-id>` | A recorded run's output, after its test counts and failed tests. `--failed`: the task's latest failed run. |
| `cinema dashboard [--port N] [--no-open] [--allow-host H]` | A local web page with tasks, runs and logs. Binds 127.0.0.1 only, with a per-launch token. To reach it on another machine: `ssh -L`, or `tailscale serve` plus `--allow-host`. |

## Evidence

| Command | What it does |
|---|---|
| `cinema keys init \| export` | Creates this machine's signing key; prints its public line for `CINEMA_TRUSTED_KEYS`. |
| `cinema push [--remote R] [--run [-j N]] [--merge-base REF] [--commit REV]... [--notes-only] [--dry-run]` | Signs receipts for the commits being pushed (failed runs too, so a retry elsewhere can't hide them) and pushes the branch and the notes together. `--run` first runs whatever lacks clean evidence for HEAD and for HEAD merged into the default branch (`--merge-base`; `none` to skip). `--notes-only` pushes evidence without the branch. `--dry-run` shows what would be signed. |

## In CI

| Command | What it does |
|---|---|
| `cinema verify --event FILE --skip-outputs WORKFLOW` | What setup's `cinema` job runs: verifies receipts for the tasks covering that workflow's jobs, writes `skip_<job>` / `exclude_<job>` step outputs, and a job summary on the run page: each covered job, skipped or run, and why. |
| `cinema verify --event FILE [--execute [--task ID]] [--report FILE]` | The older gate form: reuse what's proven, run the rest in this job (`--execute`), write `gate-report.json`. |

## Test results

Every run's output is parsed for per-test results: `go test` (plain and `-json`, subtests, named by package), pytest (`-v`, pytest-xdist, the short summary), jest and vitest (only with their `Tests:` summary line), cargo test, and, when the output names no test, JUnit XML files the run wrote anywhere in the tree (`junit*.xml`, `TEST-*.xml`, `surefire-reports/`, `failsafe-reports/`, `test-results/`; only files newer than the run's start). The counts and failed tests stay with the run (90 days); the full list expires with the logs. They are advisory: a parse never makes or breaks evidence. A list that may be missing failures says why (the output was cut, a go package failed to build, the tool's own counts name more failures than the output does).

## Exit codes

| Code | Meaning |
|---:|---|
| 0 | OK, requirement satisfied |
| 1 | Unsatisfied, or a task failed |
| 2 | Bad arguments or config |
| 4 | Transport unavailable (notes fetch/push) |
| 8 | Internal or recording failure |

`cinema exec` returns the command's own exit code.

## Planned (P4)

`cinema run --on HOST`, `cinema push --background / --now / --on`, `cinema hosts [--cleanup]`, `cinema gc`. See [plan.md §10.1](../plan.md).
