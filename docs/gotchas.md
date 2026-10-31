# Gotchas

Everything we've seen change a result, from six real projects (cli, eslint, prettier, prometheus, traefik, vite) and the P4 container spike. Each says what goes wrong and what CInema does about it. The numbered findings are in [spikes/lab/RESULTS.md](../spikes/lab/RESULTS.md).

## Things that can make a pass meaningless (false greens)

These are the ones CInema exists to prevent. Each is closed today unless marked planned.

| Gotcha | What CInema does |
|---|---|
| **Ignored files in your working tree.** A force-tracked path that `.gitignore` also matches, stale generated code, an old `node_modules`: your tree has them, CI's checkout doesn't. Tests pass locally, fail in CI. (Finding 1) | Only clean-checkout runs count as evidence ([ADR 0002](adr/0002-ci-evidence-from-clean-checkouts.md)). Each run's checkout is fully reset, ignored files included. |
| **Ignored files leaking between tasks of one run** (39) | Every task gets its own clean checkout. |
| **Two tasks with the same argv** credited for each other (35) | Runs are matched by task id and contract, not by argv. |
| **A PR edits a job** (adds a step) but its old task still matches | Tasks record the job they were drafted from (`job_digest`); the job then runs. |
| **A PR loosens its own rules** by editing `.cinema/tasks.yaml` | CI takes tasks from the base branch. |
| **Tool caches replaying results.** `go test` prints `(cached)` and replays; golangci-lint replayed another build's results from a shared cache. (6, 15) | `accept_tool_cache: false` by default, with `(cached)` detected at run time; tool caches are private per tool version. |
| **Same tool, same version, different artifact.** golangci-lint built from source and the official release disagreed. (14) | Drafts use the official release, pinned and checksum-verified, as the action does. |
| **Floating versions.** `go-version: stable`, `lts/*`, image tags, `ubuntu-latest` move on; a pinned receipt doesn't. | Today: runtimes are pinned and fingerprinted, so drift makes jobs run on the next CInema run, not silently pass. **Planned:** CI re-resolves floating values at check time and compares. |
| **Push-only steps.** A step with `if: github.event_name == 'push'` isn't in a PR-drafted task. | **Planned:** the event is part of the contract. |
| **Retrying until green.** A flaky test fails, you rerun, the pass gets signed. | Today: a failure on the same inputs on your machine blocks reuse of an earlier pass. **Planned:** failed runs are published, so a failure anywhere blocks it. |

## Things that make a local run differ from CI

| Gotcha | What to do |
|---|---|
| **Root vs non-root** changes results: permission tests fail, or skip and pass, as root. (2) | Run evidence as a normal user. The environment fingerprint records it; root and non-root never match. |
| **A machine bigger than the runner.** Worker pools sized by core count: prettier's jest deadlocked on 16 cores; tests time out under load. (37, 42) | Drafts set `cpus: 4`, like the runner. |
| **CPU-saturating neighbours make tests flaky** under `-j`. (27, 33) | `cpus:` per task; `exclusive: true` for tasks that can't share. |
| **Tasks sharing global resources:** fixed ports, a named docker network, apt's lock. (28) | `exclusive: true`. Drafts set it for jobs using `sudo` or services. |
| **Global installs** (`npm i -g pnpm`) landing in the shared runtime and leaking into later runs. (40) | Global installs go to a per-run scratch dir. |
| **Scripts writing to `$HOME`** (traefik's `safe-chain setup-ci` → `~/.safe-chain`). (56) | Clean runs get a private `HOME`, like a fresh runner; download and build caches still point at the real home, so they stay warm. |
| **Missing runner tools.** Scripts call `shellcheck`, `yq`, `xvfb-run`, `unzip`, `ssh` that GitHub's image has and your machine may not. | `cinema doctor` reads failed runs' output and lists what was missing. A missing tool is a failed run, never a false green. |
| **Tools a job `go install`s** aren't found by later steps: `actions/setup-go` puts `GOPATH/bin` on PATH. (P4-1: cli's `go-licenses`, eslint's `actionlint`) | Drafts now add `GOPATH/bin` to PATH where `setup-go` is used. |
| **No lockfile** (eslint): dependencies float between runs. (9, 30) | Nothing to pin; reuse is honest only within `max_age`. Expect review verdicts. |
| **Unpinned build backends.** httpx pins its requirements, but `python -m build` installs today's hatchling in an isolated env; at a February commit it now writes metadata 2.5, which the pinned twine rejects. CI at that commit passed; the same commit fails today, locally and in upstream's own CI. | A result is about the day it ran: `max_age` keeps receipts short-lived. Pin `build-system.requires` to make it about the commit. |
| **Network- and time-dependent checks** (11) | Keep them in CI: `reuse.allowed: false`. |
| **Workflow-level `defaults:`** apply to every job. (43) | Drafts apply them. |
| **Tasks that read git metadata** (`git describe`, the commit SHA) | `vcs_context: exact`. |

## Things about the repo's own tooling

| Gotcha | What to do |
|---|---|
| **The repo's formatter or linter rewrites or rejects CInema's files** (prettier reformatted `tasks.yaml`; eslint's YAML lint rejected empty values; workflow linters checked the gate job). (7, 22, 26, 36, 47, 55) | `setup` formats with the repo's own prettier when installed, and drafts write empty values as `""`. Otherwise add `.cinema/` to the formatter's ignore file. |
| **Upstream config files come along into mirrors** (19) | Only relevant if you mirror a repo to test CInema. |

## Things about the CI side

| Gotcha | What happens |
|---|---|
| **Jobs only start skipping after the setup PR merges** (44) | CI reads tasks from the base branch. |
| **Merging main usually makes everything run** | CI tests the merge commit; any change on main changes the source fingerprint. **Planned:** `cinema push` also tests the merge result. |
| **`include:`-only matrix legs can't be excluded** (55) | GitHub applies `exclude` before `include`. They run unless the whole job skips. |
| **Excluded legs report no check** | `setup` keeps legs that branch protection requires by name in CI. |
| **The `cinema` job costs ~1 minute per workflow**, also when nothing is reused. A fully reused run still pays it. (16, 51) | Worth it once jobs skip: eslint's first real PR went from 81 to 64 billed minutes, Linux minutes −25%. |
| **A fresh runner printed `go: downloading` into a probe**, so receipts didn't match. (45) | Go is provisioned before probes run. |
| **Review drafts can be hollow.** A job whose real work is an action CInema can't translate drafts as review with nothing in it. (48) | `setup` writes ready jobs only, unless `--include-review`. |
| **Drafts must never publish.** (41) | Steps that publish, release or deploy make the job "stays in CI". |

## Things about running in containers (P4 spike, on an M-series Mac)

Containers are **planned**; these are from the spike, for when they land.

| Gotcha | What happens |
|---|---|
| **Your Docker config's keychain helper prompts for a password on every pull** | CInema uses its own empty Docker client config (it keeps your context's endpoint, e.g. Colima). Your registry logins never reach a job. |
| **A tag pulled for one chip shadows the other.** After pulling `alpine:latest` for amd64, a plain `docker run alpine` ran amd64. | CInema always passes `--platform`. |
| **Docker's containerd image store reports the host's arch.** On Docker 29+ an image can hold several platforms; `docker image inspect` shows the host's variant unless you pass `--platform`. | CInema passes `--platform` to inspect as well as to pull and run. |
| **The legacy builder can't build for another chip.** `docker build --platform linux/amd64` on an arm64 Mac produced an arm64 image under the `:amd64` tag; `docker run --platform linux/amd64` then refused it (correctly). | CInema pulls a published multi-arch runner image rather than building one. To build images yourself, use BuildKit (`docker buildx build`). |
| **Nothing is preinstalled in a clean image.** Probes and runtimes must not depend on a system `node` or `go` (the VPS had them, which hid a bug). | Fixed: a pinned runtime's tool is found on the task's own PATH. **Planned:** Go provisioned from the official tarball, without a bootstrap `go`. |
| **Emulation is slow.** Rosetta measured 1.6–3.8× (cold) slower than arm64 native (cold caches). | Emulated amd64 is an option, off by default. |
| **`ubuntu-latest` moves.** On the first real PR from a Mac, CI's runner was Ubuntu 26.04 and the receipts came from a 24.04 image: every job ran (`ENVIRONMENT_MISMATCH`). Correct, and no saving. | The runner image follows each job's `runs-on`, and the OS release is in the environment, so a move is a miss, never a false skip. |
| **Tests exec tools no workflow mentions** (cli's tests run `ssh-keygen`) | The runner image keeps GitHub's base tools, not just what workflows name. |
| **`less` is on GitHub's runner, not in a bare Ubuntu image** (click's pager tests: 24 failures) | Added to the runner image (needs a rebuild and re-pin). |
| **On a Mac, the clean checkout is on macOS's filesystem** (bind-mounted through Colima's virtiofs): names must be valid UTF-8 (click's `test_path_surrogates` fails with EPERM) and it is case-insensitive (`Aa` and `aa` are one file), unlike ext4 on the runner | Known; the run fails (a false red). Case-insensitivity can also pass what Linux fails (an import or `#include` with the wrong case): **planned:** checkouts in a Linux volume. |
| **`DOCKER_CONFIG` pointing elsewhere hides Colima's docker context**: `analyze` reports `docker no` and every job "stays in CI" | Leave `DOCKER_CONFIG` unset for cinema (it uses its own empty client config anyway), or set `DOCKER_HOST`. |
| **GNU tar can't extract nested paths under emulation** (26.04 image, macOS 14 Rosetta: `Cannot mkdir: Function not implemented`) | Steps that untar a download fail emulated (never pass). unzip works. Run such jobs natively or on an x86-64 machine. |
| **x86 CPU flags aren't visible under Rosetta** (`/proc/cpuinfo` comes from the arm64 kernel) | Receipts record emulation; `emulation: forbid` for tasks whose results depend on CPU features. |

## Language runtimes

| Gotcha | What CInema does |
|---|---|
| **pip installs into the interpreter**, and pinned runtimes are mounted read-only | The setup-python draft copies the runtime to `$RUNNER_TEMP` and points PATH and `pythonLocation` at the copy, as setup-python's install is writable in CI (about 2 s a task). |
| **Same Python version, another build**: python-build-standalone ships 3.12.7 in several releases | The oldest build of the pinned version, so every machine gets the same files. |
| **Same Java version, another vendor**: Temurin, Zulu and Corretto 21.0.5 are different builds | The pin names the vendor (`temurin-21.0.5+11`); only Temurin is provisioned, other distributions go to review. |
| **setup-java without `check-latest` uses the runner's preinstalled JDK**, which lags the newest after a release | A floating `java-version` without `check-latest: true` goes to review. Lab: gson's CI ran Temurin 17.0.20 from `/opt/hostedtoolcache/Java_Temurin-Hotspot_jdk/17.0.20-1` (the image's own install) while Adoptium's newest was 17.0.21+9. About 0.5% of workflows using setup-java mention `check-latest`. |
| **`mvn` without the wrapper** is the runner image's Maven, which floats on 22.04 and 24.04 (their toolsets say `3`: the newest 3.x when the image is built, 3.10.0 in Oct 2026) and is 3.9.15 on 26.04 | The runner image installs the same Apache zip the same way, per release. The draft pins nothing and probes `mvn --version` on both sides (the `Apache Maven` line only), so when GitHub's image moves, CI measures another Maven and the job runs there until the runner image follows. Not a pin: CI provisions pins itself and would probe cinema's Maven, not the job's. `stCarolas/setup-maven` is a pin (`maven:`). A bare `gradle` (the image's newest-at-build Gradle), `ant` and `sbt` go to review. |
| **setup-java with several versions** (`java-version: \| 8 21 25`, Gradle toolchains) | Review: a task pins one JDK. Common in Gradle libraries (gradle-versions-plugin, NullAway). |
| **Tests that need Docker skip themselves** (Testcontainers' `disabledWithoutDocker`, Spring Boot's Docker Compose support) | The sandbox has no Docker: the task passes with fewer tests than CI ran. Lab: petclinic's Maven build passed with 4 database tests skipped or absent. Keep such jobs in CI until tasks can use Docker (planned). |
| **Gradle's build cache replays test results** | Only dependency downloads and wrapper distributions are shared between runs; Develocity remote caches go to review. |
| **`mvn install` leaves the project's own SNAPSHOTs in `~/.m2/repository`**, and a later `mvn -pl module` (no `-am`) resolves them instead of building them: in a shared repository, another commit's code | Maven 3.9's split local repository (`/etc/mavenrc` adds `-Daether.enhancedLocalRepository.split=true` to `MAVEN_OPTS`): downloads go to `cached/`, the cache volume; installs to `installed/`, in the container, gone with it. Each run starts with none installed, as on a fresh runner. Older Maven keeps the whole repository in the container: cold, never stale. A path written as `${settings.localRepository}/org/…` (a `-javaagent:` in `argLine`) isn't there locally, so that run fails; `dependency:properties` gives the real path. |
| **Two Gradle builds in separate containers on one cache volume**: the second waits for the first to release its lock, asks over UDP, never reaches it ("Timeout waiting to lock artifact cache") | Runtime caches (`~/.m2`, `~/.gradle`, cargo, NuGet, bun) are a volume per clean-checkout slot, so one container at a time uses each, as a runner has its own. cargo (`$CARGO_HOME/.package-cache`), NuGet (`/tmp/NuGetScratch`) and Maven (in-JVM) lock outside the shared directory too. Go modules, `~/.cache` (Go build, pip, uv) and npm lock within it or write atomically, and stay shared. Disk: one set per concurrent slot. |
| **`components: clippy` or `targets: wasm32-…` change what a Rust job can do** | They're part of the pin (`1.83.0+clippy+wasm32-unknown-unknown`): a toolchain without clippy never stands in for one with it. |
| **setup-rust-toolchain v1 sets `RUSTFLAGS="-D warnings"`, v2 `CARGO_BUILD_WARNINGS=deny`** | Mirrored per major version (from the action's ref), unless the job sets them. |
| **The runner image has no rustup** | Steps that run `rustup component add` or `cargo +nightly` fail locally (no receipt); put components in the action's inputs or `rust-toolchain.toml`. |
| **setup-dotnet installs next to the runner's 11 preinstalled SDKs**: without global.json the newest wins, and tests roll forward to the newest runtime | Review unless the job sets `DOTNET_INSTALL_DIR` to a directory of its own and `skip-lts-prepass: true`; then CI's `dotnet` is exactly the pinned SDK. About 0.4% of workflows using setup-dotnet set `DOTNET_INSTALL_DIR`, so nearly every .NET job is review today. |
| **The .NET SDK aborts without ICU** | The runner image has the release's `libicu` like GitHub's; never `DOTNET_SYSTEM_GLOBALIZATION_INVARIANT`, which changes culture-dependent results. |
| **Headless Java AWT fails without fonts** ("Fontconfig head is null") | The runner image has fontconfig and DejaVu fonts. |
| **setup-bun picks `-baseline` on x64 CPUs without AVX2; emulated containers show no CPU flags** | Always the AVX2 build hosted runners get. It runs under Rosetta from macOS 15 on; on macOS 14 every bun command dies with SIGILL (exit 132), and the run says so (illegal instruction). Bun jobs need macOS 15+ or an x86-64 machine. |
| **`npm install -g` in a container** wrote into the read-only runtime mount (ENOENT) | Global npm/pnpm installs go to the container's home, as on the host. |
| **pnpm's store in a bind-mounted checkout**: pnpm 12 put it in `node_modules/.pnpm-store` and failed to import from it on Colima's virtiofs | In containers the store is on the download-cache volume (`pnpm_config_store_dir`). |
| **setup-node twice in one job** (hono: the matrix's Node for `pnpm install`, then `.tool-versions`' for the tests) | Review: a task pins one Node. The `Node.js v20.x` job actually tests on 24. |
| **Deno's zip on dl.deno.land isn't the same file as GitHub's** | Downloads from GitHub, as setup-deno does. Deno 1.x and 2.0.0 publish no checksum and aren't provisioned. |
| **GitHub's API allows 60 unauthenticated requests an hour** | Release lists come from sources without that limit: git's own protocol (bun tags), dl.deno.land, Astral's and Adoptium's indexes, Rust's manifests list. |
| **Without setup-python, `python`/`pip` are Ubuntu's apt python3**, not `.python-version` nor the toolcache's (GitHub's image adds `python-is-python3`, `python3-pip` and `break-system-packages` in `/etc/pip.conf` on 24.04 and later) | No pin: the runner image installs the same, and drafts probe `python3 --version` on both sides. |
| **Without setup-java (or setup-dotnet), the image's preinstalled JDK (Temurin 11/17/25 by release) or SDKs are used**, and their builds change with every weekly image | Review: no pin can be that build reliably. |
| **Without a Rust setup action, rustup installs `rust-toolchain.toml`'s channel on first use**; with no file it's the image's stable, which lags | The file's channel is pinned; no file or `stable`: review. |
| **mise-action caches installed tools by the config's hash for up to 30 days**: `node = "22"` stays at the first 22.x installed | A floating mise version with `cache: true` goes to review; pin exact (or commit `mise.lock`), or set `cache: false`. |
| **mise's `java = "21"` is OpenJDK's build**, not Temurin; **mise sets `RUSTUP_TOOLCHAIN`**, which beats `rust-toolchain.toml` | Only `temurin-…` is pinned; mise's rust version is the pin, whatever the file says. |
| **ruby/setup-ruby's rubies only work at `/opt/hostedtoolcache/Ruby/<ver>/<arch>`** and publish no checksums | Ruby isn't provisioned; Ruby jobs stay in CI. |
| **setup-python without `check-latest` uses the runner's preinstalled Python**, which lags the newest: click's `3.14` ran 3.14.7 (Oct 4) and 3.14.9 (Oct 30) while the newest was 3.14.10 | A floating `python-version` without `check-latest: true` goes to review, as for Java. (setup-go and setup-node behave the same way; not yet flagged.) |
| **rust-toolchain.toml with only `components`** (dtolnay's crates: `rust-src`) | rustup adds them to the toolchain the action made the default; drafts pin that toolchain plus the components. |
