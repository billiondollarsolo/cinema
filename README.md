# CInema

**Record the checks you already run. Reuse what qualifies. Run only what remains.**

You and your coding agents already run `go test`, `npm run lint` and the rest before pushing, and then CI runs them again. CInema records those runs on your machine (or a Linux box you control), signs a receipt that binds each result to the exact source, task definition and toolchain, and publishes it with your push. The CI gate verifies the receipts and reuses them. Anything without a valid receipt runs in CI as before.

The rule that never bends: **unknown means run, never approve.** A macOS result never stands in for Linux. A run on a dirty tree, a run that changed files, a run replayed from a tool cache, a run of a different task definition, or a run on a different toolchain is never evidence.

Status: pre-release. P0–P2 of the [build plan](plan.md) are implemented and tested on six real projects (below); GitHub Actions first. Next (P4): running Linux jobs in containers on your own machine, macOS, Windows or Linux, with remote machines as an option ([machines and engines](docs/hosts.md)).

## How it works

```
your machine / VPS                                 GitHub Actions
──────────────────                                 ──────────────
cinema run --commit HEAD -j 4    clean checkout    cinema verify (the gate)
  → records each task's run      of the commit       → fetches receipts from git notes
cinema push                                          → checks signature, source, task, toolchain, age
  → signs passing runs (Ed25519, in-toto/DSSE)       → REUSE what qualifies
  → pushes branch + receipts (refs/notes/cinema/…)   → RUN the rest (one matrix job per task)
```

- **Tasks** live in `.cinema/tasks.yaml`: an exact argv, a platform, optional pinned runtimes (`runtime: {go: 1.26.10, node: 22.23.4}`, provisioned identically on both sides), toolchain probes, declared outputs and a reuse window. See [plan §6](plan.md).
- **Evidence comes from clean checkouts** of a commit ([ADR 0002](docs/adr/0002-ci-evidence-from-clean-checkouts.md)), never from your working tree with its ignored, stale files.
- **Trust is explicit:** `cinema trust` approves the exact tasks.yaml that may run toolchain probes, and the gate trusts only the keys in `CINEMA_TRUSTED_KEYS` (GitHub: a repo variable; GitLab: `.cinema/trusted-keys` on the default branch, reviewed like code, see [GitLab](docs/gitlab.md)).
- **No junk:** every file CInema writes has a cleanup rule ([plan §13.1](plan.md)).

## Quick start

```sh
go install github.com/billiondollarsolo/cinema/cmd/cinema@latest

cinema setup                     # from existing CI: what this machine can take over; covered jobs then skip themselves when reused (asks first)
cinema trust                     # after reviewing tasks.yaml
cinema run --commit HEAD --all   # clean-checkout runs, recorded
cinema status                    # reusable / stale / missing, with reason codes
cinema keys init && cinema keys export   # register the public key as CINEMA_TRUSTED_KEYS
cinema push                      # sign + push branch and receipts
```

`cinema setup` edits your workflows in place: each gets a small `cinema` job that verifies receipts, and each covered job gets `needs: cinema` plus an `if:` that skips it when its tasks were reused. Your checks stay the same; reused ones show as skipped, and a matrix drops just its reused legs. If the `cinema` job fails, nothing is skipped. Rerun `cinema setup` any time (it starts from your original workflows); `cinema setup --undo` takes cinema's edits back out. (The lab's separate parallel gate is in [`spikes/lab/workflows`](spikes/lab/workflows).)

### From existing CI

`cinema analyze` reads `.github/workflows` and proposes a task per Linux job and matrix combination, with exact runtimes. It says why it can't propose the rest: other OSes, secrets, service containers, artifacts passed between jobs, or actions it can't translate. It also says what *this machine* can take over: each job is `ready`, `needs docker` / `needs shellcheck` (and what installing it would add), `review`, or `stays in CI`; `--minutes` prices each group from the repo's recent PR runs. It never writes tasks.yaml. Check a draft before relying on it:

```sh
cinema analyze --yaml > draft.yaml
cinema trust --config draft.yaml
cinema run --config draft.yaml --commit HEAD --all
```

## Results so far

Six open-source projects mirrored and tested against their own upstream CI, on the same PRs ([details, findings and caveats](spikes/lab/RESULTS.md)). Billed minutes are counted the way GitHub bills them: each job rounded up to a minute, priced by OS, with the gate's own cost subtracted.

| PR | Gate | Billed minutes saved | If the repo were private |
|---|---|---|---|
| traefik | 13/13 reused, 36 s | 90% | 86% of the bill |
| prometheus | 17/17 reused, 54 s | 46% | 43% |
| prettier | 7/7 reused, 33 s | 21% | 6% (macOS jobs dominate the bill) |
| eslint | 6/6 reused, 32 s | 20% | 15% |

No false greens: upstream passed every Linux check wherever the gate passed. The ceiling is what a Linux evidence machine can't or shouldn't claim: Windows and macOS legs, fuzzing, and tests against floating third-party versions. The compute moves to a machine you pay for; it doesn't vanish.

## Commands

```
setup · init · exec · run · status · explain · log · doctor · trust · dashboard · keys · push · verify · analyze
```

`cinema <command> -h` for flags. `--json` on reporting commands, for agents ([plan §10.3](plan.md)).

## Docs

- **[docs/](docs/README.md): getting started, how it works, CI integration, tasks reference, machines and engines, gotchas, security, commands**
- [plan.md](plan.md): the build plan, the invariant, reason codes, cleanup contract
- [vision.md](vision.md): the long-range design
- [docs/adr](docs/adr): decisions on the false-green boundary
- [spikes/lab/RESULTS.md](spikes/lab/RESULTS.md): the lab, findings 1–56

## License

[Apache-2.0](LICENSE)
