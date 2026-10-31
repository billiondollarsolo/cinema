# Machines and engines

Where CInema runs your jobs, today and as planned.

## Local-first containers on any machine

([plan.md §9](../plan.md), [ADR 0003](adr/0003-local-first-container-execution.md).) With `engine: docker` in your config, each Linux CI job runs in a **Linux container on your own machine**, shaped like the job's runner: a pinned Ubuntu image of the job's own `runs-on` release (`runner/Dockerfile`, published for 22.04, 24.04 and 26.04; `ubuntu-latest` is 26.04 now, and when GitHub moves it, CI measures the new OS and stops reusing until CInema follows; `image:` in your config or `CINEMA_RUNNER_IMAGE` overrides), a non-root `runner` user with passwordless sudo and your uid, the runner's paths (`/home/runner/work/<repo>/<repo>`), a fresh home, UTC, `GITHUB_ACTIONS`/`RUNNER_*` basics, the runner's CPU and memory (4 CPUs / 16 GB for public repos, 2 / 8 GB for private ones, as GitHub's), pinned Go/Node from their official releases mounted read-only, per-repo download and build caches, and nothing else from your machine. Tests run fresh (`go test` never replays cached results). Each container is removed after its task, and removes itself after a day even if CInema dies. Windows and macOS CI jobs stay in CI.

Without an engine configured, a Linux x86-64 machine runs tasks as normal processes in clean checkouts, as before. Tasks with `platform: any` (lint, format) run anywhere.

`cinema debug <task>` runs a task in its container and leaves you a shell there. `cinema doctor` lists tools a failed run needed. `cinema analyze` shows per job whether this machine can take it over, and what to set if not.

**On a VM-based engine (Colima, Docker Desktop, OrbStack)** the engine sees only the folders it shares; Colima shares your home directory by default. CInema refuses to run a task whose git objects the engine can't see, rather than run it against an empty folder.

**Checkouts live in the VM's Linux filesystem there.** A folder shared from macOS or Windows keeps that filesystem's rules: names are case-insensitive (`Aa` and `aa` are one file, so an import or `#include` with the wrong case passes on a Mac and fails on the runner) and must be valid UTF-8 (click's surrogate-path test failed with EPERM). So when the engine runs in a VM (any engine on macOS or Windows, Docker Desktop on Linux, or a `tcp://`/`ssh://` `DOCKER_HOST`), each clean-checkout slot has a Docker volume (`cinema-checkout-…`): before every task it's emptied, the commit is checked out into it by git inside the VM (a shared clone of your repo's objects, mounted read-only), HEAD must be the commit and nothing untracked or ignored there. The task runs on it, and the source fingerprint, the mutation check, `.cinema/tasks.yaml`'s check and the JUnit reports are all read from the volume, by a fresh helper container, never from a host copy the task didn't see. `cinema debug` opens its shell on it. A local Linux engine bind-mounts the host checkout, as before. `CINEMA_CHECKOUT=volume` or `bind` overrides the choice. Measured on an M-series Mac with Colima: click's tests (178 files) went from a false red (1 failed) to CI's 2262 passed in the same 10 s; a 20,000-file tree costs about 2 s more per task than a bind mount (checkout and two fingerprints).

### Engines

| Engine | Where | Notes |
|---|---|---|
| Docker Engine / Docker Desktop | Linux, macOS, Windows (WSL2) | Docker Desktop needs a paid subscription in larger companies |
| Podman | Linux, macOS, Windows | |
| Colima | macOS, Linux | Free, command line only. `colima start --vm-type vz --vz-rosetta` gives amd64 through Rosetta |
| OrbStack | macOS | Fast; free for personal use, paid for commercial use |
| Rancher Desktop | macOS, Windows, Linux | |
| Apple `container` | macOS 26+ on Apple silicon | Each container is its own small VM; amd64 through Rosetta (`--arch amd64`). Can't give jobs a Docker socket |

You pick the engine; `cinema doctor` shows what's installed and what each can do.

**CInema uses its own Docker client config.** It never reads `~/.docker/config.json`, which can name a keychain credential helper (on a Mac: a keychain password prompt on every pull) and holds your registry logins. It keeps the endpoint of your current Docker context, so Colima, OrbStack or a remote `DOCKER_HOST` keep working.

### CPU architecture

| Your machine | amd64 jobs (`ubuntu-latest`) | arm64 jobs (`ubuntu-24.04-arm`) |
|---|---|---|
| Linux / Windows, x86-64 | native | emulated (QEMU), opt-in |
| Mac (Apple silicon), ARM Linux / Windows | **emulated (Rosetta), opt-in** | native |

A receipt only counts for a job whose runner has the same architecture the run had. **Emulation is an option, off by default:** turn it on with `emulate: [amd64]` in your user config (or `CINEMA_EMULATE=amd64`). Without it, an Apple-silicon Mac leaves amd64 jobs to CI or to an amd64 remote host.

What emulation costs, measured in the P4-1 spike (M-series Mac, Colima with Rosetta, 4 CPUs / 16 GB per container; [details](../spikes/local-container/RESULTS.md)):

| Task | arm64 native, cold | amd64 under Rosetta, cold | Slower by | Warm, both arches |
|---|---:|---:|---:|---|
| cli `lint` (golangci-lint + checks) | 2m14s | 8m35s | 3.8× | 1m20s, the same |
| cli `build` (`go test -race ./...`) | 3m24s | 9m50s | 2.9× | ~2m30s |
| cli `govulncheck` | 10s | 30s | 3.0× | |
| eslint `test-types` | 47s | 1m15s | 1.6× | |
| traefik cross-build (with its web UI) | 2m05s | 6m28s | 3.1× | 4s vs 10s |

So: 1.6–3.8× slower cold, and warm caches close most of the gap. For reference, the same cli tasks took 1m26s and 3m26s on a 16-core Linux VPS: native arm64 on a Mac is as fast.

Other emulation gotchas: x86 CPU features differ (code that picks a SIMD path at runtime may take another one); timing-sensitive tests can time out. Receipts record `emulated: true`, and a task can forbid it (`emulation: forbid`).

**`arch: any`** in a task is the honest way to run CPU-independent checks (lint, format, most pure-JS tests) natively on any machine. It's your claim, written in the task, and shown in the PR summary.

### Remote machines (planned: an option, never required)

When one machine isn't enough, send work elsewhere. The task and its rules don't change:

```text
cinema run --on local                 (default)
cinema run --on ssh:buildbox          any machine with an engine, over SSH (a VPS, a desktop at home)
cinema run --on boat:large -j 16      an on-demand machine billed by the second (later, through plugins)
```

- **Your machine signs.** Remote machines never hold a key. CInema sends the exact commit, checks the remote's source fingerprint against its own, records the run and signs it.
- **Machines are always released**, also after a crash or Ctrl-C, and every lease has a provider-side time limit as a backstop.
- **Spend is shown before it happens** (time and cost estimate) and capped per push and per month. CInema never spends money without a yes.
- **Credentials stay in your OS keychain**, never in the repo.

Order of delivery: SSH hosts first (P4), then provider plugins, fan-out across several machines, and spend policy (P5).

## Choosing

- **You have a Linux x86-64 machine:** run there today.
- **You're on an Apple-silicon Mac:** P4 runs arm64 jobs and `arch: any` tasks natively. For amd64 jobs, turn on emulation if 2–5× slower is acceptable, or add an amd64 host.
- **You're on Windows (x86-64):** P4 runs amd64 Linux jobs natively through Docker Desktop or Podman on WSL2.
