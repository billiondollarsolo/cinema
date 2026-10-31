# CInema — MVP Build Plan

> **Revision:** 2.0, 2026-10-30: local-first container execution on any host, remote machines as an option (§9, ADR 0003). 1.0 (MVP cut) was 2026-10-21.
> **Status:** P0–P1 implemented, P2 mostly (see §16 checkboxes and [spikes/lab/RESULTS.md](spikes/lab/RESULTS.md)); `cinema analyze` started (P3). Commands, schemas, and config keys are contracts.
> **Long-range design:** [vision.md](vision.md) (the full 0.2 enterprise spec, unchanged). Referenced below as `vision §N`.
> **Positioning:** Run your CI before you push, on your own machine, from the workflows you already have. CI skips what's proven and runs only what remains.

## 0. Decisions already made

| Decision | Choice |
|---|---|
| Language | Go. One static binary: CLI, recorder, verifier, dashboard server. |
| License | Apache-2.0. |
| Audience | Anyone with a repo — public OSS, private-for-now, private-forever. Solo devs through large companies. No paid tier is planned. |
| Trust mode | **Personal only** for the MVP: the repo owner trusts the keys they register, one per developer machine. Managed/Verified modes are in `vision §7, §24–25`. |
| What runs locally | **Linux CI jobs**, from any developer machine: macOS, Linux or Windows. Windows and macOS CI jobs stay in CI for now. |
| Where it runs | **Local first.** Each job runs in a Linux container on the developer's machine (§9). Remote machines (an SSH host, or an on-demand machine billed by the second) are an option the developer picks when they want to scale, never a requirement. |
| Container engine | Pluggable: any Docker-compatible engine (Docker, Podman, OrbStack, Colima, Rancher Desktop) or Apple `container` on macOS 26+. `cinema doctor` detects what's installed and the user chooses. |
| Platform reuse | Exact match: a receipt counts only for a job whose runner has the same OS and CPU architecture the run had. On an arm64 host, amd64 jobs run emulated (Rosetta, QEMU) only if the developer turns emulation on; the receipt says so. Otherwise they stay in CI or go to an amd64 host (§9.3). `arch: any` is an explicit, per-task opt-in, never inferred (§6.1). |
| Accuracy | A job starts skipping in CI only after its local stand-in has agreed with CI on real PRs (shadow mode, §11.5). Disagreement sends it back to CI. |
| Dashboard | Small, local, embedded in the binary. No Node at runtime, no React stack in the MVP. Never a public bind; a dashboard on another machine is reached through an SSH tunnel (`ssh -L`) or `tailscale serve` with `--allow-host`. |
| First provider | GitHub Actions. GitLab is next (§16, P3). |
| AI | None inside CInema. Coding agents use the JSON CLI (§10.3). |

### 0.1 Why this is worth building

- Developer machines are usually **faster than shared hosted runners**: warm caches, more cores, no queueing. No extra hardware is needed; remote machines are there for when one machine isn't enough.
- Developers and coding agents **already run** `go test`, `npm run lint`, and the rest before pushing. CI repeats that work.
- **Public repos:** Actions minutes are free, so the win is time to green and less queueing.
- **Private repos:** the same time win, plus real reductions in billed minutes.
- The same tool covers both. A repo changing visibility only changes the runner size receipts are matched against (§6.1).

### 0.2 The question P0 must answer

Even a fully reused PR still costs one runner start for the gate. The MVP is worth continuing only if, on real repos:

```text
(time of CI checks normally run) − (gate job time with everything reused) > 0, clearly
```

We also need to know the **real reuse rate** (§12). If both numbers are good, continue. If not, fix the overhead before adding features.

### 0.3 The CI pain this solves (research, 2026-10-30)

What developers report, and what each means for CInema. Most published numbers come from vendors; the direction is consistent, the exact figures aren't to be relied on.

| Pain | What it means here |
|---|---|
| **Push, wait, fail, repeat.** The slowest loop in development is the one through CI: a typo found after a 15-minute wait, workflows debugged over 20 commits. | Local runs are the feedback loop; CI stays the gate. A local failure needs to be as easy to dig into as a local test: `cinema debug <task>` opens a shell in the task's container at the failing step (§9.2). |
| **Flaky tests.** Most developers hit them monthly or more; reruns that pass without a code change are routine. | Reuse must never launder a flake (signed failures, `FAILED_ELSEWHERE`, §11.2). CInema also has what CI throws away: every run's outcome on the same inputs, on every developer's machine. Same `action_key`, different outcomes = a flaky task, reported by name in `cinema status` and the PR summary. |
| **Cost, and its volatility.** GitHub cut hosted runner prices by up to ~40% on 2026-01-01 and announced, then postponed, a per-minute fee on self-hosted runners. | Savings are priced from rates in one updatable table, never hard-coded, and the summary shows minutes as well as dollars. A per-minute platform fee, if it comes back, makes every skipped job worth more, self-hosted ones too. |
| **AI agents multiply PRs.** Several agent sessions per developer, each opening PRs, compete for one runner pool; agent PRs fail CI more often. | Agents are first-class users: they run `cinema push` / `cinema status --json` before a PR exists, so broken work never reaches CI. Many sessions on one machine share one scheduler (§9.4: machine-wide slots, not per-invocation `-j`). Agents that live in remote sandboxes are their own "developer machine" and need their own short-lived, scoped keys (§7.3). |
| **Supply-chain attacks through CI.** The 2025 `tj-actions/changed-files` compromise repointed tags and leaked secrets from thousands of repos. | Running CI locally runs third-party code (actions, install scripts) on the developer's machine. The container is the boundary: no host credentials, SSH agent, cloud config or env inside it unless a task explicitly declares them, and no host Docker socket without `docker: host`. `cinema analyze` already inlines only commit-pinned actions; it should also flag unpinned ones. |
| **Merge queues run CI twice.** Once on the PR, again on the queued merge (`merge_group`). | A queue entry whose tree equals a PR's merge-result tree (single PR, base unchanged) matches the PR's receipts: skip it too (§5.5). |
| **Monorepos rerun everything.** Teams use Nx, Turborepo or Bazel to run only affected targets. | Whole-source fingerprints rerun every task on any change. Narrower inputs stay deferred (`vision §11.5`), but shadow mode (§11.5) is now the way to prove them safe per task before trusting them. CInema must not fight a repo's own build cache. |
| **Parity tools fall short.** `act` emulates actions imperfectly; Dagger means rewriting CI. | The gap CInema fills: no rewrite, honest about what it can't reproduce (review / stays in CI), and shadow mode as proof rather than a promise. |

Sources: [GitHub: 2026 Actions pricing changes](https://resources.github.com/actions/2026-pricing-changes-for-github-actions/), [devclass on the self-hosted fee](https://devclass.com/2025/12/17/github-to-charge-for-self-hosted-runners-from-march-2026/), [Semgrep on tj-actions](https://semgrep.dev/blog/2025/popular-github-action-tj-actionschanged-files-is-compromised), [flaky-test developer survey (arXiv)](https://ar5iv.labs.arxiv.org/html/2203.00483), [Tenki on agent CI load](https://tenki.cloud/blog/hidden-ci-tax-ai-coding-agents), [LinearB 2026 benchmarks](https://linearb.io/resources/ai-engineering-productivity-gap), [Mergify on monorepo CI](https://mergify.com/learn/monorepo-ci), [debugging Actions over 20+ commits](https://dev.to/s1hofmann/debugging-github-actions-19a5).

---

## 1. The invariant

```text
GATE_PASS(candidate, policy) =
    policy is valid and loaded from a trusted source
    AND the candidate identity is the one CI is actually testing
    AND for every required task:
        an accepted reusable receipt exists
        OR a fresh execution in this CI run passed
```

- Missing, malformed, unsigned, untrusted, stale, partial, or ambiguous evidence **never** satisfies a task. It causes the task to run.
- **Unknown means run, never approve.**
- The required-task list is enumerated **before** looking at evidence. No task can disappear because a parser failed, JSON was empty, or a step was skipped.
- A signature identifies *who claimed* a result. It does not prove the work happened. Personal mode accepts that, explicitly.

---

## 2. Non-goals for the MVP

The MVP does not include:
- running actions as GitHub does (`act`-style): actions are translated into commands, or the job stays in CI
- a general CI interpreter
- dependency discovery for arbitrary programs
- selective test inference or narrowed input sets
- shell shims or global command interception
- filesystem watchers or a resident daemon
- PTY/interactive mode
- MCP, a skill pack, or AI features
- GitLab (next phase)
- Windows and macOS CI jobs (Windows and macOS *hosts* are supported; their jobs stay in CI)
- team server, SSO, managed devices, trusted executors, hardware attestation
- artifact promotion
- dollar-savings estimates beyond pricing a job from its own billed history (§11.6)

It must never:
- claim macOS ARM ≡ Linux AMD64, or count an arm64 run for an amd64 job without an explicit `arch: any`
- skip a CI job whose stand-in hasn't agreed with CI (§11.5)
- claim a green outer command proves every nested test ran
- upload source code
- collect shell history
- rewrite workflows or branch protection without showing the change first

All of these exist in `vision.md`. Each comes back only when a real need shows up.

---

## 3. Architecture

```text
cinema setup                               once: draft tasks from .github/workflows, edit them to skip in place (§11.0)

developer / agent
   └─ cinema push                           (or: cinema run <task> --on <host>)
         ├─ plan: which tasks lack evidence for HEAD and for the merge result (§5.5)
         ├─ pick hosts: local by default; remote if the developer chose it (§9.3)
         ├─ each task: clean checkout of the commit, inside a Linux container
         │     ├─ supervise, keep exit code, enforce runner-sized CPU/memory
         │     └─ fingerprint source + task + environment (image, arch, emulated)
         ├─ record → SQLite + blob store (per-user data dir)
         ├─ sign receipts with this machine's key (Ed25519, DSSE)
         └─ git push branch + refs/notes/cinema/v1/<key-fp>

GitHub Actions
   ├─ "cinema" job per workflow: verify receipts against trusted keys, the task
   │     contracts on the base branch, and the job each task covers
   ├─ covered jobs (and matrix legs) whose tasks were reused skip themselves
   └─ everything else runs as before; shadow-mode jobs run and are compared (§11.5)

cinema dashboard → 127.0.0.1 only; reads the same SQLite through the same Go services
```

### 3.1 Stack

| Layer | Choice |
|---|---|
| CLI | Cobra (or stdlib `flag` if it stays small) |
| HTTP | `net/http` stdlib; `http.ServeMux` with method patterns |
| DB | SQLite, cgo-free driver (evaluate `modernc.org/sqlite`), WAL mode |
| Hashing | SHA-256, domain-separated, length-prefixed encoding |
| Signing | stdlib `crypto/ed25519` + DSSE envelope (small enough to implement directly; test vectors from the spec) |
| Dashboard | `html/template` + `go:embed` + one small vanilla JS file for live refresh |
| Live updates | SSE (`text/event-stream`) |

### 3.2 Packages (create when needed, not upfront)

```text
cmd/cinema/
internal/
  record/     process supervision, output capture, exit semantics
  snapshot/   source manifest + fingerprint
  task/       task contract parsing, exact matching, action keys
  store/      SQLite + content-addressed blobs
  evidence/   receipt build, DSSE sign/verify, strict parsing
  plan/       REUSE / RUN / BLOCK decisions + reason codes
  transport/  git notes publish/fetch
  engine/     container engines (docker-compatible, Apple container) behind one small interface
  host/       where a task runs: local, ssh, provider plugins (§9.3)
  github/     event → candidate, gate mode
  dashboard/  embedded templates, local auth, SSE
testdata/
  repos/      fixture repositories
  attacks/    malformed / forged receipts
```

Dependency rule: `plan` and `evidence` import nothing from `dashboard`, `github`, or the CLI. Verification stays a small, pure package.

Type rule: a verified signature produces a `VerifiedStatement`, never an `AcceptedCheck`. Only `plan` turns statements into decisions. This makes "signature valid = check passed" a compile-time mistake rather than a runtime one.

---

## 4. Command recording

### 4.1 `cinema exec`

```bash
cinema exec -- go test ./...
cinema exec -- npm run lint
cinema exec -- bash -o pipefail -c 'go test ./... | tee out.txt'   # shell only when explicit
```

- Launch argv directly with `os/exec`: no shell, no string splitting, no alias expansion.
- Preserve cwd, environment, stdin, stdout, stderr, and **the child's exit code**. CInema never turns a failure into 0.
- Recording failure is reported on stderr and produces **no reusable receipt**. The child's exit code is still returned.
- Signals: forward SIGINT/SIGTERM to the process group. On timeout or cancel, terminate the group, wait a grace period, then kill.
- Output: tee to the terminal unchanged, plus a bounded capture to a blob (default 10 MiB per stream). Past the limit, keep the first 20% and the last 80% with an explicit `N bytes omitted` marker, so failure summaries (printed last) always survive. Raise the limit once capture streams to disk.
- Machine-readable metadata never goes to the child's stdout. Use `--record-json PATH`.
- Pipe mode only in the MVP. PTY mode is deferred (`vision §10.2`).

### 4.2 `cinema run <task-id>`

Runs a task defined in `.cinema/tasks.yaml` with its exact argv. This is the preferred way to produce reusable evidence, because there's no ambiguity about which task the run was. `cinema exec` runs are matched to tasks only by **exact argv + cwd** equality with a task contract.

### 4.3 What a run records

- run ID; exact argv; resolved executable path + digest; cwd relative to repo root
- source fingerprint before and after (§5); any mutation detected
- environment fingerprint (§6)
- start/end wall time; monotonic duration; exit code or signal
- output blob digests; truncation flags
- `result_origin: fresh | tool_cache | unknown` (§4.5)
- matched task ID, if any
- per-test results parsed from the output and JUnit files the run wrote (§9.7): counts and failed tests with the run, the full list as a blob with the logs. Advisory, never evidence

Secrets: record only an allowlist of environment variable names and values declared in the task contract. Never dump the full environment. Never hash low-entropy secrets.

### 4.4 Mutating commands

Formatters, `--fix` linters, and code generators can be recorded, but they never certify the tree they modified. If the source fingerprint after the run differs from before, the run is **ineligible** (`SOURCE_CHANGED_DURING_RUN`). In a clean checkout (§5.2) the tree starts as the commit, so files a run only *adds* (coverage temp files, a downloaded tool) are build junk, as on a CI runner, and don't count; modifying or deleting a tracked file does. Workflow: `cinema exec -- npm run lint:fix`, then `cinema run frontend-lint`.

### 4.5 Tool caches

`go test` may replay cached results. Task contracts declare `accept_tool_cache: true|false`. If `false`, the task argv must disable the cache (e.g. `-count=1`), and the matcher rejects runs that don't. Never relabel an unknown cached result as fresh.

Two layers enforce it. Config load rejects a bare `go test` argv without a cache-disabling flag. At runtime, CInema watches every stdout byte (independent of the bounded capture) for go test's `(cached)` marker. That catches wrappers like `env … go test`, `bash -c` and `make`. A replayed run is recorded with `result_origin: tool_cache`, and `TOOL_CACHE_REJECTED` disqualifies it.

### 4.6 Declared outputs

`outputs: [cover-profile.out, coverage]` lists files or directories a task writes into the tree that aren't gitignored. They're left out of that task's fingerprint and mutation check. Like `platform: any`, this is an explicit, reviewable claim: a path a task both reads and writes must not be declared. Outputs may not overlap `extra_inputs`.

---

## 5. Source identity

### 5.1 Whole-source fingerprint (the only strategy in the MVP)

`source_digest` = SHA-256 over a sorted, deterministic manifest of:
- every tracked file: path, mode (exec bit), symlink target or content digest
- every untracked, non-ignored file (it may affect the build)
- declared extra inputs from the task contract (e.g. an ignored `.env.test`)
- submodule commit IDs

Excluded: `.git/`, ignored files not explicitly declared, and `.cinema/tasks.yaml` (its task-relevant content is in each contract digest; see `docs/adr/0001`). `.gitignore` is not proof that an ignored file doesn't matter, so tasks that read ignored files must declare them. Otherwise results can be wrong, and the docs must say so plainly.

Narrower per-task inputs are deliberately deferred (`vision §11.5, §12.5`). A smaller fingerprint gets more hits but risks false greens.

### 5.2 Workspace-observed vs clean checkout

- **Workspace-observed** (default for `exec`): runs in the dev's checkout. Fingerprint before and after; any difference = ineligible. A file changed and restored mid-run can't be detected, so this mode assumes an honest workstation. Personal mode accepts that.
- **CI evidence comes from clean checkouts only** (ADR 0002, from a lab false green). Working-tree runs drive local `status`; `cinema push` signs clean-checkout runs, and the gate rejects others with `WORKSPACE_EVIDENCE`. Exception: tasks that opt in with `reuse: { capture: workspace }`. `cinema push --run` runs whatever lacks clean evidence for HEAD in a clean checkout first.
- **Clean checkout** (`cinema run --commit <sha>`): `git clone --shared --no-checkout` into a temp dir, then a detached checkout of the commit. The real repo is only read: no worktree entry, no index or ref changes. Not `git archive`, because `export-ignore`/`export-subst` attributes would change the bytes. This is the stronger mode, and the only one that produces CI evidence.
- **In a container, the checkout happens inside the container's filesystem**, from the commit's git objects (a bundle sent in), with `core.autocrlf=false` and symlinks enabled. Never a bind mount of the host's tree: on macOS and Windows that goes through the VM's file sharing (slow, and it can change line endings, modes and symlinks). The source digest is computed from the bytes the container actually checked out and must equal the digest computed from the commit's objects on the host. A mismatch fails the run.

### 5.3 Before-commit reuse

Developers test a dirty tree, then commit exactly those bytes. Since the fingerprint covers content, not commit IDs, a run on a dirty tree matches the later commit if the bytes are identical. `cinema push` recomputes the fingerprint for each pushed commit (from `git ls-tree`/blob hashes, no checkout needed) and pairs it with matching runs. The original run record is never rewritten.

Tasks that read git metadata (commit SHA, tags, `git describe`) must declare `vcs_context: exact`. Those only reuse when the commit SHA itself matches.

### 5.4 Performance

- Keep a local cache: `(path, size, mtime, inode) → digest`.
- Always re-hash anything that changed before signing.
- Measure hashing separately from wrapper overhead.
- If full-source hashing is too slow on big repos, that's a P0/P1 measurement finding, not a reason to quietly narrow inputs.

### 5.5 Testing what CI will test: the merge result

On `pull_request`, GitHub tests the merge of the PR into its base, not the PR head (§11.2). A receipt for the head only matches if the base hasn't changed anything, which caps reuse (P0-5: 30–95% of real PRs). So `cinema push`, for a branch with an open PR or a known base, also runs on the merge result. It computes the tree GitHub will build with `git merge-tree --write-tree origin/<base> HEAD`, and runs the tasks on that tree. A clean merge's tree is fully determined by the base and the head, so its fingerprint equals the fingerprint of GitHub's merge commit, and the receipt matches.

- No local merge commit is created; nothing in the repo changes. The tree is checked out like any commit (§5.2).
- If the merge has conflicts, GitHub runs no `pull_request` CI either: nothing to do.
- **Merge queues.** A `merge_group` run tests the queued merge. When the queue entry is a single PR and the base hasn't moved, its tree is the PR's merge-result tree, so the gate accepts the PR's receipts for it (`merge_group` is an event tasks can be drafted for, §11.2). Batched entries combine several PRs: their trees are new, and they run.
- If the base moves before CI runs, the receipt no longer matches and the jobs run (correct). `cinema push` reports how far behind the base it tested.
- Head-only receipts are still made. They cover `push` events and PRs whose base hasn't moved.
- A merge-result tree has no commit to attach a note to, so its receipts go in the **PR head's** note, marked `source.tree: merge-result`. The gate already reads the head's notes; the fingerprint decides whether they match.
- After the PR merges, the commit on the default branch (merge, squash or rebase) has no notes of its own. For `push` events the gate also reads the notes of the PR the commit came from (looked up through the API). Squash and merge-button commits have exactly the merge-result tree, so those receipts match and push-to-main CI skips too.
---

## 6. Tasks and matching

### 6.1 Task contract (`.cinema/tasks.yaml`)

```yaml
schema_version: 1
tasks:
  backend-unit:
    argv: [go, test, -count=1, ./...]
    cwd: .
    platform: linux/amd64          # GOOS/GOARCH of the machine that must run it
    toolchain:
      - cmd: [go, version]          # output is part of the environment fingerprint
    env: [CGO_ENABLED]              # allowlisted env vars that are inputs
    extra_inputs: []                # ignored files the task reads
    vcs_context: none               # none | exact
    accept_tool_cache: false
    reuse:
      allowed: true
      max_age: 72h

  frontend-lint:
    argv: [npm, run, lint]
    cwd: web
    platform: any                   # explicit claim: lint result is platform-independent
    toolchain:
      - cmd: [node, --version]
      - file: web/package-lock.json
    reuse: { allowed: true, max_age: 72h }

  integration-db:
    argv: [make, integration-test]
    cwd: .
    platform: linux/amd64
    reuse: { allowed: false }       # always runs in CI
```

Fields added from the lab (all optional):

```yaml
  integration:
    argv: [bash, -o, pipefail, -c, "make binary && go test -count=1 ./integration"]
    platform: linux/amd64
    runtime: { go: 1.26.10, node: 24.22.1 }   # provisioned identically locally and in the gate (§6.4)
    toolchain:
      - cmd: [go, version]                    # allowlist: [tool, --version] | [go, version] | [java, -version]
      - file: web/package-lock.json           # regular file, no symlinks, ≤ 64 MiB
      - image: quay.io/org/builder:1.27-base  # fingerprinted by resolved digest
    outputs: [cover-profile.out, dist]        # written by the task, unignored: left out of its fingerprint
    exclusive: true                           # never alongside other tasks under -j (fixed ports, networks)
    cpus: 4                                   # Linux: see only 4 CPUs (affinity), like the runner it replaces; scheduling only
    container: golang:1.27                    # run inside this image as root, like a GitHub container job; its digest is fingerprinted
    services:                                 # sidecars, like a GitHub job's services: (localhost ports, or by name from container:)
      postgres: { image: postgres:16, env: { POSTGRES_PASSWORD: pw }, ports: ["5432:5432"], options: "--health-cmd pg_isready" }
    timeout: 30m                              # stop a hang; a timeout is a failure (not in the contract)
    accept_tool_cache: false
    reuse: { allowed: true, max_age: 24h, capture: clean }   # capture: clean (default) | workspace (ADR 0002)
```

- `platform: any` is an explicit, reviewable claim by the repo owner. Never infer it.

Fields for container execution (§9), all optional:

```yaml
  unit:
    image: ghcr.io/billiondollarsolo/cinema-runner:ubuntu-24.04@sha256:…   # the runner-like image; default: cinema's pinned image for the job's runs-on
    arch: any                 # explicit claim: the result doesn't depend on the CPU (lint, format, pure-JS tests). Default: the covered job's runner arch
    memory: 16g               # memory limit, like the runner it replaces
    docker: host              # explicit claim: the job may use the host engine's socket (docker build, Testcontainers)
```

- `arch: any` lets an arm64 Mac run an amd64 job natively, at full speed, for checks that don't depend on the CPU. Like `platform: any`, it's the repo owner's claim, part of the contract, and shown in the PR summary (§11.6).
- `image` is the base the job runs in when it has no `container:` of its own. Its digest is part of the environment fingerprint.
- **Runner size is an input, not scheduling.** For container tasks, CPUs, memory and `/dev/shm` are in the environment digest (unlike host-process `cpus:`, which stays scheduling only). `cinema setup` writes them from GitHub's runner table for the label and the repo's visibility (public and private runners differ). The gate derives the same from the label and `repository.private` in the event, so a repo going private makes old receipts miss instead of OOM in CI. Unknown labels mean RUN.
- `cinema setup` writes every value the gate compares (image digests, runtimes, runner size, `event`, `cinema_version`) into the task. `cinema doctor` and `cinema push` warn when the installed cinema differs from `cinema_version`.
- For `npm run <x>`, the task digest includes the resolved script text from `package.json`. Changing the script invalidates old runs.

### 6.2 Action key

```text
action_key = SHA256("cinema.action.v1" || len-prefixed(
    task_contract_digest,
    source_digest,
    environment_digest,        # platform, toolchain outputs, allowlisted env
    vcs_digest_or_empty
))
```

Acceptance rules (max age, trusted keys) are **not** part of the key. A stricter policy rejects old evidence without changing what the work was.

### 6.3 Exactness

Exact argv only. `go test -run TestLogin ./...` does not satisfy `go test ./...`. Different tags, `-race`, filters, or package sets are different work. Unknown commands are kept as history and never count toward a task.

### 6.4 Writing tasks that reuse (lab rules)

Each rule comes from a real project in `spikes/lab/RESULTS.md`.

1. **Make the task what CI runs, artifact included.** A `go install` build of golangci-lint v2.14.0 and the official v2.14.0 release disagree. CI's action runs the release, so the task must download that release, pinned, and verify its checksum in the argv.
2. **Pin runtimes with `runtime: {go: 1.26.10, node: 22.23.3}`.** CInema provisions exactly that version for the task and its probes, identically wherever the task runs (and in the gate, for the older `--execute` form). Go uses `GOTOOLCHAIN` (sumdb-verified); Node uses the official tarball checked against `SHASUMS256.txt`, cached in the data dir. Environments then match by construction instead of depending on which patch each machine has installed. Without it, `go version` and `node --version` are fingerprinted to the patch, and a `check-latest` or `lts/*` drift is `ENVIRONMENT_MISMATCH`. Runtime matrices (Node 20/22/24/26) become one task per version. `cinema exec` can't provision, so it never makes evidence for a runtime-pinned task.
3. **Probe only what isn't pinned in source.** pnpm and yarn versions come from `packageManager`, already covered by the source fingerprint. Probing them, or a tool whose version differs between dev box and runner image (git), only causes misses.
4. **Isolate tool caches per tool build.** Caches outside the repo are inputs CInema can't see (golangci-lint replayed another build's results). Point them at a per-version directory.
5. **Include setup in the task.** CI evidence comes from a clean checkout (ADR 0002): no `node_modules`, no `dist/`. `yarn install --immutable && …` belongs in the argv, and warm local caches keep it fast.
6. **Declare outputs a task writes into the tree** if they aren't ignored (`cover-profile.out`, a build dir that deletes tracked files).
7. **Keep never-reusable work in its original parallel jobs** (time-boxed fuzzing, live-network checks). Inside the single gate job it only adds serial time.
8. **Run evidence as a non-root user** when CI is non-root (GitHub runners). Root changes test results.
9. **Make `.cinema/tasks.yaml` follow the repo's formatter,** or list it in the formatter's ignore file. Repo-wide formatters and linters will touch it.


### 6.5 Languages: runtimes CInema provisions

A job's language toolchain must come from a pin, provisioned identically wherever the task runs, from the official distribution, checksum-verified, never from what a machine or image happens to have (P4-1: a system `go` masked a bug). Each language is one provider: resolve the version from the workflow's setup action or the repo's version file, download for the target OS and arch, verify, install atomically into `<data dir>/runtimes/<lang>/<version>-<os>-<arch>`, and expose its `bin` on PATH (mounted read-only into containers). Version files are read the way the setup action reads them.

| Language | Setup action / version file | Official distribution, verification | Status |
|---|---|---|---|
| Go | `actions/setup-go`, `go.mod`/`go.work`, `.go-version` | go.dev tarball, SHA-256 from go.dev's release index | done (via `GOTOOLCHAIN`; tarball in P4) |
| Node.js | `actions/setup-node`, `.nvmrc`, `.node-version`, `package.json` `engines`/`volta` | nodejs.org tarball, `SHASUMS256.txt` | done |
| pnpm / yarn / npm | `pnpm/action-setup`, `packageManager` | npm registry tarball (integrity hash) | pnpm done |
| Python | `actions/setup-python`, `.python-version`, `pyproject.toml` | python-build-standalone release, SHA-256 | P4 |
| uv / Poetry | `astral-sh/setup-uv`, `uv.lock`, `poetry.lock` | official release, SHA-256 | P4 |
| Java | `actions/setup-java` (distribution + version), `.java-version`, `.sdkmanrc` | Temurin/Zulu/Corretto archives via their APIs, SHA-256 | P4 |
| Maven / Gradle | wrapper scripts in the repo (`mvnw`, `gradlew`): already pinned in source | wrapper's own checksum (`distributionSha256Sum`) | P4 (caches only) |
| Rust | `dtolnay/rust-toolchain`, `actions-rust-lang/setup-rust-toolchain`, `rust-toolchain.toml` | static.rust-lang.org dist, SHA-256 | P4 |
| Bun | `oven-sh/setup-bun`, `.bun-version`, `packageManager` | GitHub release, `SHASUMS256.txt` | P4 |
| Deno | `denoland/setup-deno`, `.dvmrc` | GitHub release, SHA-256 | P4 |
| Ruby | `ruby/setup-ruby`, `.ruby-version`, `.tool-versions` | ruby/ruby-builder prebuilt release, SHA-256 | P5 |
| .NET | `actions/setup-dotnet`, `global.json` | dotnet-install archives, SHA-512 | P5 |
| PHP | `shivammathur/setup-php`, `composer.json` | (no single official binary distribution) | review until a faithful source exists |
| Elixir / Erlang | `erlef/setup-beam`, `.tool-versions` | hex.pm builds, SHA-256 | P5 |

`.tool-versions` (asdf, mise) is read for every language it names. A job whose language has no provider yet stays review with that reason, never runs against whatever is installed.

---

## 7. Receipts and keys

### 7.1 Format

An in-toto Statement wrapped in a DSSE envelope, signed with Ed25519. The predicate URI must live on a domain the project controls; choose it before freezing v1.

```json
{
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [{"name": "cinema-action:backend-unit", "digest": {"sha256": "<action_key>"}}],
  "predicateType": "https://<project-domain>/cinema/task-result/v1",
  "predicate": {
    "schema_version": 1,
    "run_id": "run_…",
    "repository_id": "github.com/<numeric-repo-id>",
    "task": {"id": "backend-unit", "contract_digest": "sha256:…"},
    "source": {"digest": "sha256:…", "observed_commit": "<sha or null>", "tree": "commit | merge-result", "capture": "clean-checkout"},
    "environment_digest": "sha256:…",
    "platform": "linux/amd64",
    "result": {"status": "passed", "exit_code": 0, "origin": "fresh"},
    "timing": {"started_at": "…", "duration_ms": 42000, "time_authority": "local-clock"},
    "event": "pull_request",
    "runner": {"label": "ubuntu-24.04", "cpus": 4, "memory_mb": 16384, "shm_mb": 2048},
    "executor": {"kind": "container | host-process", "version": "0.2.0",
                 "engine": "docker | podman | apple-container",
                 "image": "sha256:…", "arch": "linux/amd64", "emulated": true,
                 "host": "local | ssh | <provider>"}
  }
}
```

### 7.2 Rules

- Sign the exact payload bytes (DSSE PAE) and verify those same bytes. Never re-serialize before checking.
- Strict parser:
  - reject duplicate JSON keys, unknown fields, malformed digests, and wrong types
  - reject payloads over 64 KiB
  - the key ID is only a lookup hint
- Only runs that finished and were fully stored can be signed. Never sign a crashed or partial record.
- Local timestamps are self-asserted. That's fine for Personal mode `max_age`; it says nothing more.
- Passed and failed runs both get receipts (`result.status`). The gate only ever reuses passes, and a failure on the same `action_key` blocks them (§11.2).
- `executor.arch` is the architecture the code ran as, `emulated` whether that was through translation (Rosetta, QEMU). The gate matches the job's runner arch against `arch`, never against the host's.
- Receipts go into public repos' notes, so they name the kind of host (`local`, `ssh`, a provider), never a hostname, tailnet name or username.

### 7.3 Keys

- `cinema keys init` generates an Ed25519 key, stored in the OS keychain or an encrypted file with 0600 permissions (with a warning).
- One key per developer machine: laptop, desktop, a VPS. Each gets its own fingerprint, so per-machine trust and revocation are trivial. Stored in the macOS keychain, Windows Credential Manager, or libsecret on Linux; file fallback with a warning.
- **Agents in remote sandboxes.** A coding agent that works inside a remote sandbox has no developer machine behind it. It gets its own key, short-lived (default 7 days) and listed in the trusted keys with an expiry and a name, so its receipts are distinguishable and expire with it. `cinema keys init --expires 7d --label agent:<name>`.
- **Remote hosts never hold a key** (§9.3). The developer's machine sends the checkout, collects the result, and signs; the receipt names the host. A remote machine is trusted exactly as much as the developer who chose it.
- `cinema keys export` prints only the public key.
- **Trusted public keys live in a GitHub repository variable** (`vars.CINEMA_TRUSTED_KEYS`), **not in the repo tree.** A PR can edit files in the tree. It can't edit repo variables.
- Revocation: remove the key from the variable. The next gate run stops accepting its receipts. Offboarding a developer is removing their machines' keys.

---

## 8. Transport: git notes

```text
refs/notes/cinema/v1/<key-fingerprint>
```

- One notes ref per signing key, so machines never fight over the same ref.
- Each note is attached to the commit it covers. Its content is a compact JSON array of DSSE envelopes for that commit. Caps: 1 MiB per note, 500 receipts per candidate. A head note holds head and merge-result receipts for every matrix leg (traefik's build alone is 19 legs × 2). `cinema push` replaces superseded receipts for the same task rather than appending.
- No logs in notes, ever.

### 8.1 `cinema push`

1. Resolve the commits being pushed (`git rev-list <remote>..<local>`).
2. For each, compute the source fingerprint and find matching passed runs.
3. Sign receipts and write notes.
4. `git push --atomic <remote> <branch> refs/notes/cinema/v1/<fp>`, falling back to notes-first-then-branch if the remote rejects `--atomic`.
5. Report branch push and evidence push **separately**. Evidence failing never blocks the source push.

A `pre-push` hook can't add refs to an in-flight push, so `cinema push` is the supported path. An optional `pre-push` hook warns when a plain `git push` would go out without evidence for HEAD (and how many CI minutes that costs), and lets it through.

**Not waiting on it.** Local CI can take a while, longer under emulation. Notes that land after CI has started don't help: skipped jobs have already been decided. So:
- `cinema push` (default, decided) runs, then pushes branch and notes together, showing the plan, progress and an estimate up front.
- `cinema push --background` returns immediately; a detached one-shot process runs the tasks and pushes branch and notes together when done, then exits. Its status shows in `cinema status` and the dashboard. (One process per push, not a daemon.)
- `cinema push --now` pushes the branch immediately and accepts that CI runs everything.

### 8.2 Forks

Fork contributors can't push notes to upstream, and their keys aren't trusted anyway. **Default: fork PRs get full CI.** Later (P5): a maintainer can run a fork PR's jobs on their own machine after reviewing it (in containers, which also isolate untrusted code), and push notes for that head commit from their trusted key. The fork PR then skips those jobs. Until that's designed and reviewed, `FORK_PR` stands. This is the correct behavior for open-source repos, not a limitation to work around. Never use `pull_request_target` to read fork evidence.

---

## 9. Where tasks run: local first, remote when you choose

CI jobs run on GitHub's Linux VMs. CInema runs the same jobs **in Linux containers**, by default on the developer's own machine, before the push. No other hardware is needed. When one machine isn't enough, the developer can send work to a remote machine. The task, its evidence and its rules don't change with where it runs.

### 9.1 Engines

One small interface (`internal/engine`): pull an image by digest, create a container with CPU, memory and arch limits, copy a checkout in, exec with streamed output, copy outputs out, start sidecars on a private network, remove everything. Two implementations:

| Engine | Hosts | Notes |
|---|---|---|
| Docker-compatible CLI | Docker Engine/Desktop, Podman, OrbStack, Colima, Rancher Desktop, nerdctl | One code path. `DOCKER_HOST` (incl. `ssh://`) also reaches remote engines. |
| Apple `container` | macOS 26+, Apple silicon | Each container is its own lightweight VM. amd64 images run under Rosetta (`--arch amd64`). DNS between containers covers services. |

- **cinema's own engine client config.** cinema talks to the engine with a private, empty client config (`DOCKER_CONFIG` in its data dir), never the user's `~/.docker/config.json`. The user's config can name a keychain credential helper and registry logins; using it pops a keychain password prompt on every pull (seen in P4-1 on a Mac) and would hand those logins to the job's image pulls. Private registries a task needs go through `credentials:` (§9.2).
- **Always pass the platform.** Every pull and run names `--platform linux/<arch>`. A tag pulled once for one arch otherwise satisfies later runs for the other (P4-1: `alpine:latest` pulled for amd64 then ran as amd64 with no `--platform`).
- **Runtimes need no host tools.** A runtime pin must work in an image with nothing preinstalled. P4-1: Go provisioning went through `GOTOOLCHAIN`, which needs some `go` to bootstrap from (the VPS had one, a clean container didn't). Go is provisioned from the official tarball, checked against its SHA-256, like Node.
- **Inspect with the platform too.** On Docker's containerd image store (default from Docker 29), an image holds several platforms and `image inspect` reports the host's unless given `--platform`. Digests are resolved per platform. And the legacy builder can't build for another arch (P4-1: an `:amd64` tag held an arm64 image): runner images are published multi-arch, never built on the developer's machine.
- Selection: `cinema doctor` lists the engines it finds and what each can do (arches, emulation, services). The choice goes in the user config (§9.5), not the repo. Never guessed silently between runs: switching engines changes `executor.engine` in receipts.
- What runs where:

| Host machine | amd64 jobs | arm64 jobs (`ubuntu-*-arm`) |
|---|---|---|
| Linux / Windows (x86-64) | native | emulated (QEMU), or stays in CI |
| macOS (Apple silicon), Linux / Windows on ARM | emulated (Rosetta) | native |

- **Emulation is an option, off by default** (`emulate: [amd64]` in the user config). P4-1 on an M-series Mac, Colima with Rosetta, cold caches: cli lint 3.8×, cli build 2.9×, govulncheck 3.0×, traefik cross-build 3.1×, eslint type tests 1.6× slower than arm64 native; warm caches close most of the gap. Without it, an arm64 host leaves amd64 jobs to CI or to an amd64 host. Emulation is slower and changes CPU features (Rosetta has no AVX-512, and AVX/AVX2 only on recent macOS; verify in P4-1). Code that picks a SIMD path at runtime takes a different path than on the runner. Receipts record `emulated: true`; a task can require `emulation: forbid`, and then it runs natively or stays in CI.

### 9.2 Fidelity: the container is sized and shaped like the runner

A local pass must mean CI would pass. So the container imitates the job's runner, not the developer's machine:

- **Image.** A pinned runner-like image per `runs-on` label (`ubuntu-24.04`, `ubuntu-22.04`, …): the OS release, a non-root `runner` user with passwordless sudo, as on GitHub, and only the tools jobs need. Decided: slim, not GitHub's ~60 GB image. The analyzer and `cinema doctor` find the tools each job calls, and the task's image layer adds them (pinned, part of the digest). A tool it misses is a failed local run, never a false green. "Slim" still means the base set of GitHub's image that tests call without any workflow mentioning it: P4-1 found cli's Go tests exec `ssh` and `ssh-keygen`, which no workflow scan can see. The base set (OpenSSH client, git, curl, wget, jq, zip/unzip, xz, make, gcc/g++, python3, sudo, …) is chosen from GitHub's image manifest; `cinema doctor` reads a failed run's output (`exec: "ssh": executable file not found`) and proposes the missing package. Its digest is in every fingerprint. `container:` jobs use their own image, as now.
- **Resources.** CPUs and memory match the runner size (`ubuntu-latest`: 4 vCPU / 16 GB on public repos, 2 / 7 GB on private ones; larger runners by label). Tests that size worker pools from the core count, or that would run out of memory in CI, behave as in CI.
- **User, paths, env.** Non-root `runner` user, `HOME=/home/runner` fresh per run, and the checkout at the runner's path, `/home/runner/work/<repo>/<repo>`: tests that embed absolute paths (snapshots, coverage, error messages) see the same ones. `CI=true`, `TZ=UTC`, `LANG=C.UTF-8`, and `GITHUB_*` variables only where the job reads them (from the analyzer).
- **Limits.** `ulimit -n` and process limits as on runners; `/dev/shm` sized like the runner's (Docker's 64 MB default crashes headless browsers); no swap beyond the runner's.
- **sudo.** Passwordless, as on runners. A job that runs `sudo apt-get install` (lab: prometheus, prettier) changes only its own throwaway container, so it no longer has to be review-only or `exclusive`. Package downloads float exactly as they do in CI.
- **Docker inside a job.** Many jobs call `docker build`, `docker compose` or Testcontainers, and runners have a Docker daemon. Inside a container that needs an engine of its own. Decided: the host engine's socket, opt-in per task with `docker: host`, an explicit claim because the job can then control the host's engine (the analyzer drafts it, marked, and `cinema setup` lists it). The job's containers are labeled with the run and removed with it. Not available with Apple `container`; those tasks need a Docker-compatible engine or stay in CI.
- **Network.** Internet as on runners; services on a private network, reachable by name and on localhost ports as in Actions.
- **Time.** Job `timeout-minutes` applies, scaled for emulation (§17).
- **Credentials stay out.** Nothing from the host enters the container by default: no host env, `~/.ssh`, SSH agent, `~/.aws`/`~/.config/gcloud`, `~/.npmrc` or Docker credentials. A task that needs one (a private package registry) declares it (`credentials: [npm]`), the developer's own credential is passed for that run only, and the job is marked as depending on it. Receipts never contain credentials.
- **Debugging.** `cinema debug <task>` reruns the task and leaves a shell in its container at the failing step, with the same env and files. Nothing from a debug session is signable.
- **Caches.** Download and build caches are named volumes per repo, tool **and tool version** (`cinema-cache-<repo>-go-1.26.10-build`, `…-golangci-2.14.0`): a cache shared across versions is how golangci-lint replayed another build's results (lab rule 4). Package stores that are content-addressed (Go modules, npm, pnpm) can be shared per repo. Each volume has a size cap (default 10 GiB) and is trimmed oldest-first. `cinema doctor` shows the engine's disk use; Docker Desktop's VM disk filling up is a real failure mode. Results are never cached; the checkout is always fresh. Caches are a trust boundary: they're shared across runs of one repo, so a run of hostile code could poison a later one. Runs of code the developer didn't write (maintainer-run fork PRs, §8.2) get fresh caches. Values of allowlisted env inputs are passed by name, never in an argv or a run record.

### 9.3 Remote hosts: an option, behind one interface

```text
cinema run --on local                 (default)
cinema run --on ssh:buildbox          a machine you control, with an engine
cinema run --on boat:large -j 16      an on-demand machine, billed by the second
cinema push --on auto                 ask, using the policy in §9.5
```

A host is "something that gives us an engine". Three kinds:

- **local**: the engine on this machine.
- **ssh**: any machine with an engine, reached over SSH (`DOCKER_HOST=ssh://…` for Docker-compatible engines). A VPS, a desktop at home, a beefy office box.
- **provider plugins**: on-demand machines billed by the second (sandbox and microVM providers such as boat or Lizard, cloud VMs). A plugin is an executable, `cinema-host-<name>`, that speaks a small JSON protocol on stdin/stdout:

```text
capabilities  → {arches: [...], sizes: [...], engine: "docker"|"none", snapshot: bool, price_per_s: {...}}
acquire       {size, arch, ttl}            → {id, ssh: {...}} | {id, exec: true}
exec          {id, argv, stdin, env}       → streamed output, exit code      (when no ssh/engine)
put / get     {id, path, bytes}            → copy the checkout in, outputs out
snapshot      {id, name}                   → reuse warm caches next time (optional)
release       {id}                         → always called, also on Ctrl-C and crash recovery
```

  If `acquire` returns SSH with an engine, everything else goes through the ssh host path. `exec` covers providers that only run commands (each sandbox is then one task's container).

Rules that make remote safe and honest:

- **The developer's machine signs.** Remote hosts hold no keys (§7.3). The checkout is sent as a git bundle of the exact commit; the remote reports its source digest; the local machine checks it against the digest it computed from the same objects. Logs and exit code stream back and are recorded locally like any run.
- **Released, always.** Every acquired machine is released when the run ends, fails or is interrupted. Leases are recorded in the store before use, so a crashed `cinema` releases them on its next start, and `cinema hosts --cleanup` releases anything left. Each lease has a TTL at the provider as a backstop.
- **Spend is visible and capped.** Before acquiring, `cinema` shows the estimated time and cost (from past durations and the plugin's prices) and asks, unless the policy allows it. A per-push and per-month cap stops it.
- **Credentials never enter the repo.** Provider tokens live in the OS keychain, referenced from the user config.
- **Warm caches on cold machines.** Ephemeral machines start cold. Plugins that support snapshots keep a warm cache image per repo; others restore cache volumes from an artifact the developer's machine uploads. Without either, remote is slower than it looks: the estimate says so.

### 9.4 Fan-out

Local runs share one machine-wide scheduler: several `cinema` invocations (a developer plus a few coding-agent sessions, each in its own worktree) take slots from the same pool instead of each starting `-j` containers, so the machine isn't oversubscribed and runner-sized limits stay honest.

A push with many tasks (a 19-leg build matrix, 12 test shards) can spread over several hosts: local takes what fits, the rest goes to the chosen remote, up to `-j` machines. Rebuilt artifacts (§9.6) are built once per host and shared there.

### 9.5 Who decides: the developer, with a policy

User config (`~/.config/cinema/config.yaml`, never in the repo):

```yaml
engine: apple-container          # or docker, podman, …
hosts:
  buildbox: {ssh: me@buildbox.tailnet.ts.net}
  boat:     {plugin: boat, token: keychain:cinema-boat}
policy:
  default: local
  offload_when: {estimated_local: 10m}   # ask about remote when a push would take longer locally
  max_spend: {per_push: $0.50, per_month: $20}
  never_on_battery: true
```

`cinema push` prints the plan before it starts: tasks, where each would run, estimated time and cost, and what CI would otherwise bill. The developer accepts, or picks `--on`. The default is always local and never spends money without a yes.

### 9.6 Jobs the analyzer can't translate: code-defined pipelines

For a job whose actions have no command equivalent, a repo can add a stand-in: a task with plain `argv` today, and in P5 a code-defined `pipeline:` (Dagger-style: containers, mounts and steps in code, run through the same engine; whether that means real Dagger modules or cinema's own format is decided then). It's still a claim that the stand-in tests what the job tests, so:

- it must name the job it covers (`covers:`) and is bound to that job's definition (`job_digest`);
- it goes through shadow mode like any draft (§11.5) and only skips CI after agreeing with it;
- `cinema analyze` shows it as "hand-written stand-in" in the capability report.

### 9.7 Ideas taken from Dagger, and where CInema differs

Dagger (dagger.io) runs pipelines as sandboxed, cached container operations, the same locally and in CI. What CInema takes from it, and what it does that Dagger doesn't:

| Dagger | In CInema |
|---|---|
| Sandboxed functions: no host access unless passed in explicitly | Containers get nothing from the host unless a task declares it (§9.2: credentials, `docker: host`) |
| Cache volumes for package managers | Per-repo, per-tool-version cache volumes with size caps (§9.2) |
| Local cache pruned under disk pressure | Cache volumes and images trimmed when the engine's disk runs low, not only by age (§13.1) |
| Services with health checks; `dagger up` | `services:` with health checks; P5: `cinema up` to start a task's services for local development |
| Secrets as a type, from providers (env, file, 1Password, Vault, AWS, GCP), never in caches or logs | `credentials:` resolved from providers (`env:`, `file:`, `op://`, `vault://`, `aws-sm://`, `gcp://`), passed for that run only, redacted from logs, never in receipts or fingerprints (P4: env and file; P5: the rest) |
| `dagger check -i`: a shell where a command failed | `cinema run -i` / `cinema debug <task>`: a shell in the task's container at the failing step (§9.2) |
| A TUI with live progress and traces (OpenTelemetry) | Per-step timing in every run record (each workflow step is a subshell; record where time goes) and the dashboard; P5: OpenTelemetry export |
| Toolchain detection and official modules (Go, pytest, Jest, Vitest, ESLint, Prettier, Biome, ShellCheck, Helm) | `cinema analyze` reads the CI you already have instead; `cinema init` detects languages for repos without CI (§6.5 covers the languages) |
| `dagger check` for local, CI and agents | `cinema run --all`, with `cinema check` as an alias; JSON for agents (§10.3) |
| `dagger generate` + stale-code checks | A run that changes tracked files is never evidence (`SOURCE_CHANGED_DURING_RUN`) |
| Remote engines (SSH, Kubernetes, Cloud) | Hosts: local, ssh, provider plugins (§9.3) |
| Test splitting (coming) | Fan-out across hosts (§9.4); shard-aware drafts for jobs that already split |
| Cloud cache shares results across machines | **Signed receipts are that, verifiable:** CI skips work any trusted machine already proved, without a hosted service and without trusting a cache it can't check |
| **Re-checked 2026-10-30 against Dagger 1.0 beta (2026-10-22) and its changelog:** | |
| Test-native CI: each test reported on its own, rerun only the failed one | **Built (run 2), except `--failed-only`:** parse per-test results from task output (go test, pytest, jest/vitest, cargo test, JUnit XML) into the run record; `cinema run --failed-only` reruns the task (a task is the unit of evidence, so a rerun of one test is never a receipt); the same parse lets `graduate` compare *which* tests failed in a failure pair (closes §11.5.6) |
| Smart checks (upcoming): run only checks a change affects, by tracking the files each artifact reads | **Deliberately not for skipping:** narrower inputs skip more and risk false greens (§5). P5: traced reads as an *advisory* ("this change can't affect task X") shown in `cinema status`, never as evidence |
| `.dagger/lock` pins image tags, branches and HTTP fetches | Runtimes, actions' tools (golangci-lint, pnpm) and images are pinned with checksums in tasks.yaml; floating specs are re-checked in CI. Gap: `curl … \| sh` and `@latest` inside a job's own script stay unpinned (they're in the source, so a change is seen, but the remote content isn't): P5 `cinema lock` records digests of fetched URLs during a run and flags drift |
| Test splitting across engines, balanced by per-test telemetry (upcoming) | P5 with fan-out (§9.4): per-test durations from the parsed results above drive the split |
| Remote filesystems over SSH (live two-way volume) | For remote hosts (§9.3) the checkout is copied in (git bundle + digest check), never live-mounted: evidence needs a fixed tree |
| Engines: Docker, Podman, nerdctl, Finch, Apple `container` | Docker (any context: Docker Desktop, Colima, OrbStack) built; Apple `container` P4 (macOS 26); P5: Podman and nerdctl through their Docker-compatible sockets, tested before claimed |
| `dagger mcp`: functions as tools for agents | JSON envelopes on every reporting command (§10.3); P5: `cinema mcp` exposing status, explain, run and push as MCP tools |
| `dagger check --failfast` | **Built (run 2):** `cinema run --failfast` starts nothing after a failure; running tasks finish and are recorded (cancelling them would discard work as `interrupted` to save only their remaining time) |
| Cloud Checks: Dagger Cloud runs checks on every push, replacing Actions workflows; usage-based pricing | **Different by design:** CInema keeps your CI and makes it skip proven work; no hosted service, nothing to buy. Bring-your-own compute = remote hosts (§9.3) |
| Typed artifacts, branch filtering, self-orchestrating tests | Tasks are what CI's jobs do; `event:` covers branch- and event-dependent jobs; `services:` start a task's dependencies |
| SDKs (Go, TypeScript, Python, Java, PHP, Elixir, Rust, Dang) | Not needed: CInema reads the workflows you have. Language parity is about **runtimes jobs use**: Go, Node, Python, uv, Java, Gradle, Rust, Bun, Deno, .NET built (§6.5); Ruby blocked (setup-ruby builds aren't relocatable or checksummed); PHP and Erlang/Elixir P5 |
| You rewrite CI as Dagger modules | **No rewrite:** CInema reads your workflows. A repo that already uses Dagger keeps it: a step running `dagger call …`/`dagger check` translates as a task (it needs an engine: `docker: host`), so its jobs skip in CI too |

---

## 10. CLI and dashboard

### 10.1 Commands (MVP only)

```text
cinema setup [--undo]           draft tasks from the workflows and edit them to skip in place (§11.0)
cinema doctor                   engines, arches, emulation, missing tools; what this machine can take over
cinema hosts [--cleanup]        configured hosts, live leases; release anything left behind
cinema init                     write .cinema/tasks.yaml skeleton; print the workflow to add
cinema exec -- CMD ARGS...      record an arbitrary command
cinema run <task>|--all [--commit SHA] [--merge-base REF] [--on HOST] [-j N]
cinema status [--json]          per task: last run, eligible / stale / missing + reason
cinema explain <task>           why a task is or isn't reusable for HEAD
cinema push [--on HOST|auto] [--no-run] [--notes-only]
                                run what lacks evidence for HEAD and the merge result, sign, publish (+ branch)
cinema trust                    allow the toolchain commands in this exact tasks.yaml (re-run after any edit)
cinema keys init|export|list
cinema verify --event FILE      the CI gate (§11)
cinema dashboard [--port N] [--allow-host H]
cinema analyze [--yaml|--json]  propose tasks from .github/workflows (advisory, deterministic; §16 P3)
cinema gc                       apply retention
```

Deferred: `plan`/`readiness` as separate verbs (`status` covers them), `optimize`, `mcp`, `ai`, `auth`, `enroll` (`vision §19`).

### 10.2 Exit codes (everything except `exec`, which returns the child's code)

| Code | Meaning |
|---:|---|
| 0 | OK / requirement satisfied |
| 1 | Unsatisfied / task failed |
| 2 | Bad arguments or config |
| 4 | Transport unavailable |
| 8 | Internal / recording failure |

### 10.3 JSON and agents

- `--json` writes exactly one envelope to stdout; progress goes to stderr.
- Task IDs, reason codes, and `schema_version` are stable contracts.

```json
{"schema_version":1,"operation":"status","data":{"head":"<sha>","tasks":[
  {"id":"backend-unit","state":"stale","reason_code":"INPUTS_CHANGED","changed":["internal/auth/h.go"]}]}}
```

Agent integration for the MVP is a paragraph for `AGENTS.md` / `CLAUDE.md`: run checks via `cinema run`, then confirm with `cinema status --json` after the last edit. No MCP server needed.

### 10.4 Dashboard: one page

What it shows:
- **Header:** repo, HEAD, dirty or clean.
- **Tasks table:** task | last run (age, exit) | for HEAD: `reusable` / `stale` / `missing` / `failed` | reason code | platform.
- **Runs list:** newest first; click for argv, duration, fingerprint diff vs HEAD, and captured log (escaped text, ANSI stripped or safely converted).
- **Evidence:** receipts signed and pushed, per commit, per key.
- **CI:** for pushed commits, what the gate reused vs ran. Read from a small `gate-report.json` the gate uploads as a workflow artifact (P2), fetched via `gh`-style token only if the user configures one.

Never show one green percentage. The three states stay separate: **ran locally**, **reusable for this commit**, **accepted by CI**.

Security (loopback is not authentication; any website can hit localhost):
- Random per-launch token. `cinema dashboard` prints and opens `http://127.0.0.1:PORT/#t=<token>`; the JS exchanges it for an HttpOnly, SameSite=Strict cookie and scrubs the URL.
- Host must be `127.0.0.1:PORT`, `localhost:PORT`, or an `--allow-host` value. This blocks DNS rebinding.
- Mutations (only "run task") need POST, a same-origin `Origin` header, and a CSRF token.
- CSP `default-src 'self'`, no external assets.
- Logs rendered as text only. No HTML passthrough.

Live updates: an SSE endpoint emits `runs-changed`, and the page re-fetches the table. No resumable cursors in the MVP; a reconnect simply reloads.

---

## 11. GitHub Actions integration

### 11.0 Default: skip in place (`cinema setup`)

A separate gate saves nothing on its own: the project's CI still runs every job it covers, unless someone deletes them (lab finding 51). So `cinema setup` edits the project's workflows instead, after showing what changes and asking:

- Each workflow with covered jobs gets a `cinema` job that runs `cinema verify --skip-outputs <workflow>`. It judges only tasks whose `covers:` names a job in that workflow, and outputs `skip_<job>=true` when every task of the job was reused, and, for a matrix job, `exclude_<job>` with the reused legs.
- A covered job gets `needs: cinema` and `if: ${{ !cancelled() && needs.cinema.outputs.skip_<job> != 'true' && <its needs succeeded> && (<its own if>) }}`: skipped (0 minutes, shown as skipped) when reused, run as before otherwise. If the `cinema` job fails, its outputs are empty and nothing is skipped.
- A matrix job with legs the machine can't run gets `exclude: ${{ fromJSON(needs.cinema.outputs.exclude_<job> || '[]') }}`, and never the whole-job skip. verify emits a leg only if it is exactly one leg the checked-out workflow's matrix expands to (by the matrix's own keys), and skips a matrix job whole only when every real leg was reused.
- Left alone: jobs other jobs need, `if:`s with status functions or over several lines, matrices with object or non-string values or their own `exclude:`.
- Required checks: a skipped job reports its check as passed, but an excluded leg reports nothing, and a matrix job skipped whole reports one check under the job's name. So a leg the default branch requires by name (rulesets, and classic protection when readable, via `gh`) stays in CI. If `gh` can't say, setup prints a note instead.
- The edit is textual (positions from the YAML parse); everything else stays byte-for-byte. Rerunning `setup` starts from the original; `cinema setup --undo` removes the edits.

The single-job and parallel gates below remain for repos that want a separate required check.

### 11.1 One job, not three

`vision §15.3` used separate `plan`, `baseline`, and `gate` jobs. Each job is a runner start, and billed minutes round up per job. The MVP uses **one job** that verifies, runs only what's missing inline, then decides:

```yaml
# .github/workflows/cinema.yml (generated by `cinema init`; pin actions by SHA)
name: cinema
on:
  pull_request: {}
  push: { branches: [main] }
  merge_group: {}
permissions: { contents: read }
concurrency:
  group: cinema-${{ github.event.pull_request.number || github.ref }}
  cancel-in-progress: true
jobs:
  cinema:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@<SHA>
        with: { fetch-depth: 0, persist-credentials: false }
      - name: install cinema
        run: <download pinned release + verify checksum>
      - id: verify
        run: cinema verify --event "$GITHUB_EVENT_PATH" --plan-out plan.json
        env:
          CINEMA_TRUSTED_KEYS: ${{ vars.CINEMA_TRUSTED_KEYS }}
          # private repos: used only for the notes fetch (one-shot http.extraheader), never persisted (P0-2)
          CINEMA_FETCH_TOKEN: ${{ github.token }}
      # toolchain setup only when something must run
      - if: steps.verify.outputs.needs_go == 'true'
        uses: actions/setup-go@<SHA>
      - if: steps.verify.outputs.needs_node == 'true'
        uses: actions/setup-node@<SHA>
      - name: run missing tasks + gate
        run: cinema verify --event "$GITHUB_EVENT_PATH" --plan plan.json --execute --gate
      - if: always()
        uses: actions/upload-artifact@<SHA>
        with: { name: cinema-gate-report, path: gate-report.json, retention-days: 7 }
```

- *Superseded by skip-in-place (§11.0), the default: existing required checks stay required; skipped jobs report as passed.* In this older single-job form, branch protection required the `cinema` job only.
- `--execute` runs tasks on the bare runner, not in their container, image and services, so it doesn't meet §9.2's fidelity model. It stays for repos using this form; skip-in-place never executes anything in the gate.
- Fully reused PR = one runner, checkout, binary download, signature verify. **P0 measures this number.**
- Splitting into a parallel matrix for slow suites is P3+ (`vision §15.5`). The single-job form is the default.

### 11.2 `cinema verify` logic

```text
candidate = from event: repo ID, event type, tested SHA
            (pull_request → GITHUB_SHA = merge commit, NOT the PR head)
tasks     = required tasks from .cinema/tasks.yaml at the BASE branch
            (a PR can't loosen its own requirements)
keys      = CINEMA_TRUSTED_KEYS (repo variable; PR can't edit)

if fork PR:            all tasks → RUN (FORK_PR)
if keys missing/bad:   all tasks → RUN (POLICY_UNAVAILABLE)

source_digest = fingerprint of the checked-out candidate tree
for task in tasks:
    if not task.reuse.allowed:          RUN (REUSE_DISABLED); continue
    expected = action_key(task, source_digest, expected environment (below))
    for receipt in notes on candidate SHA, PR head SHA, and each PR commit (bounded):
        verify DSSE signature with trusted keys      else reject SIGNATURE_INVALID / SIGNER_NOT_ALLOWED
        strict-parse predicate                        else reject MALFORMED
        repository_id matches                         else reject WRONG_REPOSITORY
        subject digest == expected                    else reject (INPUTS_CHANGED | TASK_DEFINITION_CHANGED | ENVIRONMENT_MISMATCH)
        executor.arch == the covered job's runner arch
          (or task arch: any)                         else reject ARCH_MISMATCH
        executor.emulated and task emulation: forbid  → reject EMULATED
        age <= max_age                                else reject RECEIPT_EXPIRED
        result passed, origin acceptable              else reject
        → REUSE; break
    else RUN (NO_RECEIPT or last rejection reason)

--execute: run every RUN task's exact argv; record pass/fail
--gate:    pass iff every task is REUSE or RUN+passed; write gate-report.json
```

**Expected environment.** For a run in a container, the environment is what the task pins: the image digest (`container:` or the runner-like `image`), `runtime:` versions, service image digests, the arch, the runner size, and allowlisted env. The gate computes it from the contract on the base branch, without running anything. Every value is written into `tasks.yaml` by `cinema setup` (image digests, runtimes, runner size, `cinema_version`); nothing comes from defaults inside the binary, so two cinema versions can't silently disagree. Images must be on the trusted list (`vars.CINEMA_TRUSTED_IMAGES`, default: cinema's published runner images), else `IMAGE_UNTRUSTED`.

**What CI would use today, not what was pinned then.** The contract records what a job resolved to; CI resolves it again on every run. When the workflow itself pins exactly (`go-version: 1.26.10`, `image@sha256:…`, `runs-on: ubuntu-24.04`) the two can't drift. When it floats, the gate re-resolves at verify time and compares:

| Floating in the workflow | Resolved at verify time from |
|---|---|
| `go-version: stable`, `1.26`, `go-version-file` with a range | Go's release index |
| `node-version: lts/*`, `24` | nodejs.org's release index |
| image by tag (`container:`, `services:`) | a registry HEAD (tag → digest) |
| `runs-on: ubuntu-latest` | the runner's `ImageOS`/`ImageVersion` (the gate runs on the same label) |

A mismatch is `ENVIRONMENT_MISMATCH` and the job runs. A source that can't be reached means RUN, never skip. Unpinned probes (a `toolchain: cmd` nothing fixes) can't be verified this way, so such tasks aren't eligible to skip; `cinema analyze` says why.

**Event.** A job can behave differently per event (`if: github.event_name == 'push'` on a step, scripts reading `GITHUB_REF`). The contract records the event a task was drafted for (`event: pull_request`), the receipt carries it, and the gate requires it to match the triggering event, else `EVENT_MISMATCH`. The analyzer drafts one task per event the job runs on when its steps differ by event, and sends jobs whose conditions it can't evaluate to review. Inside the container, the full default `GITHUB_*` set for that event is always set, since scripts and tools read them invisibly.

**Executor.** Only receipts with `executor.kind: container` count for skip-in-place. Host-process runs (`cinema exec`, `cinema run` without an engine) drive local status only, whatever `platform:` or `capture:` says: a host sees the host's filesystem (case-insensitive on macOS), tools and limits.

**Arch.** The covered job's runner arch comes from its `runs-on` label (`ubuntu-*-arm` → arm64, other GitHub Linux labels → amd64; a matrix expression is resolved per leg). A label the gate can't map, such as a larger-runner name or a group, means RUN for that job only. The gate judges each receipt against its own job's arch, never against the arch of the runner the gate itself is on.

**Duration.** Receipts record the run's duration. A fast laptop can hide a CI timeout, so the gate scales the duration by the repo's measured local-to-CI speed ratio (learned from shadow pairs, §11.5; 1× until known) and rejects the receipt as `TOO_CLOSE_TO_TIMEOUT` when the scaled time exceeds 50% of the job's `timeout-minutes` or an argv `-timeout`.

**Failures.** Retrying until green must not work. Failed runs are signed too (`result.status: failed`) and published in notes like passes. The gate treats a failure on the same `action_key`, from any trusted key, as blocking every pass (`FAILED_ELSEWHERE`), and locally `cinema push` won't sign a pass for an `action_key` that failed until it passes K consecutive times (default 3) and the user confirms it's flaky.

Key consequences:
- **Merge commit, not PR head.** The source digest is computed from the tree CI checked out. A PR-head receipt matches only if the merge result has byte-identical inputs, meaning the base hasn't changed in ways that touch the tree. When main moves, tasks rerun. That's correct, not a bug.
- **Task list from the base branch.** A PR that edits `.cinema/tasks.yaml` gets its new tasks evaluated as RUN, with no reuse for changed contracts.
- **Personal-mode honesty:** a PR from a collaborator with write access can still edit the workflow file itself. That's true of every Actions check, and maintainers review `.github/` and `.cinema/` changes (use CODEOWNERS). Protected authority via a GitHub App is `vision §15.6`.

### 11.3 Evidence arriving late

If the candidate has zero receipts, the gate may poll `git fetch` for notes for up to `CINEMA_WAIT` (default 0, max 120 s) before falling back to RUN. The wait is bounded, and timing out simply means the tasks run.

### 11.4 Gate outcomes

| Situation | Result |
|---|---|
| All tasks REUSE | pass |
| Some RUN, all passed | pass |
| Any RUN failed / canceled / not executed | fail |
| tasks.yaml on base invalid | fail (`POLICY_INVALID`): fix config, don't guess |
| Trusted keys missing | every task RUN (safe fallback, not a block) |
| Notes fetch fails | every task RUN (`TRANSPORT_UNAVAILABLE`) |
| Job canceled | no success published (Actions handles this) |

---

### 11.5 Shadow mode: proving a stand-in before it skips

A drafted or hand-written task is a claim that it tests what its CI job tests. Before a job starts skipping, that claim is checked against CI itself.

1. **Default is shadow.** The gate treats a covered job as `SHADOW` (it runs in CI regardless of receipts) unless the **base branch's** `tasks.yaml` has a graduation entry for it naming the job's current `job_digest` and leg:
   ```yaml
   graduated:
     "ci.yml#test@sha256:…(os=ubuntu-latest,node=22.x)": {agreed: 3, backtested: 20, at: 2026-11-02, by: <key-fp>}
   ```
   A changed job definition is a new digest, so it's back in shadow. A PR can't graduate its own jobs (base branch only), and the entry is reviewed like any CI change. This applies to existing setups too: no job skips without one.
2. **Backtest graduation, day one.** `cinema graduate` runs the stand-in on the last N merged commits (default 20, in containers) and compares with those commits' stored CI conclusions (via `gh`). Agreement on all of them graduates the job without waiting for new PRs. Commits where CI failed matter most: a stand-in that passes where CI failed is a false green caught before it happens.
3. **Live agreement.** For jobs that can't be backtested (too little history), each PR in shadow adds a pair: local result and CI result for the same tree. `cinema push` and `cinema shadow` collect the CI side through the API, locally: nothing extra runs in CI and the workflow keeps `contents: read`. After N agreeing pairs (default 3) `cinema graduate` proposes the entry.
4. **Disagreement.** A pass locally and a failure in CI on the same tree is a false green: `cinema` removes the graduation in a commit it proposes, flags it in `cinema status` and the PR summary, and the job runs in CI until it re-graduates. The opposite (local fails, CI passes) is flagged too. A disagreement that a CI rerun reverses is recorded as flaky; a flaky job graduates only with `allow_flaky: true` in its entry, an explicit claim.
5. **Spot checks.** A graduated job still runs in CI on a sample of runs (default 1 in 20), picked from `GITHUB_RUN_ID` by the `cinema` job, and its result is compared like a shadow pair. That makes drift visible; it doesn't make it impossible.

6. **Failure pairs compare failed tests (closed run 2; was: failures agreed by exit status only).** A stand-in that fails for another reason than CI (a missing file in a lab snapshot fails the build in 57 s where CI's failed in its tests) used to count as agreement. Now, when both sides failed, `graduate` compares the sets of failed tests: the stand-in's parsed from its recorded output (go test, pytest, jest, vitest, cargo; JUnit files are left out since CI's side has only its log), CI's parsed the same way from the job's log (`gh api …/actions/jobs/<id>/logs`). Identical sets count as agreement. Different sets, no parseable failed test on either side (a build error, a tool that isn't parsed), or a list that may be incomplete (output cut, a go package failed without a failing test, the runner's counts name more failures than its output) don't count, and graduate says which. Not counted isn't a disagreement: it neither graduates nor blocks. Still open: comparing the failed *step* for jobs whose failures aren't tests.

Shadow pairs also measure the local-to-CI speed ratio (§11.2, Duration). Shadow results are evidence about a stand-in, never about a commit: they don't make a receipt valid or invalid.

### 11.6 The PR summary

The `cinema` job writes a job summary on every run: which jobs and legs skipped, with whose receipt and from which host; which ran and why (reason code); which are in shadow; and the CI minutes skipped with their price (from the job's history). Prices come from one table of GitHub's current per-minute rates (updatable without a release, since GitHub's pricing moves), and minutes are always shown next to dollars. One table, the three states kept separate: ran locally, reusable for this commit, accepted by CI.

---

### 11.7 GitLab CI

The core (tasks, container runs, fingerprints, signed receipts in git notes, shadow mode) doesn't depend on the forge. The edges do, and each gets a GitLab implementation behind a small interface: **reading CI**, **skipping**, **the CI API** (results for graduate and live pairs) and **where trusted keys live**.

| Edge | GitLab |
|---|---|
| Reading CI | `.gitlab-ci.yml`: `stages`, `script`/`before_script`/`after_script`, `variables`, `rules`/`only`/`except`, `needs`, `extends`, `!reference`, local `include`, `parallel:matrix`, `services`, `default:`. Each job names its `image:`: the environment is that image's digest (a container task), so there's no runner image to imitate and no actions to translate. `include: remote/project/template` and `trigger:` jobs → review. |
| Skipping | `rules:` are fixed at pipeline creation, so they can't read receipts. A `cinema` job writes a **dynamic child pipeline** with only the jobs that still need to run (`trigger: include: artifact`); proven jobs never get a runner. Fallback where child pipelines don't fit: a first script line that exits early on a receipt (saves the run, not the runner start). |
| Merge requests | Merge request pipelines test the head (Free) or the merged result (Premium "merged results"); merge trains are merge queues (§5.5). `CI_PIPELINE_SOURCE` is the event. |
| Receipts | Git notes as on GitHub if GitLab accepts `refs/notes/*` pushes and the job token can fetch them (to verify); else CI artifacts or the generic package registry. |
| Keys | A protected, masked CI/CD variable (`CINEMA_TRUSTED_KEYS`), so a merge request can't change it. |
| CI API | `/projects/:id/pipelines`, `/jobs`, job traces (token or `glab`). |
| Runner sizes | SaaS Linux runners by tag (small 2 vCPU/8 GB, medium, large…; cost factors), self-hosted by tag; size and arch from the tag, declared in the task. |

P4 (run 2): the analyzer and the child-pipeline generator with tests, the forge interfaces, and answers to the open questions (notes refs, job token, Free vs Premium) from GitLab's docs and source or a local GitLab in a container. A live spike on gitlab.com needs an account here (none yet).

## 12. Reason codes (MVP set)

```text
NO_RECEIPT              INPUTS_CHANGED           TASK_DEFINITION_CHANGED
ENVIRONMENT_MISMATCH    WRONG_REPOSITORY         SIGNATURE_INVALID
SIGNER_NOT_ALLOWED      MALFORMED                RECEIPT_EXPIRED
PARTIAL_COVERAGE        SOURCE_CHANGED_DURING_RUN TOOL_CACHE_REJECTED
REUSE_DISABLED          FORK_PR                  POLICY_UNAVAILABLE
POLICY_INVALID          TRANSPORT_UNAVAILABLE    RUN_FAILED
VCS_MISMATCH            ARCH_MISMATCH            EMULATED
IMAGE_UNTRUSTED         SHADOW                   CHECKOUT_MISMATCH
EVENT_MISMATCH          TOO_CLOSE_TO_TIMEOUT     FAILED_ELSEWHERE
WORKSPACE_EVIDENCE      HOST_PROCESS_EVIDENCE
```

`VCS_MISMATCH`: a `vcs_context: exact` task whose run was on a different commit, or on a dirty tree.
`ARCH_MISMATCH`: the run's arch isn't the covered job's runner arch, and the task doesn't say `arch: any`. `EMULATED`: emulated run for a task with `emulation: forbid`. `IMAGE_UNTRUSTED`: the run's image isn't on the trusted list. `SHADOW`: the job is in shadow mode and runs regardless (§11.5). `CHECKOUT_MISMATCH`: the source digest inside the container didn't equal the host's (§5.2); never signed. `EVENT_MISMATCH`: drafted for another event. `TOO_CLOSE_TO_TIMEOUT`: the scaled duration is over half the job's timeout. `FAILED_ELSEWHERE`: a trusted key published a failure for the same `action_key`. `WORKSPACE_EVIDENCE` (ADR 0002) and `HOST_PROCESS_EVIDENCE`: not a clean-checkout container run (§11.2).

A failed run on the exact same inputs (contract, environment, source) blocks reuse of any earlier pass. Reusing the pass would hide a flaky or broken check.

CLI, JSON, dashboard, and gate report all use the same codes. Human text is presentation only. Full list in `vision §9.5`.

**Reuse-rate metric:** `REUSE / (REUSE + RUN)` per task over real PRs, broken down by reason code. If `ENVIRONMENT_MISMATCH` or `ARCH_MISMATCH` dominates, the runner image or pins need work, or the task honestly needs `arch: any`. If `INPUTS_CHANGED` dominates after a merge from main, that's the whole-source cost showing up, and it informs whether narrower inputs (`vision §11.5`) are worth the risk.

---

## 13. Local storage

```text
$XDG_DATA_HOME/cinema/  (macOS: ~/Library/Application Support/cinema/)
  state.db              SQLite, WAL
  blobs/sha256/ab/<d>   logs, manifests (temp file + fsync + rename)
  keys/                 only when no OS keychain
```

Tables: `repos`, `tasks` (by contract digest), `runs`, `snapshots` (digest + manifest blob), `receipts` (signed bytes), `pushes`.

- Short write transactions, `busy_timeout`, bounded retry. Concurrent `cinema exec` calls must not corrupt anything (ACC-051).
- A run becomes `PASSED` only after its record and blobs are durable. Crashes leave `RECORDING_INCOMPLETE`, which is never signable. A run the user stops (Ctrl-C, SIGTERM to cinema) is `interrupted`: never signable, and unlike a failure it doesn't block reuse of a pass on the same inputs. A timeout is a failure (a hang is a real bug).
- Retention defaults: run metadata 90 days, passing logs 7 days, failing logs 30 days.
- Nothing lives under the repo except `.cinema/tasks.yaml`.

### 13.1 Cleanup contract

CInema leaves nothing behind that it no longer needs. No daemon, so every rule runs inline.

| Leftover | Rule |
|---|---|
| Processes the command left running | When the child exits, SIGTERM its process group, then SIGKILL after the grace period; report it on stderr and in the run record. `setsid` escapees aren't touched, but the pipes they hold are dropped after the grace period. |
| Clean-checkout dirs (`run --commit`) | Shared clone in a stable, locked slot `<data dir>/checkouts/<repo>/<n>` (per-user, 0700, never a shared TMPDIR: a predictable /tmp path could be pre-created by another user), never `git worktree add`, so `.git` is never modified. A stable path keeps path-keyed build caches warm (lab: random paths grew a Go cache to 77 GB). Each run fully resets the slot, ignored files included (ADR 0002). Slots unused for 7 days are swept. Each task in a clean checkout gets a private `TMPDIR`, removed afterwards, so killed tools can't leave `/tmp/go-build*` behind. |
| Shared rebuilt artifacts (`artifacts/<key>`) | A task that rebuilds another job's artifact stores it here for other tasks at the same commit (key: producer job, commit, Go/Node versions). Written to `<key>.tmp` and renamed, one writer per key (`<key>.lock`). Entries unused for 24 h are swept by any `cinema run`, partial writes and stale locks after 1 h (CLEAN-008). |
| Containers and networks | Created with `--rm` (or removed explicitly) and labeled `cinema.run=<id>`. Any `cinema run` removes labeled containers and networks whose run has ended (crash leftovers). |
| Cache volumes (`cinema-cache-<repo>-<tool>-<version>`) | Per repo, tool and tool version, labeled. Size-capped (default 10 GiB each, trimmed oldest-first), swept when unused for 30 days, and by `cinema gc --caches`. Never hold results, only downloads and build caches (CLEAN-011). |
| Images cinema pulled | Pinned digests, labeled or recorded in the store. Unused for 30 days: removed, unless the user pulled the same image themselves (never remove what cinema didn't add). |
| Remote leases (§9.3) | Recorded in the store before use; released at the end of every run, on the next start after a crash, or with `cinema hosts --cleanup`. Each lease also has a provider-side TTL. Snapshots cinema made are listed and expire after 30 days unused. |
| Pre-push hook (`.git/hooks/pre-push`) | Written only by `cinema hook install`, with a marker line, never over a hook cinema didn't write. `cinema hook uninstall` removes it only if the marker is there. |
| Partial blob/DB writes | Temp file + rename. On startup, sweep `*.tmp` older than 1 h in the data dir. |
| Provisioned runtimes (`runtimes/<lang>/<version>`) | Installed atomically (temp dir + rename). The daily pass removes versions unused for 90 days and interrupted installs older than 1 h. |
| Fingerprint caches (`fscache/<repo>.gob`) | One file per repo, rewritten on use. The daily pass deletes caches unused for 90 days, and temp files older than 1 h. |
| Per-test results | Counts and failed tests are a column of the run row, deleted with it. The full per-test list is a blob referenced by the row, expired and size-capped with the run's logs, and unlinked by the same unreferenced-blob pass. |
| Old runs and logs | §13 retention. Applied by any command, at most once per 24 h (timestamp in `state.db`); one indexed `DELETE` plus blob unlink, under 50 ms. Blobs no longer referenced by any row are deleted in the same pass. `cinema gc` forces it. |
| Data dir size | Hard cap (default 2 GiB). Past it, delete oldest passing logs first, then failing logs. Run metadata goes last. |
| Notes, local | `git notes prune` on our refs during the daily pass (notes for commits that no longer exist). |
| Notes, remote | Per-key refs only grow by one commit per push. `cinema push` squashes our own ref to a single commit holding only notes for reachable commits newer than `max_age`, and force-pushes it with `--force-with-lease`. Safe because only this key writes this ref. |
| Dashboard | Nothing persisted: token and sessions live in memory. Runs started from the page get SIGTERM on shutdown, and the server waits for their cleanup. |
| CI artifacts | `gate-report.json` uploads with `retention-days: 7`. |
| Everything | `cinema uninstall [--remote]` removes the data dir, keys (keychain + file), local notes refs, remote notes refs (with `--remote`), and prints the workflow / branch-protection lines to remove. It never edits workflows itself. |

Inside the repo, CInema only writes `.cinema/tasks.yaml` (`cinema init`, `cinema setup`, `cinema graduate`) and, with `cinema setup`, the workflow edits for skip-in-place, always shown first and asked. `cinema setup --undo` removes those edits; `cinema uninstall` prints what to remove and edits nothing.

Tests: CLEAN-001 leftover child stopped (polite + SIGTERM-ignoring). CLEAN-002 `run --commit` interrupted by SIGINT leaves no temp dir. CLEAN-003 crash mid-blob-write: next start sweeps the `.tmp`. CLEAN-004 retention removes expired rows and their blobs, keeps blobs still referenced. CLEAN-005 size cap evicts passing logs first. CLEAN-006 `uninstall` leaves no data dir, key, or local notes ref. CLEAN-007 squashed notes ref still verifies every receipt within `max_age`. CLEAN-008 shared artifacts swept after a day unused, partial writes after an hour. CLEAN-009 a killed run leaves no container, network or lease after the next start. CLEAN-010 `cinema uninstall` removes cinema's volumes, images and snapshots, and nothing else. CLEAN-011 a cache volume over its cap is trimmed below it; one unused for 30 days is removed.

---

## 14. Acceptance tests (MVP subset)

IDs match `vision §37.2`. Every row is an automated test; P2 can't ship with any row red.

| ID | Scenario | Required outcome |
|---|---|---|
| ACC-001 | Wrapped command exits 0 | Exit captured; no task credit without exact match |
| ACC-002 | Wrapped command exits non-zero | Code preserved; no receipt |
| ACC-003 | Child killed by signal | Recorded as signal; no success |
| ACC-004 | Grandchildren on timeout/cancel | Process group terminated |
| ACC-006 | DB unwritable | Child semantics preserved; no receipt |
| ACC-007 | Huge/malicious output | Bounded; truncation flagged; dashboard renders safely |
| ACC-008 | Pipeline with masked failure | Only the explicit shell's outcome recorded |
| ACC-009 | Source changed after run | Stale for new HEAD |
| ACC-011 | Dirty tree later committed identically | Reused for that commit |
| ACC-012 | Untracked file affects task | Included in fingerprint |
| ACC-013 | Symlink / exec-bit / submodule change | Fingerprint changes |
| ACC-014 | Task reads git metadata | `vcs_context: exact` required for cross-commit reuse |
| ACC-015 | File removed or renamed | Fingerprint changes |
| ACC-017 | Same `npm run x`, changed script body | Old receipt rejected |
| ACC-018 | Narrowed test filter | Doesn't satisfy full task |
| ACC-019 | Different toolchain version | `ENVIRONMENT_MISMATCH` |
| ACC-020 | Go test cache hit with `accept_tool_cache: false` | Rejected |
| ACC-021 | Formatter modifies files | `SOURCE_CHANGED_DURING_RUN` |
| ACC-023 | Receipt payload byte flipped | `SIGNATURE_INVALID` → RUN |
| ACC-024 | Valid signature, untrusted key | `SIGNER_NOT_ALLOWED` → RUN |
| ACC-025 | Receipt from another repo | `WRONG_REPOSITORY` |
| ACC-026 | Duplicate JSON keys | `MALFORMED` |
| ACC-027 | Oversized note | Rejected without memory blowup |
| ACC-028 | Key removed from trusted list | Its receipts rejected next run |
| ACC-031 | Notes not pushed | All RUN; gate still works |
| ACC-032 | Two machines push notes concurrently | Separate refs; nothing lost |
| ACC-033 | Remote rejects `--atomic` | Notes-first fallback; reported |
| ACC-035 | All reused | Gate passes from verification, not from skipped steps |
| ACC-037 | Inline RUN task fails | Gate fails |
| ACC-038 | PR head ≠ merge commit inputs | Head receipt rejected; RUN |
| ACC-041 | Fork PR with forged receipt + edited tasks.yaml | All RUN; tasks from base |
| ACC-047 | Request from foreign Origin / rebinding Host | 403 |
| ACC-048 | Log contains `<script>` / ANSI tricks | Rendered as inert text |
| ACC-051 | Concurrent `cinema exec` | DB consistent |
| ACC-066 | Crash between run and signing | No signable record |
| ACC-101 | arm64 run, amd64 job, no `arch: any` | `ARCH_MISMATCH` → job runs |
| ACC-102 | amd64 under Rosetta | Receipt says `emulated: true`; rejected for `emulation: forbid` |
| ACC-103 | Windows host with `core.autocrlf=true` | Container checkout identical to the commit; digest equal |
| ACC-104 | Container checkout digest ≠ host digest | `CHECKOUT_MISMATCH`; nothing signed |
| ACC-105 | Receipt for the merge tree (§5.5) | Matches GitHub's merge commit; base moves → rejected |
| ACC-106 | Job in shadow mode with a valid receipt | Runs in CI; result compared |
| ACC-107 | CI fails a graduated job's commit that passed locally | Job back to shadow; flagged |
| ACC-108 | Remote host reports a wrong source digest | Run rejected; not signed |
| ACC-109 | Ctrl-C during a remote run | Lease released; no receipt |
| ACC-110 | Task memory limit lower than the test needs | Fails locally, as in CI |
| ACC-111 | Image not on the trusted list | `IMAGE_UNTRUSTED` → job runs |
| ACC-112 | `go-version: stable` moves past the pinned version | `ENVIRONMENT_MISMATCH` → job runs |
| ACC-113 | Image tag in the workflow now points to a new digest | `ENVIRONMENT_MISMATCH` → job runs |
| ACC-114 | PR-drafted receipt on a `push` event with a push-only step | `EVENT_MISMATCH` → job runs |
| ACC-115 | Pass after a failure on the same `action_key`, other machine | `FAILED_ELSEWHERE` → job runs |
| ACC-116 | Repo goes private (smaller runner) | old receipts miss; jobs run |
| ACC-117 | Local 4 min, CI ratio 3×, `timeout-minutes: 15` | `TOO_CLOSE_TO_TIMEOUT` |
| ACC-118 | Host-process receipt for a `platform: any` task | `HOST_PROCESS_EVIDENCE` → job runs |
| ACC-119 | Graduation entry for an old `job_digest`, or added by the PR itself | `SHADOW` → job runs |
| ACC-120 | Backtest: stand-in passes a commit where CI failed | Not graduated; reported |
| ACC-121 | Squash-merged PR, `push` to main | Receipts from the PR's merge-result tree match |
| ACC-122 | Unmappable `runs-on` label in one job | That job runs; others still skip |

Plus:
- **Property test:** for random task sets × receipt sets × run outcomes, the gate never passes with a task lacking a qualifying REUSE or passed RUN.
- **Property test:** adding an untrusted receipt never flips a fail to a pass.
- **Fuzz tests** (`go test -fuzz`): DSSE envelope parser, predicate parser, tasks.yaml parser, manifest path handling.

---

## 15. Performance budgets (measure, don't promise)

| Area | Target |
|---|---|
| `exec` overhead, excluding hashing | p95 < 50 ms |
| Fingerprint, 10k-file repo, warm cache | < 300 ms |
| Fingerprint, 100k-file repo, warm cache | measure and publish |
| `cinema verify`, 100 receipts, no network | < 2 s |
| Gate job, everything reused, `ubuntu-latest` | measure in P0 → this decides the product |
| Dashboard first paint | < 1 s |
| Container start + checkout, warm image, 10k-file repo | < 3 s (Docker on Linux), measure elsewhere |
| Emulated amd64 slowdown (Rosetta, QEMU) | measure in P4-1, per project |
| `cinema push` plan (what to run, where, cost) | < 1 s, no network |

Report hashing and wrapper overhead separately, always.

---

## 16. Phases

### P0 — Feasibility spikes (do first; throwaway code allowed)

- [x] **P0-1 Gate cost.** (fully reused gate 9 s vs 140 s realistic Go CI; continue. See spikes/README.md) Single-job workflow on a real repo: measure wall time and billed minutes for (a) all reused (simulate with a stub that verifies one DSSE receipt), (b) the original CI. **Kill/continue decision.**
- [x] **P0-2 Git notes round trip.** (private repos need a token for the notes fetch; fork case untested) Push `refs/notes/cinema/v1/*` to GitHub; fetch in Actions with `actions/checkout` + `git fetch origin 'refs/notes/cinema/*:refs/notes/cinema/*'`; confirm `--atomic`; test two machines pushing different refs; test a fork PR (expect: no notes; that's fine).
- [x] **P0-3 Merge-commit fingerprint.** (match only when base hasn't moved) For a PR, compare the tree fingerprint of PR head vs `GITHUB_SHA` merge commit across: base unchanged, base changed in unrelated file, base changed in task input. Confirms how often head receipts can match in practice.
- [x] **P0-4 Hashing cost.** (results: spikes/README.md) Fingerprint 1k / 10k / 100k-file repos, cold and warm.
- [x] **P0-5 Reuse estimate.** (test-bed CI 140 s → 9–50 s; only 30–95% of real PRs are up to date at merge, which caps whole-source reuse) Take 2–3 of your own repos: list CI checks, mark which could honestly be `platform: any`, estimate the hit rate.
- [~] **P0-6 License + predicate domain.** (Apache-2.0 chosen; domain open) Pick license; reserve a domain/URI for `predicateType`.

**Exit:** P0-1 shows a clear time win when everything is reused, and P0-2 works on github.com.

### P1 — Local core + tiny dashboard

- [x] `cinema exec`: argv exec, exit code, signals, process group, bounded tee capture (§4.1) (overhead p95 ≈ 3 ms; DB recording lands with the store)
- [x] SQLite store, blob store, run state machine, crash recovery (§13), cleanup contract (§13.1) for store-owned data (exec overhead p95 ≈ 7 ms with recording)
- [x] Whole-source fingerprint + metadata cache (§5) (exec in a 10k-file repo, before + after fingerprints, warm: p50 86 ms)
- [x] `tasks.yaml` parser (strict), exact matcher, action key, environment fingerprint (§6)
- [x] Mutation detection, tool-cache rejection (§4.4–4.5) (before/after recorded; `go test` without `-count` rejected at config load)
- [x] `cinema run [--commit]` with clean-checkout mode (§5.2); `--all` skips tasks already reusable
- [x] `cinema status/explain --json`, reason codes (§10, §12), `cinema trust` for toolchain commands
- [x] `cinema dashboard`: one page, token auth, Host/Origin checks, SSE refresh, `--allow-host` (§10.4); Run buttons (POST + Origin + CSRF)
- [x] `cinema init`: tasks skeleton from detected `go.mod` / `package.json` (npm/pnpm/yarn; never writes `platform: any`; prints the AGENTS.md paragraph)
- [ ] Tests: ACC-001…021, 047, 048, 051, 066; parser fuzzers

**Exit:** you use it daily on your own repos for a week. `status` is never wrong about stale vs reusable.

### P2 — Signed evidence + GitHub gate (the MVP)

- [x] `cinema keys` (§7.3) (file with 0600; OS keychain in P4)
- [x] DSSE + in-toto statement build/sign/verify with fixed test vectors; strict parser (§7)
- [x] Git notes publish/fetch, per-key refs, size caps (§8)
- [x] `cinema push` with atomic + fallback, separate reporting (§8.1)
- [x] `cinema verify` (`gate-report.json` still open): candidate resolution, base-branch tasks, fork rule, REUSE/RUN, `--execute`, `--gate`, `--skip-outputs`, step outputs for conditional toolchain setup (§11)
- [x] Late-evidence wait (§11.3)
- [ ] Release binaries (linux/darwin/windows × amd64/arm64) with checksums and a tagged version for `cinema setup` to pin
- [ ] VPS flow documented and tested end-to-end (superseded by P4: a VPS is an ssh host, §9.3)
- [ ] Dashboard: evidence + CI panels
- [ ] Tests: all §14 rows, property tests
- [ ] README: install, register key, first reused PR, **how to remove CInema and go back to normal CI**

**Exit (MVP demo):** on a real repo,
1. a PR whose checks ran locally gets a green `cinema` check with the expensive tasks reused, verified by the gate report;
2. a byte-flipped receipt, an untrusted key, a changed input, missing notes, and a failing inline task each produce RUN or a failing gate, never a false green.

### P4 — Local-first container CI on any host (ADR 0003)

- [~] **P4-1 Spike.** (Mac with Colima done: every result matched once three CInema bugs were fixed; Rosetta 1.6–3.8× cold; [results](spikes/local-container/RESULTS.md). Apple `container` needs macOS 26; Windows not yet.) On an Apple-silicon Mac: run tasks that pass on the Linux lab box (eslint's tests, traefik's cross-builds, prometheus's container job, a services job) in Docker, amd64 under Rosetta and arm64 native, then in Apple `container`. Compare results with the lab and with CI; time them; check CPU features under Rosetta. Same on Windows (Docker Desktop, WSL2). **Decides** whether emulated amd64 is fast enough to be the default.
- [x] **False-green closers first** (needed before anything skips on container evidence): re-resolve floating versions and tags at verify time; `event` in contract and receipt; signed failures and `FAILED_ELSEWHERE`; container-only evidence; runner size in the environment; duration vs timeout (§11.2)
- [x] Shadow mode with backtest graduation, live pairs and spot checks (hashed run id); existing setups start in shadow (§11.5); per-job summary on the run page (§11.6; minutes saved still open)
- [~] Docker-compatible engine (task.Sandbox; `doctor` engine report still open)
- [x] Runner fidelity (§9.2): images for Ubuntu 22.04, 24.04, 26.04 (amd64, arm64) published and pinned, chosen by the job's `runs-on`; memory within 1 GB / 10%, CPUs and OS exact; checkout bind-mounted (git bundle + cross-check only for remote hosts)
- [ ] Cache volumes per tool version, size caps (§9.2, CLEAN-011)
- [~] Merge-result runs; receipts on the PR head (§5.5). Push-to-main lookup still open
- [~] Pre-push hook warning (§8.1). `--background` still open
- [ ] ssh hosts via `DOCKER_HOST=ssh://` (§9.3)
- [ ] Credentials kept out of containers by default; `credentials:` opt-in (§9.2)
- [x] `cinema debug <task>` (§9.2)
- [ ] Flaky-task detection from local history (§0.3)
- [ ] Machine-wide scheduler for concurrent invocations (§9.4)
- [ ] `merge_group` receipts (§5.5); runner prices from one updatable table (§11.6)
- [ ] Short-lived, labeled keys for agents in sandboxes (§7.3)
- [ ] Apple `container` engine
- [~] Runtime providers (§6.5): registry; Go, Node, Python, uv, Java (Temurin), Gradle, Rust, Bun, Deno, .NET built with their setup actions. Run 2: pins without a setup action (version files), `.tool-versions` / mise, Maven SNAPSHOT and Gradle lock hazards. Ruby blocked (not relocatable, no checksums); PHP, Erlang/Elixir P5
- [ ] `credentials:` from `env:` and `file:` providers, redacted from logs (§9.7)
- [ ] Per-step timing in run records; `cinema check` alias; disk-pressure pruning (§9.7)
- [ ] Repos that use Dagger: `dagger` steps translate as tasks (§9.7)
- [x] Per-test results parsed from task output (go test, pytest, jest/vitest, cargo, JUnit XML); failure pairs compare failed tests (§11.5.6); `cinema run --failfast` (§9.7, run 2). Not yet: `cinema run --failed-only`
- [ ] Real projects per language, locally against upstream CI's results (run 2)
- [ ] GitLab CI (§11.7): analyzer, child-pipeline skipping, forge interfaces (run 2); live spike once there's a GitLab account
- [ ] Tests: ACC-101…122, CLEAN-009…011

**Exit:** (Mac part met 2026-10-31: [cli#9](https://github.com/mjtechguy/cinema-lab-cli/pull/9); minutes saved in the summary still open) a developer on a Mac, with no other hardware, pushes a PR whose Linux jobs skip in CI, every skipped job graduated from shadow mode (backtest or live), and the PR summary shows what was saved. Then the same on Windows.

### P5 — Scaling out

- [ ] Provider plugin protocol, one provider with SSH + Docker first (boat-style), then exec-only ones (§9.3)
- [ ] Leases, spend estimate and caps, the `auto` policy (§9.3, §9.5)
- [ ] Fan-out across hosts (§9.4)
- [ ] Code-defined stand-ins, `pipeline:` (§9.6)
- [ ] Maintainer-run fork PRs (§8.2)
- [ ] Ruby, .NET, Elixir/Erlang providers (§6.5); secret providers 1Password, Vault, AWS, GCP; OpenTelemetry export; `cinema up` (§9.7)

### P3+ — Next, in rough order (details in vision.md)

0. **Started:** `cinema analyze` reads GitHub workflows and drafts tasks per Linux job and matrix combination. It handles the run steps (one subshell per step, like Actions), literal env, matrix substitution, a matrix-only subset of `if:`, and exact runtimes from setup-go/setup-node, version files, or ranges resolved to today's release. It never writes tasks.yaml. Jobs it can't express (other OSes, secrets, services, artifacts from other jobs, unknown actions) come out as review or skip with the reason. Since then: services, container jobs, composite actions, golangci-lint-action and pnpm/action-setup translations, artifacts rebuilt from the producing job (shared per commit), and `cinema setup` with skip-in-place (§11.0). Shadow mode (§11.5) replaces the planned one-off verification mode.
1. GitLab: same `cinema verify`, GitLab adapter (`vision §16`)
2. Parallel matrix for slow suites (`vision §15.5`)
3. (done differently: remote hosts are §9.3)
4. MCP server + skills for agents (`vision §31`)
5. Shims/shell integration (`vision §10.5`), PTY mode
6. Narrower per-task inputs via Nx/Turbo/Go package graphs (`vision §11.5, §12.5`), only with shadow-mode comparison first
7. Windows and macOS CI jobs, run natively on matching hosts (`vision §6.3`); Windows and macOS *hosts* are already P4
8. Team service / GitHub App with protected authority (`vision §15.6, §26`)
9. Managed/Verified modes, AI features (`vision §7, §24–30`)

Changing anything in §1, §5.1, §7.2, or §11.2 needs a written ADR in `docs/adr/`. Those sections are the false-green boundary.

---

## 17. Open questions

| Question | Default until answered |
|---|---|
| Apache-2.0 or MIT? | Apache-2.0 (patent grant) |
| Project domain for `predicateType` | Must pick before v1 receipts ship |
| Cobra vs stdlib `flag` | Cobra if subcommand count stays > 8 |
| Should the gate also verify receipts on the PR head when base moved but the change can't affect inputs? | No. Whole-source only in the MVP; P0-3 data informs later |
| Store `CINEMA_TRUSTED_KEYS` in a repo variable vs an org variable | Repo variable; org variables work identically if users want them |
| Emulated amd64 by default on Apple silicon? | **Decided: an option, off by default.** P4-1 measured Rosetta at 1.6–3.8× (cold) slower than native (cold runs). Documented with its costs; `arch: any` for CPU-independent checks |
| Scale job timeouts under emulation? | Local timeouts scale by the measured slowdown (capped at 3×); the gate's check uses the local-to-CI ratio (§11.2) |
| Shadow graduation threshold | 3 agreeing PRs, spot checks 1 in 20 |
| Which provider plugin first? | One with SSH + Docker (boat-style) first: it reuses the ssh host path |
| `pipeline:` format: Dagger modules, or cinema's own container steps? | Deferred to P5; plain `argv` stand-ins until real jobs show the need |
| Backtest depth for graduation | Last 20 merged commits, must include CI failures if any exist |
| Failed-run publishing: K consecutive passes before a flaky action_key can be signed again | K = 3 |
