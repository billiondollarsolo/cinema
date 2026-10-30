# How it works

## The one rule

**Unknown means run, never approve.** A job is skipped in CI only when a signed receipt proves that this exact work passed: the same source bytes, the same task definition, the same environment. Anything missing, malformed, stale, untrusted or ambiguous means the job runs, exactly as it would without CInema.

## The pieces

```text
your machine                                     GitHub Actions
────────────                                     ──────────────
cinema run --commit HEAD --all                   cinema job (one per workflow)
  clean checkout of the commit                     fetch receipts (git notes)
  run each task, record the result                 verify signature, source, task, environment, age
cinema push                                        output skip_<job> / exclude_<job>
  sign receipts (Ed25519, in-toto/DSSE)          covered jobs
  push branch + refs/notes/cinema/v1/<key>         skip themselves when reused, run otherwise
```

### Tasks

A task (in `.cinema/tasks.yaml`) is the work a CI job does, as an exact command: argv, working directory, platform, pinned runtimes, environment inputs. `cinema setup` drafts one per Linux job and matrix leg from your workflows and binds it to that job (`covers:` and `job_digest:`). See the [tasks reference](tasks.md).

### Runs and evidence

`cinema run --commit REV` runs a task in a clean checkout of `REV` and records the result: exit code, timing, output, and three fingerprints:

- **source**: every tracked and untracked-but-not-ignored file, by content, mode and path. `.cinema/tasks.yaml` itself is excluded ([ADR 0001](adr/0001-exclude-tasks-yaml-from-source.md)).
- **task**: the task's contract (argv, platform, runtimes, toolchain, env names, and for `npm run x` the script body).
- **environment**: the platform, user (root or not), pinned runtime versions, toolchain probe output, container and service image digests, and allowlisted env values.

Only runs from clean checkouts count as CI evidence ([ADR 0002](adr/0002-ci-evidence-from-clean-checkouts.md)). A run on your working tree feeds `cinema status`, never CI.

### Receipts

`cinema push` turns passing clean-checkout runs into receipts: a statement of "task T passed on source S in environment E", signed with this machine's key, stored as a git note on the commit. One notes ref per key, so machines never overwrite each other. Notes never contain logs.

### The CI side

The `cinema` job that `setup` adds runs `cinema verify --skip-outputs <workflow>`. For each task covering a job in that workflow it:

1. takes the task list from the **base branch** (a PR can't loosen its own rules);
2. computes the fingerprint of the tree CI checked out, which for a PR is the **merge commit**, not the PR head;
3. looks for a receipt whose signature verifies against `CINEMA_TRUSTED_KEYS`, for this repository, with matching source, task and environment, within `max_age`;
4. checks the job in the workflow is still exactly the one the task was drafted from (`job_digest`): a PR that adds a step to a job makes it run.

It then outputs `skip_<job>=true` when every task of a job was reused, and for a matrix the legs to exclude. Covered jobs read those outputs in their `if:`. If the `cinema` job fails or is cancelled, its outputs are empty and nothing is skipped.

## What "reused" doesn't mean

- **It doesn't prove the work happened.** A receipt is a claim by a trusted key. CInema's trust model is personal: you trust the machines you register ([Security](security.md)).
- **It doesn't carry across changes.** A merge from main that touches any file changes the source fingerprint; the jobs run. That's whole-source fingerprinting, deliberately: narrower inputs would skip more and risk false greens.
- **It doesn't cover other platforms.** A macOS result never stands in for a Linux job.

## Built since P4

- **Runs in containers on your own machine** (macOS, Windows, Linux), shaped like the job's runner. See [Machines and engines](hosts.md).
- **Shadow mode.** A covered job keeps running in CI until its task is graduated by backtesting on past merged commits (`cinema graduate`).
- **Merge-result runs.** `cinema push --run` also tests the tree GitHub will build for the PR, so receipts match even after main has moved.
- **Stricter matching:** floating versions re-resolved at check time; the event in the contract; failed runs published, so a failure from any trusted machine blocks a pass on the same work, and a pass after a failure counts only after three in a row; the runner's CPUs, memory and OS release in the environment; a run that used over half the job's timeout isn't reused.

Still planned: live shadow pairs and spot-check comparison, remote hosts, Apple `container`, more language runtimes ([plan §16](../plan.md)).
