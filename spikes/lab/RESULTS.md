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
| [vite#1](https://github.com/mjtechguy/cinema-lab-vite/pull/1) | **PASS, 6/6 reused, 13 s** | 9 jobs, 25.2 runner-min, 31 billed, 4.9 min to green; **failed on Windows only**, every Ubuntu job passed | Lint job (81 s) fully; the Node 24 Ubuntu Build&Test partially (unit tests, not its Playwright e2e suites): **~2–6 runner-min (10–25%)** | Node 20/22/26 cells (the evidence machine has only Node 24, and Node is fingerprinted to the patch), macOS, Windows, e2e (Playwright) |

**No false greens:** upstream passed every Linux check on every commit where the gate passed (vite's only failure was Windows, which the gate never claims). Local evidence for vite takes 48–50 s with `-j 4`.

**What it says about savings**
- **Runner minutes:** the gate replaces whatever Linux work its tasks cover, at a fixed cost of about 25–50 s and 1 billed minute. That's 24–75% here. It goes up as more jobs become tasks: Docker integration tests and cross-builds are the next obvious ones.
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

## The parallel gate with no evidence (fork-like): first runs

| Project | Parallel gate wall | Task jobs | Upstream median PR time to green |
|---|---:|---:|---:|
| prettier | 5.2 min | 8 | 7.2 min |
| eslint | 3.7 min | 6 | 12.6 min |
| traefik ([#2](https://github.com/mjtechguy/cinema-lab-traefik/pull/2)) | **72 min** (12 of 13 jobs done in ≤ 12 min; `build-cross` 71 min) | 13 | 9.1 min (same PR, 115 runner-min) |

With no receipts, the gate is no longer a regression *when each task is about one upstream job*: each matrix job runs one task. traefik's `build-cross` packs 18 upstream matrix legs into one task, and so it ran 71 min serially on a 4-vCPU runner versus 4.6 min per upstream leg (finding 31). It is now 6 tasks of 3 targets, about 12 min each, the same as the longest integration group. (Three task jobs failed on these first runs because of task-definition and formatting issues, findings 25–26. They were fixed before the evidence runs.)

## Local runs (`run-lab.sh`, VPS, root pass)

| Project | Tasks run | Result | Notes |
|---|---|---|---|
| cli | 7 | 6 pass; `test` fails as root only | root finding (2) |
| golangci-lint | 3 | all pass, 109 s; re-run finds nothing to do | |
| traefik | 8 | lint fails (Go drift); webui-build deletes a tracked file | findings 3, 4 |
| vite | 6 | all pass, 92 s; config reformatted by `format-check` | finding 7 |
