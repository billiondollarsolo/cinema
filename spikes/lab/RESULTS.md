# CInema lab: real projects, real CI

Started 2026-10-21. The question: on big open-source projects with heavy CI, how much CI time does CInema save, and what breaks when it meets real repos?

**Setup**
- **Evidence machine:** Hetzner VPS, 16 cores / 30 GB RAM, Ubuntu 24.04, linux/amd64 (the same platform as `ubuntu-latest`), user `alphabravo` (non-root).
- **Mirrors:** public `mjtechguy/cinema-lab-<project>`. Each holds upstream's exact tracked tree as one orphan commit (license kept, `LAB.md` attribution) plus `.cinema/tasks.yaml` and a `cinema` gate workflow. Upstream's own workflows stay in place as the baseline.
- **Task definitions:** extracted by reading each project's workflows (`tasks/*.yaml`, with comments on what each maps to).

Scripts in this directory:
- `ci-baseline.sh`: upstream PR CI cost from the Actions API.
- `run-lab.sh`: local run, determinism and tree-writes check.
- `make-mirror.sh`: builds a mirror.
- `workflows/`: the gate workflows.

## Upstream PR CI, per push (medians, `ci-baseline.sh`, last 60 PR runs)

| Project | Jobs | Runner-min | Billed min* | Time to green |
|---|---:|---:|---:|---:|
| containerd | 50 | 547 | 576 | 44 min |
| prometheus | 27 | 149 | 167 | 23 min |
| traefik | 40 | 128 | 146 | 10 min |
| hugo | 3 | 118 | 119 | 81 min |
| vitest | 24 | 98 | 110 | 13 min |
| gitea | 15 | 95 | 104 | 22 min |
| prettier | 27 | 84 | 99 | 7 min |
| eslint | 35 | 70 | 88 | 13 min |
| golangci-lint | 17 | 50 | 61 | 14 min |
| caddy | 19 | 35 | 44 | 6 min |
| cli/cli | 8 | 31 | 35 | 10 min |
| vite | 10 | 27 | 33 | 7 min |

\* Σ per-job minutes rounded up. Public repos pay nothing; this is what the same CI would bill privately.

## End-to-end results

### cli/cli: gate PASS with 6 of 7 tasks reused (`mjtechguy/cinema-lab-cli`)

Push to trunk at `4fbe56b`. Evidence: `cinema push --run` on the VPS, all tasks in a clean checkout, 135 s.

| Workflow on the same commit | Wall time |
|---|---:|
| upstream Unit and Integration Tests (ubuntu + macOS + Windows) | 590 s |
| upstream Lint | 158 s |
| upstream Code Scanning (CodeQL) | 152 s |
| **cinema gate**: build, go-fix, go-mod-tidy, licenses-check, lint and test REUSED; govulncheck (never reusable) ran inline for 26 s | **50 s** |

Upstream CI also passed on this commit, so the gate's PASS was correct. What the gate can't replace: the macOS and Windows legs (different platforms, by design), and CodeQL (results are uploaded to GitHub, so there's nothing to reuse).

### prometheus (`mjtechguy/cinema-lab-prometheus`)

Push to main at `2aec0e5`. Evidence: `cinema push --run`, 16 tasks in a clean checkout, 1995 s on the VPS. That time is inflated: a parallel root-user lab pass was competing for the 16 cores.

| Task | VPS (clean checkout) | Result |
|---|---:|---|
| test-race (`go test -race ./...`) | 8m34s | pass |
| forcedirectio-race | 8m10s | pass |
| test-386 | 4m23s | pass |
| dedupelabels | 2m31s | pass |
| slicelabels-race | 2m07s | pass |
| compliance | 1m55s | pass |
| tsdb-no-isolation | 1m42s | pass |
| ui (assets, lint, tests) | 1m39s | pass |
| style, license, go-mod-tidy, generated-parser, examples-syntax | under 10 s each | pass |
| golangci-lint ×3 (v2.14.0) | 25–50 s | **fail on the VPS, pass in upstream CI** (see finding 14) |

**Gate result at `3764398`: PASS, 16 of 16 tasks REUSED, 42 s for the whole gate job.** Upstream PR CI for this work takes about 27 jobs, 149 runner-minutes and 23 minutes to green.

How we got there (each step is a finding):
1. `2aec0e5`: 13 REUSED, 3 lint RUN inline, gate FAIL. Lint failed both on the VPS and on the runner (finding 14).
2. `787b733`: the task file changed to the official lint binary. Only tasks.yaml changed, so the 13 existing runs stayed valid (ADR 0001) and only lint re-ran, 90 s. It still failed: polluted tool cache (finding 15).
3. `3764398`: the lint cache is isolated per binary. Lint passes, 16 receipts, 180 s.

### traefik (`mjtechguy/cinema-lab-traefik`)

13 tasks: build (linux/amd64 and the 18 cross targets), lint, validate-generate, validate-vendor, webui build and unit tests, unit tests, and the integration suites as 5 `exclusive` groups (core-a, core-b, docker, k8s, kv). Clean-checkout evidence with `-j 3` takes 23 min on the VPS. The integration groups take 1–9 min each and run alone (finding 28).

## PR experiments: real code change, merge-commit gate, same PR as upstream

Each PR appends a Go comment to the project's main entry point, so upstream's path filters run their full PR CI. Evidence: `cinema push --run` on the VPS (clean checkout, non-root). CI checks out GitHub's synthetic merge commit. `main` hadn't moved, so its tree equals the PR head's, and receipts made for the head match.

| PR | Gate | Upstream PR CI (same PR) | Covered by the gate | Not covered (why) |
|---|---|---|---|---|
| [prometheus#1](https://github.com/mjtechguy/cinema-lab-prometheus/pull/1) | **PASS, 16/16 reused, 24 s** | 24 jobs, 128.7 runner-min, 144 billed, 16.6 min to green; passed | Go tests, More Go tests, lint, 32-bit, UI, compliance, parser: **37 runner-min (29%)** | Windows tests (16.6 min, the critical path; different platform), fuzzing ×4 (33 min; never reusable), cross-build (21.5 min; Docker, needs image fingerprinting), previous-Go, upgrade, mixins |
| [prometheus#2](https://github.com/mjtechguy/cinema-lab-prometheus/pull/2) | **PASS, 17/17 reused, 54 s** (incl. the Docker crossbuild) | 24 jobs, 136.2 runner-min, 16.5 min to green; **failed on Windows only** (`TestDelayedCompaction`, tsdb timing), every Linux job passed | Go tests, More Go tests, cross-build (3 legs, 25 min), lint, 32-bit, UI, compliance, parser: **~67 runner-min (49%)** | Windows (16.4 min, the critical path), fuzzing ×4 (33 min), upgrade/downgrade (8.8), previous Go, mixins, CodeQL, npm publish |
| [traefik#1](https://github.com/mjtechguy/cinema-lab-traefik/pull/1) | **PASS, 8/8 reused, 23 s** | 39 jobs, 119.4 runner-min, 138 billed, 8.7 min to green; passed | Build Binaries (19 targets), Validate, Test Unit: **~90 runner-min (75%)** | Test Integration (30 runner-min, 8.7 min, the critical path; Docker, added in #3) |
| [traefik#3](https://github.com/mjtechguy/cinema-lab-traefik/pull/3) | **PASS, 13/13 reused, 36 s, 0.4 runner-min** | 39 jobs, 127.1 runner-min, 9.2 min to green; passed | Build Binaries, Validate, Test Unit, **Test Integration**: **~127 runner-min (~100%)**, including the critical path | nothing on Linux |
| [cli#3](https://github.com/mjtechguy/cinema-lab-cli/pull/3) | **PASS, 6/7 reused, 51 s** (govulncheck inline) | 10 jobs, 32 runner-min, 38 billed, 11 min to green; passed | Ubuntu build/test, lint: **7.7 runner-min (24%)** | macOS + Windows legs (19 min), CodeQL (upload-only), integration tests (need a GitHub token) |
| [prettier#1](https://github.com/mjtechguy/cinema-lab-prettier/pull/1) | **PASS, 7/7 reused, 33 s** | 40 jobs, ~95 runner-min; Lint **failed at Dependency Review only** (the mirror has no dependency graph; every lint step our task covers passed, and upstream skipped the steps after it, which our task ran); a macOS job was cancelled at 15 min | Lint, prod lint, Node 22/24/26 prod tests, Node 22 and Node 26-full dev tests, build: **~21 runner-min (22%)** | macOS/Windows (~42), Node 14/16/18/20 cells (~12; lockfile rewrite or two-runtime pipeline, finding 25), browser tests (~7), sizes, CLI |
| [eslint#1](https://github.com/mjtechguy/cinema-lab-eslint/pull/1) | **PASS, 6/6 reused, 32 s** | 35 jobs, ~64 runner-min (CI workflow 31, 3.8 min to green); passed | Verify Files, Test Types, mocha on Node 20/22/24/26: **~14 runner-min (46% of the CI workflow, 23% overall)** | ecosystem plugin tests (22; floating plugin versions), types integration (9), macOS/Windows, Node 20.19.0 and `--experimental-transform-types` legs, browser test, package managers, CodeQL |
| [vite#1](https://github.com/mjtechguy/cinema-lab-vite/pull/1) | **PASS, 6/6 reused, 13 s** | 9 jobs, 25.2 runner-min, 31 billed, 4.9 min to green; **failed on Windows only**, every Ubuntu job passed | Lint job (81 s) fully; the Node 24 Ubuntu Build&Test partially (unit tests, not its Playwright e2e suites): **~2–6 runner-min (10–25%)** | Node 20/22/26 cells (the evidence machine has only Node 24, and Node is fingerprinted to the patch), macOS, Windows, e2e (Playwright) |

**No false greens:** upstream passed every Linux check on every commit where the gate passed (vite's only failure was Windows, which the gate never claims). Local evidence for vite takes 48–50 s with `-j 4`.

**How savings are counted** (`ci-bill.py`)

The first numbers in this file summed raw job time and counted Linux, Windows and macOS minutes the same. That overstated savings. GitHub [rounds each job up to a whole minute and prices by runner OS](https://docs.github.com/en/billing/reference/actions-runner-pricing): Linux $0.006, Windows $0.010, macOS $0.062 per minute on standard runners. The gate's own jobs cost at least 1 billed minute each. Recomputed on the same PR runs:

| PR | Upstream billed min ($ if private) | Covered by reused tasks | Gate | **Net billed minutes saved** | **Net $ saved per push (private repo)** |
|---|---|---|---|---|---|
| traefik#3 | 144 ($0.91; incl. 12 Windows min) | 132 ($0.79) | 2 ($0.01) | **130 (90%)** | **$0.78 (86%)** |
| prometheus#2 | 152 ($0.98; incl. 17 Windows) | 72 ($0.43) | 2 | **70 (46%)** | **$0.42 (43%)** |
| prettier#1 | 109 ($2.51; 31 macOS min = $1.92) | 25 ($0.15) | 2 | **23 (21%)** | **$0.14 (6%)** |
| eslint#1 | 85 ($0.69) | 19 ($0.11) | 2 | **17 (20%)** | **$0.10 (15%)** |

Caveats:
- **The mirrors are public, so GitHub bills them $0.** The timing API reports 0 billable ms. The $ column is what the same CI would cost in a private repo.
- **Runner size:** public repos get 4-vCPU Linux runners and private ones 2-vCPU, so private jobs run longer. Absolute minutes are understated; percentages roughly hold.
- **Minutes, not dollars, for macOS-heavy projects:** CInema replaces Linux jobs, and macOS is about 10× the price. That's why prettier saves 21% of minutes but only 6% of dollars.
- **The compute moves.** Evidence runs on a machine you pay for (traefik: 23 min on the 16-core VPS). What's saved is hosted minutes, plus time-to-green when the critical path is covered.
- **Hypothetical:** it assumes the covered upstream jobs are switched off and the gate replaces them. The mirrors run both.
- **With misses,** each RUN matrix job adds at least a minute plus setup.

**What it says about savings**
- **Runner minutes:** the gate replaces whatever Linux work its tasks cover, at a fixed cost of about 25–50 s and 1 billed minute. Measured as billed minutes net of the gate, that's 20–90% across the four recomputed PRs (6–86% of what a private repo would pay). traefik (#3) is all of its Linux CI, including the Docker integration suites: 90% of minutes once its Windows jobs are counted. prometheus went from 29% to 49% once its Docker crossbuild became a task. The ceiling is set by what a Linux evidence machine can't or shouldn't claim: Windows/macOS legs, fuzzing, and jobs that test floating third-party versions (eslint's ecosystem plugins).
- **Time to green improves only when the critical path is covered.** In these projects the slowest job is usually something a Linux box can't or shouldn't replace: Windows tests, fuzzing, Docker integration. The gate is never slower than upstream when evidence exists; without evidence the parallel gate is needed (finding 16).
- **Local cost:** serial clean-checkout runs took 2.3 min (cli), 10.7 min (traefik) and 28 min (prometheus) on 16 cores. With `-j 4` and stable checkout slots, prometheus's 16 tasks take **11.8 min cold and 11.2 min warm (2.4× faster)**. That's now bounded by the two longest tasks (race suites, about 8.8 min each), which are dominated by running tests that `-count=1` never caches. More workers wouldn't help.

## Findings and what changed in CInema

Each finding came from a real project and was fixed with a test.

1. **False green from ignored files (critical) → ADR 0002.**
   - What happened: the cli mirror lost force-tracked `internal/gh/` to `.gitignore`. The VPS working tree still had it, ignored and so not fingerprinted.
   - Result: build and test passed locally, and the gate REUSEd them for a commit that doesn't compile in CI.
   - Change: CI evidence now comes from clean checkouts only. `push` signs only clean-checkout runs, the gate rejects others with `WORKSPACE_EVIDENCE`, and `push --run` makes clean evidence for HEAD.
2. **Root vs non-root changes results.** cli's permission tests fail as root, and tests that skip as root would *pass*. GitHub runners are non-root. The environment fingerprint now includes it.
3. **Toolchain drift is the main source of misses.**
   - traefik CI uses Go 1.26.x; the VPS had 1.27. golangci-lint built for 1.26 can't even read 1.27 export data.
   - git and other probed tool versions differ between a dev box and the runner image.
   - Lesson: probe only what matters and isn't pinned in source. Pin the gate's toolchain to the evidence machine's exact versions.
4. **One task's tree writes poisoned the next.** traefik's `yarn build` (vite `emptyOutDir`) deletes a tracked file, so every later task saw a different tree.
   - Fix: a clean checkout is restored between tasks.
   - Fix: tasks can declare `outputs:` for files they write.
5. **Unignored outputs.** caddy writes `cover-profile.out` into the repo root. `outputs:` keeps such files out of that task's fingerprint and mutation check.
6. **`go test` cache replays hide behind wrappers** (`env … go test`, `bash -c`, `make`), where the config-time `-count=1` check can't see them. Now `(cached)` is detected at runtime on the full stdout stream, and the run is rejected with `TOOL_CACHE_REJECTED`.
7. **A repo formatter rewrote CInema's own config.** vite's `pnpm format` reformatted `.cinema/tasks.yaml`. Trust dropped (correct), but the run wasn't flagged, because the config is outside the fingerprint (ADR 0001). Now it is.
8. **Pushing from a second clone with the same key would wipe published receipts** (squash + force-push from an empty local ref). `push` now fetches its own ref from the push URL first.
9. **Floating dependencies (eslint has no lockfile).** Declaring `node_modules/.package-lock.json` (npm's record of the installed tree) as a toolchain file binds evidence to the exact installed versions.
10. **Derived, ignored build outputs.** vite and prettier tests read `dist/`, which is ignored and so not fingerprinted. A stale `dist/` could pass tests for new source. Lab tasks build first. ADR 0002 (clean checkout) removes the stale-`dist/` case entirely.
11. **Network- and time-dependent checks** (govulncheck, cli's live-Sigstore integration tests, `@latest` tools) get `reuse: false` or a short `max_age`.
12. **Never-reusable, parallel jobs belong outside the gate.** prometheus fuzzes in 4 parallel 4-minute jobs. Inside one gate job they'd run serially for 16 minutes and make CI slower.
14. **"Same tool, same version" isn't the same artifact.** golangci-lint v2.14.0 built with `go install`/`go run` reports a revive issue that the official v2.14.0 release binary doesn't, whichever Go builds it. CI uses the release binary (golangci-lint-action). Rule: tasks must invoke the exact artifact CI uses. Lab lint tasks now download the pinned release and verify its SHA-256 (in the argv, so part of the contract).
15. **Tool caches outside the repo are unfingerprinted inputs.** golangci-lint keys its analysis cache on the version string and file hashes. The `go install` build and the official binary both call themselves 2.14.0, so the official binary replayed the other build's cached issue: polluted cache exit 1, cleared cache exit 0. Unlike `go test`, nothing in the output shows it. Rule: isolate tool caches per tool build (lab: `GOLANGCI_LINT_CACHE` per binary). General guidance: a tool cache shared across different builds is an input CInema can't see.
16. **With no evidence, one gate job is a regression.** First push, forks, contributors without CInema: prometheus's serial gate was 9 of 16 tasks in after 34 min; traefik's cross-build alone took 62 min on a 4-vCPU runner (26 min on the 16-core VPS). Upstream runs those in parallel jobs, 10–23 min. Fix: a parallel gate. A plan job verifies receipts, a matrix runs one job per RUN task, and a gate job is the single required check. Fully reused costs plan + gate only.
17. **Local runs are serial.** prometheus took 28 min while lint and small checks left cores idle. `cinema run --commit REV -j N` / `push --run -j N` run tasks in parallel, each worker in its own clean checkout.
18. **Docker-built checks need image fingerprints.** prometheus's 109-runner-minute cross-build runs in `golang-builder:<tag>` images. A floating tag is an input `[tool, --version]` can't capture. A toolchain entry that records an image's resolved digest is needed before it can be a task.
20. **Random clean-checkout paths defeat path-keyed build caches.** Go without `-trimpath`, and most compilers, key on source paths: a fresh `/tmp/cinema-run-<pid>-x` per run recompiled everything and grew the Go cache to 77 GB. Fix: stable, locked slots per repo, fully reset between runs (ignored files too, so ADR 0002 still holds), swept after 7 days. Security follow-up: slots live in the per-user data dir, not a shared TMPDIR.
21. **Killed tools leave temp junk.** `/tmp/go-build*` work dirs piled up after go processes were stopped. Each task in a clean checkout now gets a private `TMPDIR` that's removed afterwards.
22. **Files CInema adds must follow the repo's formatter.** In vite, oxfmt's `format && git diff --exit-code` failed on `.cinema/tasks.yaml` and on the gate workflow until both were formatted with the repo's own formatter. Formatting `tasks.yaml` changes layout, not meaning, so every task contract stayed identical and only `format-check` re-ran (ADR 0001). Formatting the workflow changed the tree, so everything re-ran.
23. **Runtime-version matrices need one task per version.** vite tests Node 20, 22, 24 and 26. Node is fingerprinted to the patch (correctly), so only the Node 24 cell can reuse evidence from a Node-24 machine. Covering the matrix means a task per version and an evidence machine with each version installed.
24. **Toolchain drift and runtime matrices are now handled by `runtime:` pinning.** `runtime: {go: 1.26.10, node: 22.23.4}` makes CInema provision exactly that version for the task and its probes, on the evidence machine and in the gate. Go goes through `GOTOOLCHAIN`; Node uses the official tarball checked against `SHASUMS256.txt`. Environments match by construction. On GitHub runners the plan jobs provisioned Node 20/22/24/26 without any `setup-node`. A runtime-version matrix is one task per version.
25. **One task, one runtime.** prettier's Node 20 cell builds `dist/` with Node 22 and only *tests* on 20. A single-runtime task can't express a two-runtime pipeline, so that cell stays an upstream job.
26. **A repo's linter reads CInema's files.** eslint parsed a YAML comment starting with "eslint" as an inline config directive and failed. prettier's `--check` and knip also flagged the lab files. CInema's files must be lint- and format-clean for the repo they live in.
27. **Parallel local runs can trip timing-sensitive tests.** traefik's `TestForwardAuthClientClosedRequest` (a client-close race) failed under `-j 4` load and passed alone. It's a safe failure (no receipt) but costs a re-run. Such tasks can be marked `exclusive`.
28. **Tasks that share global resources** (traefik's integration suites use a fixed docker subnet and fixed ports) get `exclusive: true`: they run alone under `-j`.
29. **Failed runs need to be readable from a terminal.** On a headless box there was no way to see a failed task's output (no sqlite3, and the dashboard needs a browser). `cinema log [--failed] <task|run-id>` prints it.
30. **No lockfile means no exact reuse.** eslint resolves dependency versions only at install time, after the environment is fingerprinted and in a way the gate can't check without installing. Evidence is bound to source and Node version only, with a short `max_age`. This is a limit, not a bug.

19. **Upstream config files come along into mirrors.** `dependabot.yml` opened PRs on the cli mirror. Mirrors should drop bots that act on the repo.
13. **Security review findings while building P2,** all fixed:
    - Toolchain commands are an allowlist (`[tool, --version]`).
    - The gate fails closed when the base config can't be confirmed.
    - The dashboard URL token is single-use.
    - Notes are size-checked before they're read.
31. **One task per upstream matrix leg, or near it.** The parallel gate's no-evidence wall time is its slowest task. A task that loops over a matrix upstream runs in parallel (traefik's 18 cross targets) turns minutes into an hour. Split such loops into several tasks. With evidence it makes no difference: the gate reused all 13 tasks in 36 s.
32. **Container tasks can leave root-owned files in a checkout slot.** Then `git clean` can't reset the slot, and nothing can delete it without root. CInema now fails with the slot path, the `sudo rm -rf` to remove it, and the fix: run containers with `--user "$(id -u):$(id -g)"`. prometheus's crossbuild already does, and its outputs are user-owned.
33. **CPU-saturating tasks make neighbours flaky.** prometheus's crossbuild (6 platforms, `--parallelism 6`, 13 min) ran under `-j 4` beside the race suites. test-race (`TestQueryConcurrency`), test-386 (tsdb) and forcedirectio-race (10 min test timeout) failed, all timing. That's a safe failure (no receipts) but costs a re-run. Heavy tasks get `exclusive: true`. It's a scheduling hint, not part of the contract, so existing receipts stay valid.
34. **Stopping CInema was recorded as the check failing.** prettier's jest runs hung (see below). Killing the `push --run` marked all 4 in-flight tasks `failed` on those exact inputs, which blocks reuse, and push then pushed and exited 0. Now a run ended by the user's SIGINT/SIGTERM is `interrupted`: never signable, ignored by reuse decisions. An interrupted push pushes nothing. Timeouts stay failures (a hang is a real bug).
35. **Tasks with the same argv were mis-attributed: a false-green path.** eslint's mocha-20/22/24/26 share one argv and differ only in `runtime:`. `cinema run` found "the task" by argv, and Go's random map order picked one: a Node 20 run was filed under mocha-26 with mocha-26's Node 26 fingerprint. Had it passed, it would have been evidence for the wrong runtime. None was published (those runs failed, or were overwritten). `cinema run` now records the task it runs; a bare `exec` of a shared argv is history only.
36. **The gate workflow must pass the repo's workflow linters.** prometheus runs zizmor on every PR. It flagged the lab gate workflow's `npm i -g corepack@latest` (an unpinned install outside a lockfile, `adhoc-packages`). prometheus doesn't even need corepack (no `packageManager`), so the step was dropped from its template. A shipped workflow must install nothing unpinned. The longer-term fix is for CInema to provision the package manager the way it provisions Node.
37. **A machine bigger than the runner can break a project's tests.** prettier's jest (`jest-light-runner/child-process`) deadlocks with its default worker count on the 16-core lab box (15 workers): no CPU, nothing running, forever. That's true without CInema too. With `--maxWorkers=3`, as on a 4-vCPU runner, the suite passes in 84 s. Evidence machines should match the runner's parallelism where a tool scales with core count. A deadlock is a safe failure, but it costs hours without a task timeout. (Candidate feature: a per-task `timeout:`.)
38. **Parallel tasks share caches outside the repo.** eslint's four mocha tasks ran `npm install` at the same time. Each triggers cypress's postinstall download into the shared `~/.cache/Cypress`, and two failed with "could not be downloaded". Same class as finding 15: a shared, unfingerprinted cache. Safe (no receipts), but such tasks run `-j 1` or `exclusive`.
39. **Ignored files leaked between tasks in one run: a false-green path.** Slots were fully reset between runs but not between tasks, so one task's `node_modules` and build outputs were visible to the next (eslint `pnpm-test`). Each task now starts from a fully clean checkout (see the analyze section).
40. **Global installs must not touch the shared runtime.** Parallel `npm i -g pnpm` broke the provisioned Node for both tasks. Global npm/pnpm installs now go to the run's scratch dir, in evidence and gate runs.
41. **Drafts must never publish.** prometheus's UI release job was drafted and ran a `pnpm publish` dry run. OIDC-token jobs are now skipped as publish/deploy.
42. **Size tasks like the runner they replace.** `cpus: N` pins a task to N CPUs (Linux CPU affinity, inherited by every child), and parallel workers get separate CPU blocks. `nproc`, Go, Node's `availableParallelism()` and Python's `process_cpu_count()` all see N. Node's `os.cpus()` doesn't, and prettier's jest still started 16 workers, but on 4 CPUs it no longer deadlocked: the jobs went from 1 h timeouts to 3 min passes. Scheduling only, like `exclusive`: not part of the contract. Drafts get `cpus: 4`.
43. **Workflow-level `defaults:` apply to every job.** The analyzer read only job-level `defaults.run.working-directory`, so prettier's release-script job was drafted to run the wrong suite from the repo root. Had it passed, the receipt would have certified a different check than the one CI runs.
44. **The gate takes the task list from the base branch, so a new tasks.yaml only counts after it merges.** The first raw-draft PR (prometheus #3) put the draft on the PR branch, and the gate correctly required main's old tasks. A PR can't redefine what it must pass. For adopters: the PR that adds cinema runs everything in CI; reuse starts with the next PR.
45. **A pinned Go printed "go: downloading" into the probe on fresh runners.** The plan job's first `go version` under `GOTOOLCHAIN` fetched the toolchain, and that stderr line became part of the probe output, so the environment digest never matched the VPS's (which already had that Go). Every Go-pinned task would have missed on GitHub. The pinned Go is now fetched before any probe runs. Local verification can't catch this class: only the real gate on a fresh runner did.
46. **Raw drafts of container jobs had no environment fingerprint.** prometheus runs its tests in `golang-builder`; the draft ran the steps on the bare machine with no Go pin, so its receipts would match any Linux box. Drafts now pin Go/Node from the repo's version file when a job uses them without a setup action, and container jobs get `container:` (finding 50).
47. **Raw drafts must pass the repo's formatter.** eslint's `prettier --check` failed on the draft twice: once for `{a: b}` flow maps (fixed: block maps), then because eslint formats YAML with 4-space indents, which no generator can guess. `cinema setup` runs the repo's own prettier on the files it writes. Reformatting `tasks.yaml` kept all 19 existing receipts valid (ADR 0001; finding 22 confirmed on a raw draft): only the failed task re-ran, 2 min instead of 80.
48. **Review drafts can be hollow.** prometheus's `golangci` draft kept the job's setup steps but not the lint (an action the analyzer can't translate), and it passed. Reusing it would have claimed a lint that never ran. Savings are counted only for drafts that reproduce their job, and `cinema setup` writes only `ready` jobs unless asked.
49. **Off-the-shelf Actions emulation wasn't faithful (spike).** `run-local-ci` 0.18.1 (agent-ci) feeds jobs to the official runner, but it never started prometheus's job `container:` (no "Initialize containers" step), so `promci-setup` hit `go: command not found` in its minimal runner image, and its checkout copies the working tree (files showed as modified). GitHub's `ubuntu-latest` VM image isn't available as a container, so any emulator runs on a smaller machine. Decision: native tasks plus first-class `container:`, with the capability report naming what the machine lacks.
50. **`container:` and composite inlining close most of the container gap.** `container: IMAGE` runs a task in the image as root (like a container job), hands files back, and fingerprints the resolved digest, which is exactly what runs. The analyzer emits it for container jobs and inlines commit-pinned composite actions (prometheus's `promci-setup`). prometheus: 8 task drafts instead of 2.
51. **The separate gate doesn't cut a bill by itself; skip-in-place does.** Every saving above assumes someone deletes the jobs the gate covers. `cinema setup` now edits the workflows instead: a `cinema` job verifies receipts and each covered job gets `needs: cinema` and an `if:` that skips it when its tasks were reused (0 minutes; it shows as skipped in the usual checks). If the `cinema` job fails, nothing is skipped. On [cli#4](https://github.com/mjtechguy/cinema-lab-cli/pull/4) the `cinema` job took 10 s, `govulncheck` was skipped on its receipt, and every other job ran as before. The edit is textual: the rest of the file stays byte-for-byte. Not covered yet: matrix jobs with any leg the machine can't run (GitHub doesn't allow `matrix` in a job's `if:`), and jobs other jobs need (skipping one skips its dependents).
52. **Common actions can be translated faithfully.** `golangci-lint-action` → the official release binary of the exact version, SHA-256 checked, private cache; `pnpm/action-setup` → that pnpm in the run's scratch dir. cli's lint (`version-file`), traefik's lint (`${{ env.X }}`) and eslint's pnpm-test (`latest`, pinned with a note) became tasks; cli's passed (golangci-lint 2.15.0, 0 issues). Hollow review drafts still fail where they should: prometheus's `govulncheck` and `zizmor` install their tools through actions, and failed in the gate with `command not found`.
53. **Analysis must survive cinema's own edits.** After setup, a project's workflows run `cinema verify`, and the analyzer skipped any such workflow as "the gate". Now only cinema's job is skipped, and a skip-in-place `if:` is read back as the job's original condition.
54. **Matrix jobs drop their reused legs.** Matrices with legs a Linux box can't run (macOS, Windows) are common, and a job's `if:` can't see `matrix`. `setup` adds `exclude: ${{ fromJSON(needs.cinema.outputs.exclude_<job> || '[]') }}` to the matrix; the `cinema` job outputs the reused legs. On [cli#5](https://github.com/mjtechguy/cinema-lab-cli/pull/5), `build (ubuntu-latest)` never started while the Windows and macOS legs ran. Two bugs on the way: verify first said "SKIP build" because build's only task (the ubuntu leg) was reused, so nothing was excluded (safe, no saving); and, from a security review, a stale or partial `leg:` would have excluded legs without evidence. verify now expands the workflow as checked out: an exclude entry must be exactly one real leg, and a matrix job skips whole only when every real leg was reused.
56. **Artifacts from another job are rebuilt, not a dead end.** "Needs artifacts from another job" was the commonest reason a job stayed in CI: 77 of the 154 review/skip jobs across the six mirrors. A job that downloads an artifact a job it `needs` uploads (same workflow, or a local reusable workflow with one job) now becomes a task that builds the artifact itself. The producer's steps run in a fresh copy of the commit, as on its own runner, without the consumer's env, and only the uploaded path is copied back, placed as `download-artifact` would. Whether the path is a file or a directory is checked when the task runs; a mismatch fails closed. Two setup steps that want different Node or Go versions in one task are now flagged instead of the last one silently winning. **traefik:** all 19 cross-build legs of `build.yaml` (which need the web UI) went from "stays in CI" to ready. All 19 pass locally: 4m42s at `-j 4`, about 55 s each, each rebuilding the web UI. Upstream bills those legs **22 to 88 min per PR** (cache hit vs miss); `setup` skips the whole job, and only the 1-minute web UI build still runs, because the job needs it. **prettier** stays in review, correctly: its build job's `setup-node` has no version, so CI builds with whatever Node the runner has. Follow-ups: the artifact is now built once per commit and shared by the tasks that need it (keyed by producer job, commit and Go/Node versions; swept after a day). traefik's 19 legs: **4m43s → 2m10s**. The first 4 build concurrently; the other 15 take about 14 s each. And traefik's `safe-chain setup-ci` had been writing `~/.safe-chain` on the evidence machine: clean runs now get a private `HOME`, as on a fresh runner, with download and build caches kept in the real home. A regression pass on cli, vite, prometheus and traefik: everything still passes.
55. **First real savings: eslint, skip-in-place.** On [eslint#4](https://github.com/mjtechguy/cinema-lab-eslint/pull/4) the jobs really didn't run: `Test Types`, `Test pnpm Type Support` and `Are the types wrong?` were skipped, and the `Test` matrix's five plain Ubuntu legs never started while macOS and Windows ran. Billed: **64 minutes against 81** on the same mirror with nothing skipped (eslint#3): 17 minutes, 21%. On Linux alone, 72 → 54 (25%, $0.43 → $0.32). The dollar total came out flat in this one sample because the untouched macOS leg took 7 minutes instead of 5 (+$0.12 at ten times the Linux rate). What still ran: the `--experimental-transform-types` leg (added by `include:`, which GitHub applies after `exclude`, so it can't be excluded), `Verify Files` (no receipt: see below), and the review drafts `setup` leaves to CI (ecosystem plugin tests). eslint's YAML lint also caught a draft bug: an empty matrix value was written as an empty mapping value; drafts now write `""`.

## The parallel gate with no evidence (fork-like): first runs

| Project | Parallel gate wall | Task jobs | Upstream median PR time to green |
|---|---:|---:|---:|
| prettier | 5.2 min | 8 | 7.2 min |
| eslint | 3.7 min | 6 | 12.6 min |
| traefik ([#2](https://github.com/mjtechguy/cinema-lab-traefik/pull/2)) | **72 min** (12 of 13 jobs done in ≤ 12 min; `build-cross` 71 min) | 13 | 9.1 min (same PR, 115 runner-min) |
| traefik ([#4](https://github.com/mjtechguy/cinema-lab-traefik/pull/4), `build-cross` split into 6) | **14.3 min** (critical path integration-docker 13.6) | 18 | 17.2 min (same PR; both competed for the 20-concurrent-job limit) |

With no receipts, the gate is no longer a regression *when each task is about one upstream job*: each matrix job runs one task. traefik's `build-cross` packs 18 upstream matrix legs into one task, and so it ran 71 min serially on a 4-vCPU runner versus 4.6 min per upstream leg (finding 31). It is now 6 tasks of 3 targets, about 12 min each, the same as the longest integration group. (Three task jobs failed on these first runs because of task-definition and formatting issues, findings 25–26. They were fixed before the evidence runs.)

## `cinema analyze` against the mirrors

Deterministic drafts from each project's `.github/workflows`, one per Linux job and matrix combination (current analyzer):

| Project | task | review | skip |
|---|---:|---:|---:|
| eslint | 11 | 10 | 12 |
| traefik | 6 | 4 | 35 |
| prettier | 5 | 5 | 25 |
| cli | 2 | 2 | 12 |
| vite | 0 | 12 | 4 |
| prometheus | 2 | 13 | 26 |

- **Before version handling:** 4 jobs in total were "task".
- **Version files** (`.go-version`, `go.mod`, `.nvmrc`; ~74 jobs) and **ranges** resolved to today's release (`1.26` → 1.26.10, `22` → 22.23.4) made most of the difference.
- **`if:` evaluation** covers `matrix.*`, `runner.*`, `github.event_name` (always `pull_request`) and the status functions, with short-circuiting, so `failure() && …` steps drop out.
- **Skipped as not checks:** other OSes, secrets, jobs that check out another repository, aggregator jobs that report `needs.*.result`, OIDC (`id-token: write`) publish jobs, steps reading `github.event.*`, and artifacts passed between jobs.
- **Artifact uploads** become `outputs:`, so a job that writes what it uploads isn't flagged as changing the tree.

### Verification: every draft, run unmodified

Every draft with a proposal (task *and* review verdicts, except those using `sudo`) was run as-is with `cinema run --commit HEAD -j 2` in clean checkouts on the VPS, on each mirror's default branch. Nothing in the projects was edited or tuned: a failure is either a CInema bug (fixed, then everything re-run) or recorded below as a limit. After the first pass, the VPS got the GitHub runner image's standard tools that `cinema doctor` named (`shellcheck`, `xvfb`, mikefarah `yq`, Playwright's Chromium and Firefox system libraries), and the affected drafts were re-run.

| Project | Passed as drafted | of which verdict task | of which verdict review |
|---|---:|---:|---:|
| eslint | 20/20 | 11/11 | 9/9 |
| prometheus | 8/9 | 2/2 | 6/7 |
| cli | 3/4 | 2/2 | 1/2 |
| traefik | 5/9 | 4/6 | 1/3 |
| vite | 1/5 | – | 1/5 |
| prettier | 8/8 | 5/5 | 3/3 |
| **total** | **45/55** | **24/26** | **21/29** |

**Failures, by cause:**

- **Machine prerequisites (7 at first, 0 now):** traefik validate (`shellcheck`), vite preview (`yq`), vite's four test jobs (Chromium: `libnspr4.so`), eslint test-on-browser (`Xvfb`, then Firefox's `libgtk-3`). All are on the GitHub runner image. A failed run now names them (`not on this machine: shellcheck`), and `cinema doctor` lists them per task. Installed on the VPS, validate and test-on-browser passed. Trap: Ubuntu's `yq` package is a different tool (kislyuk's jq wrapper) from the runner's mikefarah `yq`; install the release binary.
- **Flaky on this box (4):** vite's test jobs now run, and fail only in `playground/hmr*` (HMR updates time out under full parallel load). The same suite fails or passes from run to run outside CInema too, with identical settings. Same class as finding 33.
- **Not a check (1, now skipped):** vite's `preview` job ran `pkg-pr-new publish` ("Continuous Releases are only available in GitHub Actions"). Steps that publish (`npm/pnpm/yarn publish`, `pkg-pr-new publish`, `goreleaser release`, `docker push`, …, but not `--dry-run`) now make the analyzer skip the job.
- **Too big a machine (5 at first, 0 now):** prettier's jest jobs deadlocked on 16 cores and hit the 1 h `timeout:` (finding 37). Drafts now carry `cpus: 4` (GitHub's standard Linux runner), and all four passed in about 3 minutes each, with no project change (finding 42). The fifth, `test-release-script-test`, was an analyzer bug: it ignored the workflow-level `defaults.run.working-directory`, so the draft ran prettier's whole suite from the root on Node 20 instead of the release script's 8 tests (finding 43). Fixed; it passes.
- **Writes files during the run (1 now; 3 before the fix below):** traefik's gateway conformance rewrites a tracked report; prettier `test` leaves c8 `coverage/tmp/*.json` files; prettier `lint` downloads `./actionlint` into the repo root. The last two only added untracked files, which GitHub would ignore too. CInema now ignores files a run only adds in a clean checkout (they don't change what was tested); a modified or deleted tracked file still blocks evidence. Re-run with that change, both passed as evidence.
- **Needs the CI environment (3):** cli integration-tests (`GH_TOKEN`), traefik knative (`ko-build/setup-ko` exports `KO_DOCKER_REPO`), traefik docs (`setup-python`; Debian's system pip refuses installs). All three were verdict review with that reason.
- **Containers / parallel collisions (2):** prometheus test-go-more runs in the project's builder image (verdict review, "runs in a container"); traefik's merged conformance collides with the other conformance cluster under `-j` and passed alone (needs `exclusive`).

**CInema bugs this exposed, all fixed:**
- **Ignored files leaked between tasks in one run: a false-green path.** Slots were reset with `git clean -ffdxq` between runs but only `-fdq` between tasks, so `node_modules` and build outputs from one task were visible to the next. eslint's `pnpm-test` refused to run ("Delete node_modules…"); a task that depended on another's build could have passed here and failed on GitHub. Each task now starts from a fully clean checkout. The earlier verification pass was discarded and re-run.
- **Global installs went into the shared runtime.** `npm i -g pnpm` in two parallel tasks wrote into the same provisioned Node and broke both (`Cannot find module …/bin/pnpm`), and would have leaked into later runs. Global npm/pnpm installs now go to the run's scratch directory, in evidence and gate runs alike, with both `$PNPM_HOME` and pnpm 11's `$PNPM_HOME/bin` on `PATH`.
- **A publish job was drafted and ran.** prometheus's `publish_ui_release` became a task and ran `pnpm publish` (a dry run that 404'd; nothing was published). Jobs that request an OIDC token are now skipped as publish/deploy, as are status aggregators, steps reading the PR body, and third-party artifact restores.
- **Analyzer translation gaps:** object-valued matrix entries (`matrix.package.directory`), checkouts of other repositories, `failure() && …` steps included unconditionally, and artifact uploads reported as source changes.

**So:** with clean environments fixed, the runner image's standard tools installed and tasks sized like the runner, 82% of drafts (45/55) pass unmodified, and 24 of 26 task-verdict drafts. The two task-verdict failures are traefik's: a `-j` collision (needs `exclusive`) and a job that rewrites a tracked report. Review is earned: those drafts fail where their reason says they would.

**Caveat on the earlier lab numbers:** the hand-written lab task files behind the savings table were tuned in places (e.g. prettier's `--maxWorkers=3`, `exclusive` on heavy tasks, pinned lint binaries). The savings are what CInema does with a reviewed tasks.yaml, not with raw drafts.

## Raw drafts on real PRs (out of the box)

`tasks.yaml` exactly as `cinema analyze --yaml` wrote it, the generic gate (`exp-draft.sh`), evidence on the VPS, then a one-line code change as a PR so upstream runs its full PR CI. Billed minutes as in `ci-bill-draft.py`: **strict** counts upstream jobs covered by reused task-verdict drafts; **faithful** adds reused review drafts that reproduce every step of their job (hollow ones never count). The gate's own jobs, including tasks it had to run, are subtracted.

| PR | Upstream billed min | Gate | Strict net saved | Faithful net saved | Hand-tuned tasks (earlier) |
|---|---:|---:|---:|---:|---:|
| [prometheus#4](https://github.com/mjtechguy/cinema-lab-prometheus/pull/4) | 144 | 26 (2 tasks ran: golangci, test-go-more) | −24 (−17%) | 3 (2%) | 70 (46%) |
| [prometheus#5](https://github.com/mjtechguy/cinema-lab-prometheus/pull/5) (with `container:` + composite inlining) | 155 | 4 (2 hollow review drafts ran and failed) | 32 (21%) | 36 (23%) | 70 (46%) |
| [eslint#3](https://github.com/mjtechguy/cinema-lab-eslint/pull/3) | 81 | 2 (all 20 reused) | 23 (28%) | 52 (64%) | 17 (20%) |

**eslint#3:** the gate reused all 20 draft tasks, upstream CI passed, and the raw draft beat the hand-written tasks: the analyzer drafted all six Node legs, the types checks and the ecosystem tests where the lab file had 6 tasks. The dollar share is lower (18% strict, 40% faithful) because macOS and Windows stay in CI. Two drafts went through review fixes first: the draft had to be formatted with eslint's prettier (finding 47).

**prometheus#5:** with `container:` and composite inlining, the gate reused 10 of 12 tasks, every container test job included (their image digests matched on the runner). The two it ran, `govulncheck` and `zizmor`, are hollow review drafts (their tools come from actions) and failed, which is why `cinema setup` writes ready jobs only. Upstream passed. The remaining gap to the hand-written tasks is prometheus's Docker crossbuild (6 jobs) and golangci-lint, whose version comes from a step output.

**prometheus#4:** the gate reused 9 of 11 tasks. `golangci` missed on finding 45; `test-go-more` had no receipt and failed on the runner because the draft ran outside its container (`buf: command not found`). Upstream CI passed. The raw draft saved nothing because prometheus runs nearly all its CI in `golang-builder`, which the analyzer couldn't express yet (findings 46, 50).

## Local runs (`run-lab.sh`, VPS, root pass)

| Project | Tasks run | Result | Notes |
|---|---|---|---|
| cli | 7 | 6 pass; `test` fails as root only | root finding (2) |
| golangci-lint | 3 | all pass, 109 s; re-run finds nothing to do | |
| traefik | 8 | lint fails (Go drift); webui-build deletes a tracked file | findings 3, 4 |
| vite | 6 | all pass, 92 s; config reformatted by `format-check` | finding 7 |
