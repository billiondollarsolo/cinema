# CInema — MVP Build Plan

> **Revision:** 1.0 — MVP cut, 2026-10-21
> **Status:** P0–P2 implemented (see §16 checkboxes and [spikes/lab/RESULTS.md](spikes/lab/RESULTS.md)); `cinema analyze` started (P3). Commands, schemas, and config keys are contracts.
> **Long-range design:** [vision.md](vision.md) (the full 0.2 enterprise spec, unchanged). Referenced below as `vision §N`.
> **Positioning:** Record the checks you already run. Reuse what qualifies. Run only what remains.

## 0. Decisions already made

| Decision | Choice |
|---|---|
| Language | Go. One static binary: CLI, recorder, verifier, dashboard server. |
| License | Apache-2.0. |
| Audience | Anyone with a repo — public OSS, private-for-now, private-forever. Solo devs through large companies. No paid tier is planned. |
| Trust mode | **Personal only** for the MVP: the repo owner trusts the keys they register. Managed/Verified modes are in `vision §7, §24–25`. |
| Platform reuse | No guessed equivalence. A macOS result never satisfies a Linux task. Users who need Linux AMD64 results run `cinema` on a Linux box or VPS they control (§9). |
| Dashboard | Small, local, embedded in the binary. No Node at runtime, no React stack in the MVP. Remote access goes through `tailscale serve` or an SSH tunnel, never a public bind. |
| First provider | GitHub Actions. GitLab is next (§16, P3). |
| AI | None inside CInema. Coding agents use the JSON CLI (§10.3). |

### 0.1 Why this is worth building

- Local machines and owned VPSes are usually **faster than shared hosted runners**: warm caches, more cores, no queueing.
- Developers and coding agents **already run** `go test`, `npm run lint`, and the rest before pushing. CI repeats that work.
- **Public repos:** Actions minutes are free, so the win is time to green and less queueing.
- **Private repos:** the same time win, plus real reductions in billed minutes.
- The same tool covers both. A repo going from private to public changes nothing.

### 0.2 The question P0 must answer

Even a fully reused PR still costs one runner start for the gate. The MVP is worth continuing only if, on real repos:

```text
(time of CI checks normally run) − (gate job time with everything reused) > 0, clearly
```

We also need to know the **real reuse rate** (§12). If both numbers are good, continue. If not, fix the overhead before adding features.

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
- workflow emulation (`act`-style)
- a CI interpreter
- dependency discovery for arbitrary programs
- selective test inference or narrowed input sets
- shell shims or global command interception
- filesystem watchers or a resident daemon
- PTY/interactive mode
- MCP, a skill pack, or AI features
- GitLab (next phase)
- Windows
- team server, SSO, managed devices, trusted executors, hardware attestation
- artifact promotion
- dollar-savings estimates

It must never:
- claim macOS ARM ≡ Linux AMD64
- claim a green outer command proves every nested test ran
- upload source code
- collect shell history
- rewrite workflows or branch protection without showing the change first

All of these exist in `vision.md`. Each comes back only when a real need shows up.

---

## 3. Architecture

```text
developer / agent
   └─ cinema exec -- go test ./...          (or: cinema run backend-unit)
         ├─ supervise process, keep exit code
         ├─ fingerprint source + task + environment
         └─ write run record → SQLite + blob store (per-user data dir)

cinema push
   ├─ match runs to required tasks for the commits being pushed
   ├─ sign eligible receipts (Ed25519, DSSE)
   └─ git push branch + refs/notes/cinema/v1/<key-fp>

GitHub Actions: one "cinema" job (required check)
   ├─ resolve candidate from the event (merge ref, not just PR head)
   ├─ fetch notes, verify receipts against trusted keys + task contracts
   ├─ for each required task: REUSE, or RUN inline in this job
   └─ pass only if every required task is accounted for

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
- **Clean checkout** (`cinema run --commit <sha>`): `git clone --shared --no-checkout` into a temp dir, then a detached checkout of the commit. The real repo is only read: no worktree entry, no index or ref changes. Not `git archive`, because `export-ignore`/`export-subst` attributes would change the bytes. This is the stronger mode and the one the VPS flow uses (§9).

### 5.3 Before-commit reuse

Developers test a dirty tree, then commit exactly those bytes. Since the fingerprint covers content, not commit IDs, a run on a dirty tree matches the later commit if the bytes are identical. `cinema push` recomputes the fingerprint for each pushed commit (from `git ls-tree`/blob hashes, no checkout needed) and pairs it with matching runs. The original run record is never rewritten.

Tasks that read git metadata (commit SHA, tags, `git describe`) must declare `vcs_context: exact`. Those only reuse when the commit SHA itself matches.

### 5.4 Performance

- Keep a local cache: `(path, size, mtime, inode) → digest`.
- Always re-hash anything that changed before signing.
- Measure hashing separately from wrapper overhead.
- If full-source hashing is too slow on big repos, that's a P0/P1 measurement finding, not a reason to quietly narrow inputs.

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
    timeout: 30m                              # stop a hang; a timeout is a failure (not in the contract)
    accept_tool_cache: false
    reuse: { allowed: true, max_age: 24h, capture: clean }   # capture: clean (default) | workspace (ADR 0002)
```

- `platform: any` is an explicit, reviewable claim by the repo owner. It's the main lever for reusing Mac results on lint/typecheck/format checks. Never infer it.
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
2. **Pin runtimes with `runtime: {go: 1.26.10, node: 22.23.3}`.** CInema provisions exactly that version for the task and its probes, identically on the evidence machine and in the gate. Go uses `GOTOOLCHAIN` (sumdb-verified); Node uses the official tarball checked against `SHASUMS256.txt`, cached in the data dir. Environments then match by construction instead of depending on which patch each machine has installed. Without it, `go version` and `node --version` are fingerprinted to the patch, and a `check-latest` or `lts/*` drift is `ENVIRONMENT_MISMATCH`. Runtime matrices (Node 20/22/24/26) become one task per version. `cinema exec` can't provision, so it never makes evidence for a runtime-pinned task.
3. **Probe only what isn't pinned in source.** pnpm and yarn versions come from `packageManager`, already covered by the source fingerprint. Probing them, or a tool whose version differs between dev box and runner image (git), only causes misses.
4. **Isolate tool caches per tool build.** Caches outside the repo are inputs CInema can't see (golangci-lint replayed another build's results). Point them at a per-version directory.
5. **Include setup in the task.** CI evidence comes from a clean checkout (ADR 0002): no `node_modules`, no `dist/`. `yarn install --immutable && …` belongs in the argv, and warm local caches keep it fast.
6. **Declare outputs a task writes into the tree** if they aren't ignored (`cover-profile.out`, a build dir that deletes tracked files).
7. **Keep never-reusable work in its original parallel jobs** (time-boxed fuzzing, live-network checks). Inside the single gate job it only adds serial time.
8. **Run evidence as a non-root user** when CI is non-root (GitHub runners). Root changes test results.
9. **Make `.cinema/tasks.yaml` follow the repo's formatter,** or list it in the formatter's ignore file. Repo-wide formatters and linters will touch it.

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
    "source": {"digest": "sha256:…", "observed_commit": "<sha or null>", "capture": "workspace-observed"},
    "environment_digest": "sha256:…",
    "platform": "linux/amd64",
    "result": {"status": "passed", "exit_code": 0, "origin": "fresh"},
    "timing": {"started_at": "…", "duration_ms": 42000, "time_authority": "local-clock"},
    "executor": {"kind": "cinema-cli", "version": "0.1.0"}
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
- Only passed runs get receipts. Failed runs stay local history.

### 7.3 Keys

- `cinema keys init` generates an Ed25519 key, stored in the OS keychain or an encrypted file with 0600 permissions (with a warning).
- One key per machine: laptop, VPS, desktop. Each gets its own fingerprint, so per-machine trust and revocation are trivial.
- `cinema keys export` prints only the public key.
- **Trusted public keys live in a GitHub repository variable** (`vars.CINEMA_TRUSTED_KEYS`), **not in the repo tree.** A PR can edit files in the tree. It can't edit repo variables.
- Revocation: remove the key from the variable. The next gate run stops accepting its receipts.

---

## 8. Transport: git notes

```text
refs/notes/cinema/v1/<key-fingerprint>
```

- One notes ref per signing key, so machines never fight over the same ref.
- Each note is attached to the commit it covers. Its content is a compact JSON array of DSSE envelopes for that commit (cap: 256 KiB per note, 100 receipts per candidate).
- No logs in notes, ever.

### 8.1 `cinema push`

1. Resolve the commits being pushed (`git rev-list <remote>..<local>`).
2. For each, compute the source fingerprint and find matching passed runs.
3. Sign receipts and write notes.
4. `git push --atomic <remote> <branch> refs/notes/cinema/v1/<fp>`, falling back to notes-first-then-branch if the remote rejects `--atomic`.
5. Report branch push and evidence push **separately**. Evidence failing never blocks the source push.

A `pre-push` hook can't add refs to an in-flight push, so `cinema push` is the supported path. An optional hook only prints a hint.

### 8.2 Forks

Fork contributors can't push notes to upstream, and their keys aren't trusted anyway. **Default: fork PRs get full CI.** This is the correct behavior for open-source repos, not a limitation to work around. Never use `pull_request_target` to read fork evidence.

---

## 9. Linux AMD64 results from a box you control

The Mac-vs-CI platform gap is solved by running `cinema` where the platform matches, not by pretending.

```bash
# on the VPS (once)
cinema keys init                   # this machine gets its own key
cinema keys export                 # add to vars.CINEMA_TRUSTED_KEYS

# from the laptop, per push (MVP: plain ssh)
git push origin feature/x
ssh vps 'cd ~/src/repo && git fetch -q && cinema run --all --commit origin/feature/x && cinema push --notes-only'
```

- `--commit` runs in a clean checkout (§5.2), so the VPS result is bound to exactly the pushed bytes.
- Order matters: the branch push triggers CI immediately, possibly before VPS evidence lands. The gate therefore **waits briefly** for notes (§11.3).
- Later convenience: `cinema remote run <host>` wraps the ssh call. Not in the MVP. The ssh one-liner is documented and good enough.

### 9.1 Reaching the dashboard on the VPS

`cinema dashboard` binds `127.0.0.1` only. Remote access:

```bash
# on the VPS, Tailscale terminates TLS and limits access to your tailnet
tailscale serve --bg 7777
cinema dashboard --port 7777 --allow-host vps.your-tailnet.ts.net

# or
ssh -L 7777:127.0.0.1:7777 vps
```

`--allow-host` adds a name to the Host-header allowlist. The dashboard never binds a non-loopback address in the MVP.

---

## 10. CLI and dashboard

### 10.1 Commands (MVP only)

```text
cinema init                     write .cinema/tasks.yaml skeleton; print the workflow to add
cinema exec -- CMD ARGS...      record an arbitrary command
cinema run <task>|--all [--commit SHA]
cinema status [--json]          per task: last run, eligible / stale / missing + reason
cinema explain <task>           why a task is or isn't reusable for HEAD
cinema push [--notes-only]      sign + publish evidence (+ branch)
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

- **Branch protection requires the `cinema` job only.** Add the existing workflows' real checks as `cinema` tasks; mark expensive ones reusable and keep never-reusable ones (`reuse.allowed: false`) running inline or in their original workflows.
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
    expected = action_key(task, source_digest, CI environment for task.platform)
    for receipt in notes on candidate SHA, PR head SHA, and each PR commit (bounded):
        verify DSSE signature with trusted keys      else reject SIGNATURE_INVALID / SIGNER_NOT_ALLOWED
        strict-parse predicate                        else reject MALFORMED
        repository_id matches                         else reject WRONG_REPOSITORY
        subject digest == expected                    else reject (INPUTS_CHANGED | TASK_DEFINITION_CHANGED | ENVIRONMENT_MISMATCH)
        age <= max_age                                else reject RECEIPT_EXPIRED
        result passed, origin acceptable              else reject
        → REUSE; break
    else RUN (NO_RECEIPT or last rejection reason)

--execute: run every RUN task's exact argv; record pass/fail
--gate:    pass iff every task is REUSE or RUN+passed; write gate-report.json
```

Key consequences:
- **Merge commit, not PR head.** The source digest is computed from the tree CI checked out. A PR-head receipt matches only if the merge result has byte-identical inputs, meaning the base hasn't changed in ways that touch the tree. When main moves, tasks rerun. That's correct, not a bug.
- **Task list from the base branch.** A PR that edits `.cinema/tasks.yaml` gets its new tasks evaluated as RUN, with no reuse for changed contracts.
- **Personal-mode honesty:** a PR from a collaborator with write access can still edit the workflow file itself. That's true of every Actions check, and maintainers review `.github/` and `.cinema/` changes (use CODEOWNERS). Protected authority via a GitHub App is `vision §15.6`.

### 11.3 Evidence arriving late

If the candidate has zero receipts and the trusted keys include a "remote runner" key (§9), the gate may poll `git fetch` for notes for up to `CINEMA_WAIT` (default 0, max 120 s) before falling back to RUN. The wait is bounded, and timing out simply means the tasks run.

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

## 12. Reason codes (MVP set)

```text
NO_RECEIPT              INPUTS_CHANGED           TASK_DEFINITION_CHANGED
ENVIRONMENT_MISMATCH    WRONG_REPOSITORY         SIGNATURE_INVALID
SIGNER_NOT_ALLOWED      MALFORMED                RECEIPT_EXPIRED
PARTIAL_COVERAGE        SOURCE_CHANGED_DURING_RUN TOOL_CACHE_REJECTED
REUSE_DISABLED          FORK_PR                  POLICY_UNAVAILABLE
POLICY_INVALID          TRANSPORT_UNAVAILABLE    RUN_FAILED
VCS_MISMATCH
```

`VCS_MISMATCH`: a `vcs_context: exact` task whose run was on a different commit, or on a dirty tree.

A failed run on the exact same inputs (contract, environment, source) blocks reuse of any earlier pass. Reusing the pass would hide a flaky or broken check.

CLI, JSON, dashboard, and gate report all use the same codes. Human text is presentation only. Full list in `vision §9.5`.

**Reuse-rate metric:** `REUSE / (REUSE + RUN)` per task over real PRs, broken down by reason code. If `ENVIRONMENT_MISMATCH` dominates, users need `platform: any` on more tasks or a Linux box (§9). If `INPUTS_CHANGED` dominates after a merge from main, that's the whole-source cost showing up, and it informs whether narrower inputs (`vision §11.5`) are worth the risk.

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
| Partial blob/DB writes | Temp file + rename. On startup, sweep `*.tmp` older than 1 h in the data dir. |
| Provisioned runtimes (`runtimes/<lang>/<version>`) | Installed atomically (temp dir + rename). The daily pass removes versions unused for 90 days and interrupted installs older than 1 h. |
| Fingerprint caches (`fscache/<repo>.gob`) | One file per repo, rewritten on use. The daily pass deletes caches unused for 90 days, and temp files older than 1 h. |
| Old runs and logs | §13 retention. Applied by any command, at most once per 24 h (timestamp in `state.db`); one indexed `DELETE` plus blob unlink, under 50 ms. Blobs no longer referenced by any row are deleted in the same pass. `cinema gc` forces it. |
| Data dir size | Hard cap (default 2 GiB). Past it, delete oldest passing logs first, then failing logs. Run metadata goes last. |
| Notes, local | `git notes prune` on our refs during the daily pass (notes for commits that no longer exist). |
| Notes, remote | Per-key refs only grow by one commit per push. `cinema push` squashes our own ref to a single commit holding only notes for reachable commits newer than `max_age`, and force-pushes it with `--force-with-lease`. Safe because only this key writes this ref. |
| Dashboard | Nothing persisted: token and sessions live in memory. Runs started from the page get SIGTERM on shutdown, and the server waits for their cleanup. |
| CI artifacts | `gate-report.json` uploads with `retention-days: 7`. |
| Everything | `cinema uninstall [--remote]` removes the data dir, keys (keychain + file), local notes refs, remote notes refs (with `--remote`), and prints the workflow / branch-protection lines to remove. It never edits workflows itself. |

Inside the repo, CInema only ever creates `.cinema/tasks.yaml`, and only via `cinema init`.

Tests: CLEAN-001 leftover child stopped (polite + SIGTERM-ignoring). CLEAN-002 `run --commit` interrupted by SIGINT leaves no temp dir. CLEAN-003 crash mid-blob-write: next start sweeps the `.tmp`. CLEAN-004 retention removes expired rows and their blobs, keeps blobs still referenced. CLEAN-005 size cap evicts passing logs first. CLEAN-006 `uninstall` leaves no data dir, key, or local notes ref. CLEAN-007 squashed notes ref still verifies every receipt within `max_age`.

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
- [x] `cinema dashboard`: one page, token auth, Host/Origin checks, SSE refresh, `--allow-host` (§10.4, §9.1); Run buttons (POST + Origin + CSRF)
- [x] `cinema init`: tasks skeleton from detected `go.mod` / `package.json` (npm/pnpm/yarn; never writes `platform: any`; prints the AGENTS.md paragraph)
- [ ] Tests: ACC-001…021, 047, 048, 051, 066; parser fuzzers

**Exit:** you use it daily on your own repos for a week. `status` is never wrong about stale vs reusable.

### P2 — Signed evidence + GitHub gate (the MVP)

- [ ] `cinema keys` (OS keychain + file fallback) (§7.3)
- [ ] DSSE + in-toto statement build/sign/verify with fixed test vectors; strict parser (§7)
- [ ] Git notes publish/fetch, per-key refs, size caps (§8)
- [ ] `cinema push` with atomic + fallback, separate reporting (§8.1)
- [ ] `cinema verify`: candidate resolution, base-branch tasks, fork rule, REUSE/RUN, `--execute`, `--gate`, `gate-report.json`, step outputs for conditional toolchain setup (§11)
- [ ] Late-evidence wait (§11.3)
- [ ] Release binaries (linux/darwin × amd64/arm64) with checksums; `cinema init` prints the pinned workflow
- [ ] VPS flow documented and tested end-to-end (§9)
- [ ] Dashboard: evidence + CI panels
- [ ] Tests: all §14 rows, property tests
- [ ] README: install, register key, first reused PR, **how to remove CInema and go back to normal CI**

**Exit (MVP demo):** on a real repo,
1. a PR whose checks ran locally gets a green `cinema` check with the expensive tasks reused, verified by the gate report;
2. a byte-flipped receipt, an untrusted key, a changed input, missing notes, and a failing inline task each produce RUN or a failing gate, never a false green.

### P3+ — Next, in rough order (details in vision.md)

0. **Started:** `cinema analyze` reads GitHub workflows and drafts tasks per Linux job and matrix combination. It handles the run steps (one subshell per step, like Actions), literal env, matrix substitution, a matrix-only subset of `if:`, and exact runtimes from setup-go/setup-node, version files, or ranges resolved to today's release. It never writes tasks.yaml. Jobs it can't express (other OSes, secrets, services, artifacts from other jobs, unknown actions) come out as review or skip with the reason. Next: run each draft in a clean checkout and compare with the same commit's CI result (a verification mode), and translations for common actions (golangci-lint-action, pnpm setup).
1. GitLab: same `cinema verify`, GitLab adapter (`vision §16`)
2. Parallel matrix for slow suites (`vision §15.5`)
3. `cinema remote run <host>` wrapper for the VPS flow
4. MCP server + skills for agents (`vision §31`)
5. Shims/shell integration (`vision §10.5`), PTY mode
6. Narrower per-task inputs via Nx/Turbo/Go package graphs (`vision §11.5, §12.5`), only with shadow-mode comparison first
7. Windows (`vision §6.3`)
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
