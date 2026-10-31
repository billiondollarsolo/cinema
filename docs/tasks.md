# Tasks reference

`.cinema/tasks.yaml` holds the work CInema runs and CI may skip. `cinema setup` and `cinema analyze --yaml` draft it from your workflows; you can also write tasks by hand. The parser is strict: unknown fields and wrong types are errors.

```yaml
schema_version: 1
tasks:
  test-22-ubuntu-latest:                # task id: lowercase, digits, . _ -; at most 64 characters
    argv: [bash, -eo, pipefail, -c, "npm ci\nnpm test"]
    platform: linux/amd64
    runtime: { node: 22.23.4 }
    toolchain:
      - cmd: [node, --version]
    covers: ci.yml#test
    leg: { os: ubuntu-latest, node: 22.x }
    job_digest: sha256:…
    cpus: 4
    timeout: 60m
    reuse: { allowed: true, max_age: 24h }
```

## What the work is (part of the contract)

Changing any of these changes the task's contract digest: older receipts stop matching.

| Field | Meaning |
|---|---|
| `argv` | The exact command, no shell unless you write one. Drafts use `bash -eo pipefail -c` with each workflow step in its own subshell, as Actions runs them. |
| `cwd` | Working directory, relative to the repo root. Default `.`. |
| `platform` | `goos/goarch` of the machine that must run it (`linux/amd64`), or `any`: an explicit claim that the result doesn't depend on the OS (lint, format). Never inferred. |
| `runtime` | Exact language runtime versions (`go`, `node`, `python`, `uv`, `java`, `gradle`, `rust`, `bun`, `deno`, `dotnet`). CInema provisions exactly these, from the publisher, checksum-verified, for the task and its probes. See [Runtimes](#runtimes). |
| `toolchain` | Environment inputs. `cmd:` a version command (`[go, version]`, `[tool, --version]`; its output is fingerprinted); `file:` a file whose content counts (≤ 64 MiB, no symlinks); `image:` an image, fingerprinted by resolved digest. |
| `env` | Names of environment variables that are inputs. Their values feed the fingerprint and are never shown. Don't list secrets. |
| `extra_inputs` | Ignored files the task reads (an `.env.test`). Ignored files are otherwise not part of the source. |
| `outputs` | Files or directories the task writes into the tree that aren't ignored (`coverage/`). Left out of its fingerprint and mutation check. A path a task both reads and writes must not be listed. |
| `vcs_context` | `none` (default) or `exact`: the task reads git metadata (commit SHA, tags, `git describe`), so it only reuses on the same commit. |
| `accept_tool_cache` | Default `false`: `go test` must not replay cached results (`-count=1` in the argv), and a run that printed `(cached)` doesn't count. |
| `container` | Run inside this image as root, like a GitHub container job. Its resolved digest is part of the environment; an image given by tag needs `cinema trust` (resolving it runs `docker pull`). |
| `services` | Sidecar containers, like a job's `services:`: `image`, `env`, `ports`, `options` (only `--health-*`, `--tmpfs`, `--shm-size`, `--ulimit`, `--hostname`, `-e/--env`). Image digests are fingerprinted. |

## How it runs (not part of the contract)

| Field | Meaning |
|---|---|
| `cpus` | Linux: the task sees only this many CPUs (affinity, `--cpuset-cpus` in a container), like the runner it replaces. Drafts use 4. |
| `exclusive` | Never run alongside other tasks under `-j`: fixed ports, a fixed docker network, `sudo`. Tasks with services are always exclusive. |
| `timeout` | Stop a run that takes longer; a timeout is a failure. Drafts use the job's `timeout-minutes`. In CI, a receipt whose run used over half of it isn't reused (`TOO_CLOSE_TO_TIMEOUT`). |
| `runs_on` | The covered job's runner label. Picks the runner image, so the OS release matches CI's. Drafted. |
| `memory` | The runner's memory (`16g`), with `cpus:` the runner size a container run imitates and reports. Part of a Linux task's environment: a 4-CPU receipt doesn't count on a 2-CPU private runner. |

## How it maps to CI (metadata, not part of the contract)

| Field | Meaning |
|---|---|
| `covers` | The job this task stands in for: `workflow.yml#job-id`. Only tasks with `covers` make jobs skip. |
| `leg` | For a matrix job: this leg's values for the matrix's own keys. A reused leg is excluded. |
| `job_digest` | The job leg as drafted. The job only skips if the workflow under test still has exactly that leg. |
| `graduated` | Written by `cinema graduate`: the task agreed with CI for this `job_digest`, on `backtested` past commits (`ci_failures` of them failing) and `agreed` PR trees. Without it (or for another digest) the job runs in CI whatever the receipts say (shadow mode). |
| `event` | The job reads the triggering event or ref, so the task counts only for this event (`pull_request`, `push`, `merge_group`). Drafted automatically. |
| `floating` | The workflow's version specs the `runtime:` pins came from (`{go: stable, node: "lts/*", python: "3.12"}`). CI resolves them again on every run and runs the job if today's release isn't the pin. |

## Runtimes

`cinema setup` pins these from the job's setup action (version input, version file, or the action's default) and fingerprints them with the version command. Each is downloaded once per machine from its publisher, checked against the publisher's checksum, and mounted read-only into sandboxes. A version spec that floats (`3.12`, `latest`, `stable`) is pinned to today's release and recorded in `floating:`, so CI runs the job once the release moves.

| Key | Exact version | Source, checked against | Setup actions translated |
|---|---|---|---|
| `go` | `1.26.10` | go.dev (sumdb) | `actions/setup-go` |
| `node` | `22.23.3` | nodejs.org, `SHASUMS256.txt` | `actions/setup-node`, `pnpm/setup` |
| `python` | `3.12.7` | python-build-standalone (the oldest build of that version, so every machine gets the same files), the release's `SHA256SUMS` | `actions/setup-python` (copied to `$RUNNER_TEMP`, as setup-python's install is writable) |
| `uv` | `0.9.5` | astral-sh/uv releases, `.sha256` | `astral-sh/setup-uv` |
| `java` | `temurin-21.0.5+11` (the vendor is part of the version) | Adoptium API, SHA-256 | `actions/setup-java` (Temurin; other vendors go to review) |
| `gradle` | `8.14.3` | services.gradle.org, `.sha256` | `gradle/actions/setup-gradle`, wrapper validation |
| `rust` | `1.83.0`, `nightly-2026-10-01`, plus components and targets: `1.83.0+clippy+rustfmt` | static.rust-lang.org channel manifest (`.sha256`), each package's SHA-256; no rustup | `dtolnay/rust-toolchain`, `actions-rust-lang/setup-rust-toolchain`, `actions-rs/toolchain`, `rust-toolchain(.toml)` |
| `bun` | `1.2.23` | oven-sh/bun releases, `SHASUMS256.txt` | `oven-sh/setup-bun` |
| `deno` | `2.5.6` (2.0.1 and later) | denoland/deno releases, `.sha256sum` | `denoland/setup-deno` |
| `dotnet` | `8.0.404` (SDK) | builds.dotnet.microsoft.com, SHA-512 from `releases.json` | `actions/setup-dotnet` |

Ruby isn't provisioned: `ruby/setup-ruby`'s prebuilt rubies only work at their build path and publish no checksums, so Ruby jobs stay in CI ([Gotchas](gotchas.md)).

## Reuse policy

```yaml
reuse:
  allowed: true      # false: the job always runs in CI
  max_age: 24h       # a receipt older than this doesn't count
  capture: clean     # clean (default): only clean-checkout runs count. workspace: also working-tree runs, an explicit claim that nothing ignored matters
```

Policy isn't part of the contract: tightening it rejects old receipts without changing what the work was.

## Writing tasks that reuse

Rules from real projects ([lab results](../spikes/lab/RESULTS.md)):

1. **Make the task what CI runs, artifact included.** A tool built from source and its official release of the same version can disagree. Drafts download the official release, pinned and checksum-verified, as CI's action does.
2. **Pin runtimes.** Without `runtime:`, `go version` and `node --version` are fingerprinted to the patch, and any drift on either side is a miss. One task per runtime version in a matrix.
3. **Probe only what isn't pinned in source.** pnpm and yarn versions come from `packageManager`, already in the source fingerprint.
4. **Keep tool caches per tool version.** A cache shared across versions replayed another build's lint results.
5. **Include setup in the task.** Clean checkouts have no `node_modules` or `dist/`: `npm ci && …` belongs in the argv; warm caches keep it fast.
6. **Declare outputs a task writes into the tree.**
7. **Leave never-reusable work in CI** (`reuse.allowed: false`): time-boxed fuzzing, live-network checks.
8. **Run as non-root** when CI does. Root changes test results.
9. **Make `.cinema/tasks.yaml` pass your formatter**, or list it in the formatter's ignore file.

## Planned fields

`image`, `arch: any`, `emulation: forbid`, `docker: host`, `credentials:`. See [plan §6.1](../plan.md) and [Machines and engines](hosts.md).
