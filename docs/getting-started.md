# Getting started

From a repo with GitHub Actions CI to a PR whose Linux jobs skip themselves.

## What you need

- Go 1.27+ to install CInema (release binaries are planned).
- Today: a **Linux x86-64 machine** to produce evidence (your desktop, a VPS). Running in containers on macOS and Windows is **planned** (P4); see [Machines and engines](hosts.md).
- `gh`, logged in, if you want minute and price estimates (`--minutes`).
- Admin access to the repo, to add one repository variable.

## 1. Install

```sh
go install github.com/billiondollarsolo/cinema/cmd/cinema@latest
```

## 2. See what CInema can take over

```sh
cd your-repo
cinema analyze --minutes
```

Every job and matrix leg gets a verdict:

| Verdict | Meaning |
|---|---|
| `ready` | Drafted, and this machine can run it now |
| `needs <tool>` | Drafted; install the tool to run it here |
| `review` | Drafted with open issues (the WHY column says which). `setup` leaves these to CI unless you pass `--include-review` |
| `stays in CI` | Not something to run locally: another OS, secrets, publishing, a reusable workflow from another repo… |

`--minutes` prices each group from your recent PR runs (median billed minutes).

## 3. Set it up

```sh
cinema setup --minutes
```

`setup` shows what changes and asks before writing:

- `.cinema/tasks.yaml`: one task per covered job or matrix leg, each bound to the job it stands in for;
- your workflows: a small `cinema` job per workflow, and on each covered job `needs: cinema` plus an `if:` that skips it when its tasks were reused. See [CI integration](ci-integration.md) for the exact edits.

It formats what it writes with your repo's own prettier if it's installed. `cinema setup --undo` takes the workflow edits back out.

## 4. Check the tasks, then trust them

Read `.cinema/tasks.yaml`. Then:

```sh
cinema trust
```

`trust` approves this exact file's toolchain commands (`go version`, `node --version`). Any edit needs `trust` again.

## 5. Make evidence

```sh
cinema run --commit HEAD --all -j 4
cinema status
```

`--commit HEAD` runs each task in a **clean checkout** of the commit: exactly the committed bytes, nothing ignored, nothing left over from earlier runs. Only these runs count as evidence. `status` shows each task as reusable, stale or missing, with a reason code. If a task fails, `cinema log --failed <task>` shows its output and `cinema doctor` lists tools it needed that this machine lacks.

## 6. Register this machine's key

```sh
cinema keys init
cinema keys export
```

Add the printed line to the repository variable `CINEMA_TRUSTED_KEYS` (Settings → Secrets and variables → Actions → Variables). One line per machine. A variable, not a file in the repo: a PR can edit files, not variables.

## 7. Push with evidence

Commit the changes from `setup`, then:

```sh
cinema push
```

It signs receipts for the pushed commits and pushes the branch and the receipts (git notes) together.

**The PR that adds CInema runs everything as before.** CI reads tasks from the base branch, so jobs start skipping once that PR is merged. From then on, every PR pushed with `cinema push` skips the jobs whose evidence matches.

## 8. Going back

- `cinema setup --undo` removes the workflow edits.
- Delete `.cinema/`, the `CINEMA_TRUSTED_KEYS` variable, and optionally the notes refs (`git push origin --delete refs/notes/cinema/v1/<key-id>`).
- CInema's local data lives in `~/.local/share/cinema` (macOS: `~/Library/Application Support/cinema`).
