# Machines and engines

Where CInema runs your jobs, today and as planned.

## Today

Evidence for Linux jobs comes from a **Linux x86-64 machine** you control (a desktop, a VPS), running tasks as normal processes in clean checkouts. Tasks with `container:` or `services:` need Docker on that machine. Tasks with `platform: any` (lint, format) can come from any machine, including a Mac.

`cinema doctor` lists tools a failed run needed that the machine lacks. `cinema analyze` shows per job whether this machine is ready for it.

## Planned: local-first containers on any machine (P4)

Designed in [plan.md §9](../plan.md) and [ADR 0003](adr/0003-local-first-container-execution.md). The P4-1 spike ran real tasks this way on a Mac; the code isn't in CInema yet.

Each Linux CI job runs in a **Linux container on your own machine**, shaped like the job's runner: a pinned Ubuntu image, a non-root `runner` user with passwordless sudo, the runner's paths (`/home/runner/work/<repo>/<repo>`), a fresh home, UTC, the runner's CPU and memory, and a clean checkout made inside the container. Windows and macOS CI jobs stay in CI.

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

A receipt only counts for a job whose runner has the same architecture the run had. **Emulation is an option, off by default:** turn it on with `emulate: [amd64]` in your user config. Without it, an Apple-silicon Mac leaves amd64 jobs to CI or to an amd64 remote host.

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

### Remote machines (an option, never required)

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
