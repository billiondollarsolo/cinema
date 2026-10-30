# P4-1 spike: Linux CI jobs in containers on a Mac

2026-10-30. Apple-silicon Mac (16 cores, 128 GB), macOS 14.6.1, Colima 0.10.3 (`--vm-type vz --vz-rosetta`, 8 CPUs / 16 GB VM), Docker 29.9. Each task ran in a runner-like container (this directory's `Dockerfile`: Ubuntu 24.04, non-root `runner` with sudo, 4 CPUs / 16 GB / 2 GB shm), in a clean checkout, from a fresh `cinema analyze --yaml` draft. amd64 ran under Rosetta, arm64 natively. "Warm" is a second run in the same container (Go/npm caches, CInema's shared artifact).

Apple `container` couldn't be tried: it needs macOS 26. Windows hasn't been tried yet.

## Results (after the fixes below)

| Task | arm64 cold | arm64 warm | amd64 (Rosetta) cold | amd64 warm | Linux VPS (reference) |
|---|---:|---:|---:|---:|---:|
| cli `lint` | ✓ 2m14s | ✓ 1m20s | ✓ 8m35s | ✓ 1m21s | 1m26s |
| cli `build` (`go test -race ./...`) | ✓ 3m24s | ✓ 2m31s | ✓ 9m50s | ! 2m29s, go test cache replay: not evidence | 3m26s |
| cli `govulncheck` | ✓ 10s | | ✓ 30s | | 6s |
| eslint `test-types` | ✓ 47s | | ✓ 1m15s | | 1m01s |
| eslint `pnpm-test` | ✓ 3s | | ✓ 9s | | 10s |
| eslint `verify-files` | ✗ expected | ✗ expected | ✗ expected | ✗ expected | |
| traefik cross-build (+ web UI) | ✓ 2m05s | ✓ 4s | ✓ 6m28s | ✓ 10s | 46–55s |

- **Every result matched the lab box and CI** once CInema's bugs were fixed. `verify-files` fails because the spike commits the raw, unformatted draft and eslint format-checks every YAML file; `cinema setup` formats it with the repo's prettier. The failure is right.
- **Rosetta costs 1.6–5.2× cold** (cli lint 3.8× here, build 2.9×, traefik 3.1×). Warm runs mostly close the gap: cli lint is the same on both arches once golangci-lint's cache is warm.
- **Native arm64 is as fast as the 16-core VPS** for cli, and close for eslint.
- **The tool-cache guard worked:** the warm amd64 `go test` replayed cached results and was refused as evidence.

## What it found

CInema bugs, fixed:
1. **Probes and the task's own command didn't find a pinned runtime's tool** unless a system copy existed. `exec.Command` keeps its failed lookup even after `Path` is set. On the VPS a system `node`/`go` masked it.
2. **`actions/setup-go` puts `GOPATH/bin` on PATH;** drafts didn't. Tools a job `go install`s weren't found (cli's `go-licenses`, eslint's `actionlint`). The VPS's login PATH masked it.
3. **CInema's docker calls used the user's `~/.docker/config.json`,** whose keychain credential helper prompted for a password on every pull. Now an empty config in CInema's data dir, keeping the user's context endpoint.

Design inputs (plan §9):
4. **Go provisioning needs a bootstrap `go`** (`GOTOOLCHAIN`). A clean image has none: provision Go from the official tarball, like Node.
5. **Slim images must keep GitHub's base tools.** cli's tests exec `ssh-keygen`; no workflow mentions it.
6. **Always pass `--platform`**, to pull, run *and* inspect: a tag pulled for one arch satisfies the other, and Docker 29's containerd store reports the host's variant on inspect.
7. **The legacy builder can't build for another arch** (an `:amd64` tag held an arm64 image). Publish runner images multi-arch; don't build them on developer machines.
8. **Under Rosetta, x86 CPU flags aren't visible** in `/proc/cpuinfo` (it's the arm64 kernel's).

## Reproduce

```sh
brew install colima docker docker-buildx
colima start --vm-type vz --vz-rosetta --cpu 8 --memory 16
export DOCKER_CONFIG=$(mktemp -d); echo '{"cliPluginsExtraDirs":["/opt/homebrew/lib/docker/cli-plugins"]}' > $DOCKER_CONFIG/config.json
for a in amd64 arm64; do docker buildx build --load --platform linux/$a -t cinema-spike-runner:$a .; done
GOOS=linux GOARCH=amd64 go build -o cinema-amd64 ../../cmd/cinema; GOOS=linux GOARCH=arm64 go build -o cinema-arm64 ../../cmd/cinema
./rerun.sh        # ARCHES=amd64 ./rerun.sh for one arch
```
