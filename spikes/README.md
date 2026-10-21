# P0 spikes (plan.md §16)

Throwaway code. It answers the feasibility questions and then gets deleted; none of it is the product.
GitHub spikes ran on 2026-10-21 in the private throwaway repo `mjtechguy/cinema-p0`, using the workflows in `workflows/`.

## Findings that change the plan

1. **Private repos cannot fetch notes with `persist-credentials: false`.** The fetch fails with `could not read Username`. Fix: pass the job token for that one fetch only, via
   `git -c http.extraheader="AUTHORIZATION: basic $(printf x-access-token:%s "$TOKEN" | base64 -w0)" fetch origin '+refs/notes/cinema/*:refs/notes/cinema/*'`.
   The token stays out of `.git/config`, so later task steps can't read it. Public repos are unaffected.
2. **Per-job billing rounds up to whole minutes.** A 10 s gate bills 1 minute, the same as a 44 s CI job. On private repos, billed minutes go down only when the reused work spans more than 1 billed job-minute. Wall time improves either way. The pitch should lead with wall time.
3. **Whole-source receipts reuse only when the PR branch already contains the latest base.** If base has moved at all, even in an unrelated file, the merge-commit fingerprint differs from the PR head's (P0-3). Reuse rate therefore depends on how often PRs are up to date at CI time. This is the number P0-5 needs from real repos.

## P0-1 Gate cost: floor measured ✅ (real-repo comparison pending)

| Workflow (run-start → finish, excludes queue) | PR A | PR B | PR C |
|---|---:|---:|---:|
| `p0-gate`: checkout + download binary + 1 DSSE verify | 10 s | 11 s | 10 s |
| `p0-baseline`: setup-go + vet + `go test` on a toy repo | 46 s | 44 s | 41 s |

Gate steps: checkout under 1 s, binary download 1 s, verify under 1 s. The rest is runner setup and teardown.
The baseline is deliberately tiny. Any real Go/Node CI is slower, so ~10 s is the floor to beat. This doesn't decide the kill/continue question yet: that needs one real repo's CI times (P0-5).

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
- **P0-5:** list CI jobs and their durations for 2–3 real repos, decide which tasks are honestly `platform: any`, and measure how often PRs are up to date with base at CI time.
- **P0-6:** license is Apache-2.0 (done). The `predicateType` domain is still unpicked.
- Fork PR behavior (P0-2) needs a second GitHub account.
