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
| `cinema run <task>... \| --all [--commit REV] [-j N] [--config FILE] [--force]` | Runs tasks. With `--commit`, each in its own clean checkout of `REV`: the only runs that count as CI evidence. `--all` runs every task this machine can satisfy that isn't already reusable (`--force` reruns those too). `-j` runs several at once. `--config` uses a draft instead of `.cinema/tasks.yaml`. |
| `cinema exec [--timeout D] [--record-json PATH] -- CMD [ARGS...]` | Runs and records any command in your working tree. Feeds local status; matches a task only on exact argv and cwd. Returns the command's own exit code. |
| `cinema trust [--config FILE]` | Approves the toolchain commands of this exact tasks.yaml. |
| `cinema doctor [--config FILE]` | Tools and libraries that failed runs needed and this machine lacks. |

## Seeing what's what

| Command | What it does |
|---|---|
| `cinema status [--json]` | Per task: reusable / stale / missing for HEAD, with a reason code. |
| `cinema explain <task> [--json]` | Why a task is or isn't reusable: what changed. |
| `cinema log [--failed] <task \| run-id>` | A recorded run's output. |
| `cinema dashboard [--port N] [--no-open] [--allow-host H]` | A local web page with tasks, runs and logs. Binds 127.0.0.1 only, with a per-launch token. To reach it on another machine: `ssh -L`, or `tailscale serve` plus `--allow-host`. |

## Evidence

| Command | What it does |
|---|---|
| `cinema keys init \| export` | Creates this machine's signing key; prints its public line for `CINEMA_TRUSTED_KEYS`. |
| `cinema push [--remote R] [--run [-j N]] [--commit REV]... [--notes-only] [--dry-run]` | Signs receipts for the commits being pushed and pushes the branch and the notes together. `--run` first runs whatever lacks clean evidence for HEAD. `--notes-only` pushes evidence without the branch. `--dry-run` shows what would be signed. |

## In CI

| Command | What it does |
|---|---|
| `cinema verify --event FILE --skip-outputs WORKFLOW` | What setup's `cinema` job runs: verifies receipts for the tasks covering that workflow's jobs and writes `skip_<job>` / `exclude_<job>` step outputs. |
| `cinema verify --event FILE [--execute [--task ID]] [--report FILE]` | The older gate form: reuse what's proven, run the rest in this job (`--execute`), write `gate-report.json`. |

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

`cinema run --on HOST`, `cinema push --background / --now / --on`, `cinema debug <task>`, `cinema graduate`, `cinema shadow`, `cinema hosts [--cleanup]`, `cinema gc`. See [plan.md §10.1](../plan.md).
