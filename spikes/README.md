# P0 spikes (plan.md §16)

Throwaway code. It answers the feasibility questions and then gets deleted; none of it is the product.
GitHub spikes ran on 2026-10-21 in private throwaway repos `mjtechguy/cinema-p0{,-goapi,-webapp}` (since deleted), using the workflows in `workflows/`.
The same code now lives in public test beds: `mjtechguy/cinema-test-gate`, `cinema-test-goapi`, `cinema-test-webapp` (free Actions minutes).

## Findings that change the plan

1. **Private repos cannot fetch notes with `persist-credentials: false`.** The fetch fails with `could not read Username`. Fix: pass the job token for that one fetch only, via
   `git -c http.extraheader="AUTHORIZATION: basic $(printf x-access-token:%s "$TOKEN" | base64 -w0)" fetch origin '+refs/notes/cinema/*:refs/notes/cinema/*'`.
   The token stays out of `.git/config`, so later task steps can't read it. Public repos are unaffected.
2. **Per-job billing rounds up to whole minutes.** A 10 s gate bills 1 minute, the same as a 44 s CI job. On private repos, billed minutes go down only when the reused work spans more than 1 billed job-minute. Wall time improves either way. The pitch should lead with wall time.
3. **Whole-source receipts reuse only when the PR branch already contains the latest base.** If base has moved at all, even in an unrelated file, the merge-commit fingerprint differs from the PR head's (P0-3). Reuse rate therefore depends on how often PRs are up to date at CI time. This is the number P0-5 needs from real repos.

## P0-5 Reuse estimate on made-up repos ✅ (up-to-date rate still unknown)

Two private throwaway apps, each with realistic CI and 2 PRs, measured with `spikes/p0-5-measure.sh`. "Green" means run created → finished; "billed" means the sum of per-job minutes, each rounded up.

| Repo | CI today | Gate, all reused | Gate, realistic |
|---|---|---|---|
| `cinema-p0-goapi`: golangci-lint, `go test -race`, 20 s fuzz, 3-target build matrix | **138–144 s**, 7 billed min | 9 s, 1 min | Linux VPS: ~50 s, 1 min (fuzz has `reuse: false`, so it always runs inline). Mac only: ~115 s, 2 min (race tests are `linux/amd64` and run inline). |
| `cinema-p0-webapp`: eslint, tsc, vitest, vite build as a 4-job matrix | **19 s**, 4 billed min | 9–10 s, 1 min | Same as all reused: every task is `platform: any`, so Mac receipts cover it. |

What this says:
- **The win scales with how slow CI is.** On goapi, time to green drops from about 140 s to about 50 s with a Linux box. On webapp, CI is already fast, so the gain is about 9 s. Small JS repos gain little wall time, and on public repos wall time is the only gain.
- **Billed minutes drop on every private repo** (7→1, 4→1), because each matrix job bills at least a minute.
- **Without a Linux box, most of the Go win is gone** (140 s → 115 s). The plan's §9 VPS flow is central to the product, not an add-on.
- **Unpinned CI tools defeat reuse.** `golangci-lint-action` installed v2.15 in CI while the local copy is v2.11. The environment fingerprint would correctly reject Mac lint receipts as `ENVIRONMENT_MISMATCH`. `cinema init` should nudge users to pin linter versions, otherwise lint reuse is close to 0.
- **Flaky checks:** the fuzz step failed once with `context deadline exceeded` at its time limit. Tasks with `reuse: false` carry their flakiness into the gate. Reusing a locally passed flaky test hides a flake that CI would sometimes catch; vision.md should state that semantics explicitly.
- **Not measured:** how often real PRs are up to date with base at CI time (finding 3). Made-up repos can't produce that number. It needs merged-PR history from real repos.

## P0-1 Gate cost: floor measured ✅

| Workflow (run-start → finish, excludes queue) | PR A | PR B | PR C |
|---|---:|---:|---:|
| `p0-gate`: checkout + download binary + 1 DSSE verify | 10 s | 11 s | 10 s |
| `p0-baseline`: setup-go + vet + `go test` on a toy repo | 46 s | 44 s | 41 s |

Gate steps: checkout under 1 s, binary download 1 s, verify under 1 s. The rest is runner setup and teardown.
The baseline is deliberately tiny, so ~10 s is the floor. Against realistic CI (P0-5), the fully reused gate cuts goapi from ~140 s to 9 s. **Exit criterion met: continue.**

## P0-2 Git notes round trip ✅ (fork case not tested)

- `git push --atomic origin <branch> refs/notes/cinema/v1/<fp>` works on github.com.
- Two clones pushed different `refs/notes/cinema/v1/<fp>` refs at the same moment, and both landed.
- In Actions, the notes fetch takes about 1 s once authenticated (see finding 1). Notes attach to the **PR head SHA**. `GITHUB_SHA` is a synthetic merge commit that never has notes, even when the PR is up to date.
- Fork PR: not tested; it needs a second account. Expected result: no notes, so full CI.

## P0-3 Merge commit vs PR head fingerprint ✅

| PR | Base after branching | merge == head digest? |
|---|---|---|
| A | unchanged | **yes**: head receipts are reusable |
| B | unrelated file added on main | no |
| C | task input changed on main | no |

B and C behave as the plan predicts (§11.2: "when main moves, tasks rerun"). Options to raise reuse later: a merge queue, or a "require up to date" branch rule. Narrower inputs (vision §11.5) would also help but are deferred.

## P0-4 Hashing cost ✅

```bash
go run ./spikes/fingerprint -repo PATH -cache /tmp/fp.cache   # run twice: cold, then warm
```

Synthetic repos (4 KiB random files), Apple M3 Max, 16 cores, OS page cache warm:

| Files | Size | Cold (no metadata cache) | Warm (metadata cache) | Budget |
|---:|---:|---:|---:|---:|
| 1k | 8 MB | 65 ms | 22 ms | — |
| 10k | 63 MB | 490 ms | 63 ms | < 300 ms warm ✅ |
| 100k | 791 MB | 4.47 s | ~0.5 s | measure ✅ |

- Warm runs spend most of their time on the `git ls-files` list step and on `lstat`. Hashing is skipped.
- Not measured yet: a truly cold disk (`sudo purge`), real repos with large binaries, and Linux.

## Reproduce P0-1/2/3

```bash
GOOS=linux GOARCH=amd64 go build -o gatestub-linux-amd64 ./spikes/gatestub
go run ./spikes/gatestub keygen ~/.cinema-p0.key              # prints PUBKEY
go run ./spikes/gatestub sign ~/.cinema-p0.key payload.json > .cinema/p0-envelope.json
gh release create p0-gatestub gatestub-linux-amd64 --prerelease
gh variable set CINEMA_TRUSTED_KEYS --body "$PUBKEY"
cp spikes/workflows/*.yml .github/workflows/                  # then open PRs
```

## Open
- **P0-5 remainder:** measure how often PRs are up to date with base at CI time, using real merged-PR history.
- **P0-6:** license is Apache-2.0 (done). The `predicateType` domain is still unpicked.
- Fork PR behavior (P0-2) needs a second GitHub account.
