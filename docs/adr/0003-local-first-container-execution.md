# ADR 0003: Local-first container execution on any host

- Status: proposed, 2026-10-30 (accept before §11.2 changes ship)
- Touches: plan.md §0, §5.2, §5.5, §6.1, §7, §8, §9, §11.2, §11.5 (false-green boundary)

## Context

Evidence for Linux jobs came from a Linux box the developer controls (a VPS), with tasks run as plain processes on it. That works (lab: eslint#4 skipped 25% of Linux minutes), but:

- most developers don't have a spare Linux machine, and the goal is CI done **before** the push, on the machine they already use: macOS, Windows or Linux;
- a plain process sees the host: its tools, its home directory, its core count and memory. The lab found each of these changing results (findings on CPU counts, `$HOME` writes, missing runner tools);
- scaling past one machine had no answer except "buy a bigger VPS".

## Decision

1. **Linux jobs run in Linux containers** shaped like the job's runner: pinned runner-like image, runner-sized CPU and memory, non-root `runner` user with sudo, runner paths, fresh `HOME`, UTC (plan §9.2). The checkout happens inside the container from the commit's objects, and its digest must equal the host's (`CHECKOUT_MISMATCH` otherwise).
2. **Engines are pluggable:** Docker-compatible CLIs and Apple `container`. The user picks one; receipts name it.
3. **Exact architecture match.** Receipts record the arch the code ran as and whether it was emulated. The gate accepts a receipt only for jobs whose runner has that arch, unless the task says `arch: any` (an explicit claim, like `platform: any`). `emulation: forbid` rejects emulated runs.
4. **The environment is what CI would use today.** The gate compares image digests, runtimes, service images, arch and runner size from the base-branch contract (all written there by `cinema setup`, none defaulted inside the binary). Where the workflow floats (`stable`, `lts/*`, image tags, `ubuntu-latest`), the gate re-resolves at verify time; a mismatch or an unreachable source means the job runs. Images must be on a trusted list.
5. **Only clean container runs count.** Receipts carry `executor.kind`; host-process runs drive local status only.
6. **The event is part of the contract.** Receipts carry the event they were drafted for, and the gate requires a match.
7. **Failures are evidence too.** Failed runs are signed and published; a failure on the same action key from any trusted key blocks every pass. Retrying until green doesn't work.
8. **Runner size and duration count.** CPUs, memory and `/dev/shm` are in the environment digest, derived from the label and repo visibility. A receipt whose duration, scaled by the measured local-to-CI ratio, is over half the job's timeout is rejected.
9. **Merge-result runs.** `cinema push` also runs tasks on the tree GitHub will test for the PR (`git merge-tree`). Those receipts go on the PR head's note; push-to-main runs read the notes of the PR a commit came from.
10. **Remote hosts are optional and hold no keys.** SSH hosts (and later, on-demand provider plugins) only provide an engine. The developer's machine sends a bundle, cross-checks the source digest, records the run and signs it. Receipts name the host kind, never a hostname.
11. **Shadow mode is the default.** A covered job runs in CI regardless of receipts until the base branch's `tasks.yaml` graduates its current `job_digest`, by backtesting on past merged commits or by agreeing on live PRs. Disagreement removes the graduation; spot checks keep sampling.

## Consequences

- No hardware beyond the developer's machine. A VPS still works, as an ssh host.
- Emulated amd64 on Apple silicon is slower. P4-1 measures by how much; `arch: any` is the honest escape for CPU-independent checks.
- Emulation and container limits can change behaviour (CPU features under Rosetta, `/dev/shm`). Receipts record emulation, and shadow mode catches what slips through.
- Jobs needing Docker inside them get the host engine's socket, opt-in per task (`docker: host`); not available with Apple `container`.
- The runner image is slim: tools come from what each job is found to need, not GitHub's ~60 GB image.
- New reason codes: `ARCH_MISMATCH`, `EMULATED`, `IMAGE_UNTRUSTED`, `SHADOW`, `CHECKOUT_MISMATCH`, `EVENT_MISMATCH`, `TOO_CLOSE_TO_TIMEOUT`, `FAILED_ELSEWHERE`, `HOST_PROCESS_EVIDENCE`.
- Existing skip-in-place setups stop skipping until their jobs graduate. Backtest graduation keeps that to one command.
- Publishing failed runs reveals that a commit failed somewhere; notes never carry logs.
- Plain-process runs stay for `cinema exec` and fast local feedback. They never make CI evidence for a container task.
