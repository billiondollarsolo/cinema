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

### traefik: in progress

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
13. **Security review findings while building P2,** all fixed:
    - Toolchain commands are an allowlist (`[tool, --version]`).
    - The gate fails closed when the base config can't be confirmed.
    - The dashboard URL token is single-use.
    - Notes are size-checked before they're read.

## Local runs (`run-lab.sh`, VPS, root pass)

| Project | Tasks run | Result | Notes |
|---|---|---|---|
| cli | 7 | 6 pass; `test` fails as root only | root finding (2) |
| golangci-lint | 3 | all pass, 109 s; re-run finds nothing to do | |
| traefik | 8 | lint fails (Go drift); webui-build deletes a tracked file | findings 3, 4 |
| vite | 6 | all pass, 92 s; config reformatted by `format-check` | finding 7 |
