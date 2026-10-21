# P0 spikes (plan.md §16)

Throwaway code. It answers the feasibility questions and then gets deleted; none of it is the product.

## P0-4 Hashing cost: done (local)

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
- Not measured yet: a truly cold disk (needs `sudo purge`), real repos with large binaries, and the Linux VPS.

## P0-1 Gate cost: needs a real GitHub repo

```bash
GOOS=linux GOARCH=amd64 go build -o gatestub-linux-amd64 ./spikes/gatestub
go run ./spikes/gatestub keygen ~/.cinema-p0.key              # prints PUBKEY
echo '{"_type":"https://in-toto.io/Statement/v1"}' > p.json
go run ./spikes/gatestub sign ~/.cinema-p0.key p.json > p0-envelope.json

# in the target repo:
gh release create p0-gatestub gatestub-linux-amd64 --prerelease --title "P0 spike"
gh variable set CINEMA_TRUSTED_KEYS --body "$PUBKEY"
mkdir -p .cinema .github/workflows && cp p0-envelope.json .cinema/ && cp spikes/gatestub/p0-gate.yml .github/workflows/
# open a PR, then compare:
gh run list --workflow p0-gate.yml --json databaseId,createdAt,updatedAt
gh run list --workflow <existing-ci>.yml --json databaseId,createdAt,updatedAt
```

Decision: if `p0-gate` wall time is not clearly below the existing CI wall time, stop and fix the overhead first.

## Open
- P0-2 git notes round trip, P0-3 merge-commit fingerprint, P0-5 reuse estimate: need a real repo.
- P0-6: license is Apache-2.0 (the plan's default). The `predicateType` domain is still unpicked.
