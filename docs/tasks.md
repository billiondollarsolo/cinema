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
| `runtime` | Exact language runtime versions (`go`, `node`, `python`, `uv`, `java`, `gradle`, `maven`, `rust`, `bun`, `deno`, `dotnet`). CInema provisions exactly these, from the publisher, checksum-verified, for the task and its probes. See [Runtimes](#runtimes). |
| `toolchain` | Environment inputs. `cmd:` a version command (`[go, version]`, `[tool, --version]`; its output is fingerprinted); `file:` a file whose content counts (≤ 64 MiB, no symlinks); `image:` an image, fingerprinted by resolved digest. |
| `env` | Names of environment variables that are inputs. Their values feed the fingerprint and are never shown. Don't list secrets. |
| `extra_inputs` | Ignored files the task reads (an `.env.test`). Ignored files are otherwise not part of the source. |
| `outputs` | Files or directories the task writes into the tree that aren't ignored (`coverage/`). Left out of its fingerprint and mutation check. A path a task both reads and writes must not be listed. |
| `vcs_context` | `none` (default) or `exact`: the task reads git metadata (commit SHA, tags, `git describe`), so it only reuses on the same commit. |
| `accept_tool_cache` | Default `false`: `go test` must not replay cached results (`-count=1` in the argv), and a run that printed `(cached)` doesn't count. |
| `container` | Run inside this image as root, like a GitHub container job. Its resolved digest is part of the environment; an image given by tag needs `cinema trust` (resolving it runs `docker pull`). |
| `services` | Sidecar containers, like a job's `services:`: `image`, `env`, `ports`, `options` (only `--health-*`, `--tmpfs`, `--shm-size`, `--ulimit`, `--hostname`, `-e/--env`). Image digests are fingerprinted. |
| `docker` | `host`: the task may use the container engine, as a job uses the runner's Docker (Testcontainers, `docker run`/`build`/`compose`). Its socket is mounted at `/var/run/docker.sock` (`DOCKER_HOST` set, the runner user in its group). **Root-equivalent on the engine**: the task can start privileged containers and mount anything the engine can see ([Security](security.md#running-ci-on-your-machine)). Off by default; `cinema analyze` drafts it for jobs that run docker or whose Maven/Gradle build declares Testcontainers, with a note. Part of the contract (without it those tests skip and pass), and the engine's platform is part of the environment. The task runs alone under `-j`; containers and networks that appear on the engine while it runs are removed after it. Natively only (an arm64 engine would run arm64 images under an emulated amd64 task). The ports of containers it starts are at the sandbox's gateway, not `localhost` (`TESTCONTAINERS_HOST_OVERRIDE`, `SERVICES_HOST` point there); a bind mount of the checkout from such a container sees the engine's filesystem, not the checkout. Tests that rely on either fail locally, never pass. The published runner image has no `docker` CLI yet: API clients (Testcontainers) work, steps and tests that exec `docker` (Compose support) fail. |

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
| `floating` | The workflow's version specs the `runtime:` pins came from (`{go: stable, node: "lts/*", python: "3.12"}`). CI resolves them again on every run, as the job's setup action would on that runner, and runs the job if the result isn't the pin. |
| `preinstalled` | Per `floating:` entry of Go, Node, Python, Java and .NET: `true` when the job's setup action uses what the runner has preinstalled first, so CI checks the runner's; `false` when it installs the newest release (`check-latest: true`, mise). Tasks without it (drafted before) are checked both ways. Below. |

## Runtimes

`cinema setup` pins these from the job's setup action (version input, version file, or the action's default) and fingerprints them with the version command. Each is downloaded once per machine from its publisher, checked against the publisher's checksum, and mounted read-only into sandboxes. A version spec that floats (`3.12`, `latest`, `stable`) is pinned to the version the job's setup action would use today and recorded in `floating:`; CI resolves it again on every run, on its runner, and runs the job when that isn't the pin.

Without `check-latest: true`, setup-go, setup-node, setup-python and setup-java (Temurin) take the newest matching version already in the runner's tool cache (`$RUNNER_TOOL_CACHE/<go|node|Python|Java_Temurin-Hotspot_jdk>/<version>/x64`, preinstalled by GitHub's image and updated with it about weekly), and download the newest release only when none matches. So:

- **Drafts** pin what the job's runner image has cached: the image's software report ([actions/runner-images](https://github.com/actions/runner-images/tree/main/images/ubuntu), `Ubuntu2604-Readme.md` for `ubuntu-latest`) lists its Go, Node.js, Python and Java versions; a spec it has nothing for gets the newest release. Unreadable report: the newest release, noted. `preinstalled: {python: true}`.
- **CI** (`cinema verify`, on the runner) reads its own tool cache, as the action matches: complete entries only (`x64.complete`), prereleases left out, the newest satisfying the spec; nothing matching: the newest release. Entries written after the runner booted (a setup step earlier in the gate's job) don't count. `stable` (Go) and `latest` (Node) resolve to the newest release before the cache is looked at; `lts/*` to the newest LTS's major, which the cache answers. `check-latest: true`: the newest release (`preinstalled: false`). A JDK's build comes from its `release` file (the directory name drops a respin's 4th number: `11.0.32-1` is 11.0.32.1+1, Adoptium's `11.0.32+101`). If the task's `runs_on` is another release than the gate runner's, the cache isn't the job's: the job runs. A `check-latest` given by an expression is review.
- **.NET**: setup-dotnet installs into `/usr/share/dotnet` next to the image's SDKs unless the job sets `DOTNET_INSTALL_DIR`; there `dotnet` uses the newest SDK (unless `global.json` picks one) and apps the newest runtime patch present. A single floating spec on the newest GA channel (`10.0.x`, or `latest` when that's GA) is a task: the image's SDKs can't be newer than that channel's newest, and its runtime is the pin's. CI checks the pin is still the channel's newest SDK and that the runner's `/usr/share/dotnet/sdk` has none newer (previews included). Older channels (`8.0.x` next to the image's 10.0), several specs, an exact SDK, or a `global.json` whose `rollForward` is `patch`/`feature`/`minor`/`major` (they take the oldest SDK that fits, the image's) stay review.

| Key | Exact version | Source, checked against | Setup actions translated |
|---|---|---|---|
| `go` | `1.26.10` | go.dev (sumdb) | `actions/setup-go` |
| `node` | `22.23.3` | nodejs.org, `SHASUMS256.txt` | `actions/setup-node`, `pnpm/setup` |
| `python` | `3.12.7` | python-build-standalone (the oldest build of that version, so every machine gets the same files), the release's `SHA256SUMS` | `actions/setup-python` (copied to `$RUNNER_TEMP`, as setup-python's install is writable) |
| `uv` | `0.9.5` | astral-sh/uv releases, `.sha256` | `astral-sh/setup-uv` |
| `java` | `temurin-21.0.5+11` (the vendor is part of the version) | Adoptium API, SHA-256 | `actions/setup-java` (Temurin; other vendors go to review) |
| `gradle` | `8.14.3` | services.gradle.org, `.sha256` | `gradle/actions/setup-gradle`, wrapper validation |
| `maven` | `3.9.15` | archive.apache.org bin zip, `.sha512` | `stCarolas/setup-maven` (`maven-version`, default `3.8.2`) |
| `rust` | `1.83.0`, `nightly-2026-10-01`, plus components and targets: `1.83.0+clippy+rustfmt` | static.rust-lang.org channel manifest (`.sha256`), each package's SHA-256; no rustup | `dtolnay/rust-toolchain`, `actions-rust-lang/setup-rust-toolchain`, `actions-rs/toolchain`, `rust-toolchain(.toml)` |
| `bun` | `1.2.23` | oven-sh/bun releases, `SHASUMS256.txt` | `oven-sh/setup-bun` |
| `deno` | `2.5.6` (2.0.1 and later) | denoland/deno releases, `.sha256sum` | `denoland/setup-deno` |
| `dotnet` | `8.0.404` (SDK) | builds.dotnet.microsoft.com, SHA-512 from `releases.json` | `actions/setup-dotnet` |

A job that uses a runtime with no setup action gets what GitHub's runner really has, not its version file: Go and Node are pinned from `go.mod`/`.go-version` and `.nvmrc`/`.node-version`; Rust from `rust-toolchain(.toml)`, which rustup on the runner installs on first use (no file, or `stable`: review, the image's stable lags); Python is the release's apt `python3` (3.10 on 22.04, 3.12 on 24.04, 3.14 on 26.04, `.python-version` ignored), which the runner image has too, so nothing is pinned and `python3 --version` is probed; `mvn` (not `./mvnw`) is the image's Maven (Apache's zip in `/usr/share`, linked as `/usr/bin/mvn`: 3.10.0 on 22.04 and 24.04, 3.9.15 on 26.04, as of the images of 2026-10-26), which the runner image installs the same way, so `mvn --version` is probed (its `Apache Maven x.y.z (commit)` line only: the rest is the JVM and the kernel) and a CI image with another Maven makes the job run; a bare `gradle` is the image's newest-at-build Gradle (9.8.2), which moves with each image: review; Ant (apt, on every image) and sbt (22.04 only) aren't in the runner image: review; Java and .NET use the image's preinstalled JDK and SDKs, whose builds move with each image release, so they go to review. Bun, Deno and uv aren't on the runner (such a step fails in CI too).

`jdx/mise-action` and `asdf-vm/actions/install` are translated into pins from `mise.toml` (and `.mise.toml`, `mise.local.toml`, `mise/config.toml`, `.config/mise.toml`, … in mise's precedence, then `working_directory`'s), `mise.lock` and `.tool-versions` (asdf reads only that), and the `mise_toml`/`tool_versions` inputs. mise's `java` needs the vendor (`temurin-21`; a bare `21` is OpenJDK's build). `[env]` is mirrored through `$GITHUB_ENV`. Review: tools cinema doesn't provision (named), several versions of one tool, `system`, tool options other than Rust's components/targets/profile, env directives (`_.path`, `_.file`) and templates, hooks, aliases, plugins, `conf.d` fragments, idiomatic version files, `MISE_ENV`, and a floating version with `cache: true` (the action's cache keeps the first install for up to 30 days). `asdf-vm/actions/setup` installs only asdf: review.

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

`image`, `arch: any`, `emulation: forbid`, `credentials:`. See [plan §6.1](../plan.md) and [Machines and engines](hosts.md).
