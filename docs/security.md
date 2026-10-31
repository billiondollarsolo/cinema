# Security and trust

## What CI trusts

- **Keys you register, nothing else.** On GitHub, `CINEMA_TRUSTED_KEYS` is a repository variable: one public key per line, one line per machine. A PR can change files in the repo, not repository variables, so it can't add a key.
- **On GitLab, the keys are a reviewed file.** A pipeline variable (`git push -o ci.variable=…`, Run pipeline, the API) outranks any CI/CD variable, so a Developer could pass their own keys without a trace in any diff. The cinema job reads `.cinema/trusted-keys` from the tip of the branch `cinema setup` wrote into `.gitlab-ci.yml` (`--trust-branch`), never from the environment, and takes tasks.yaml only from a merge request target that is on that branch. Protect the branch and review `.cinema/`. Also set "Minimum role to use pipeline variables" to `no_one_allowed` or Maintainer: anyone who sets the cinema job's variables (`GOPROXY`, `GIT_CONFIG_*`…) controls the job itself, whatever it reads ([docs/gitlab.md](gitlab.md#trusted-keys-a-reviewed-file-not-a-variable)).
- **Tasks from the base branch.** A PR that edits `.cinema/tasks.yaml` can't relax its own requirements; its changes count after merge.
- **Exact matches only.** A receipt counts only for this repository, this task contract, this source fingerprint (of the tree CI checked out), this environment, within `max_age`, signed by a trusted key. The parser rejects duplicate JSON keys, unknown fields, wrong types and payloads over 64 KiB. Signatures are checked over the exact signed bytes.

## What a receipt proves, and what it doesn't

A signature says **who claimed** a result, not that the work happened. CInema's trust model is personal: you trust the machines you register, as you trust their owners to push code. Anyone with write access can also edit workflow files, as with any Actions check; review `.github/` and `.cinema/` changes (CODEOWNERS helps).

## Keys

- `cinema keys init` creates an Ed25519 key for this machine (today a file with 0600 permissions in the data dir; OS keychain storage is planned). `cinema keys export` prints the public line.
- One key per machine. Revoking a machine is deleting its line from `CINEMA_TRUSTED_KEYS` (GitLab: `.cinema/trusted-keys` on the trust branch); its receipts stop counting on the next CI run. Offboarding a person is deleting their machines' lines.
- **Planned:** short-lived, labeled keys for coding agents that work in remote sandboxes (`--expires 7d --label agent:<name>`).

## Forks

Fork PRs get full CI. Fork contributors can't push notes to your repo, and their keys aren't trusted. CInema never uses `pull_request_target` to read fork evidence, and `setup` leaves workflows triggered by it alone. **Planned (P5):** a maintainer can run a reviewed fork PR's jobs on their own machine and vouch for it with their key.

## The `cinema` job's permissions

It needs to read the repository and its notes. It uses the job's own `github.token` to fetch notes (`CINEMA_FETCH_TOKEN`), and the checkout step uses `persist-credentials: false`. Its actions are pinned by commit SHA.

## Running CI on your machine

Running your CI locally runs its code: your project's tests and install scripts, and anything they download.

- **Today**, tasks run as normal processes in clean checkouts, as your user, with a private `HOME` and `TMPDIR` per run. They can reach whatever your user can.
- **Planned (P4)**, tasks run in containers, which become the boundary: no host environment, SSH agent, `~/.ssh`, cloud credentials, `~/.npmrc` or Docker logins inside, unless a task declares one (`credentials: [npm]`), and no host Docker socket without `docker: host`. CInema's own engine calls use an empty Docker client config, so your registry logins never reach a job's image pulls.
- **Caches are shared between runs of the same repo** (Go modules and build cache, npm and other package caches, as named volumes). That's what keeps local runs fast, and it's a trust boundary: a run of hostile code could plant files that a later run picks up. In CInema's personal trust model you run your own code, so the risk is the same as your own `~/go` and `~/.npm` today. **Planned:** runs of code you didn't write (the maintainer-runs-a-fork flow) always get fresh caches.
- **Env input values never appear in a command line or the run record:** containers get allowlisted variables by name, and the run history records the task's own command.
- `cinema analyze` inlines composite actions only when they're pinned to a commit, never by tag: tags can be repointed (the 2025 `tj-actions/changed-files` compromise did exactly that).
- **The CI config is untrusted input to the analyzer** (and to the gate, which analyzes it in CI). Files it names (version files, mise and asdf configs, `global.json`, local GitLab includes, reusable workflows) are read only inside the repository: through `os.Root`, so neither `..` nor a symlinked directory leads out, regular files only (a FIFO can't hang it), at most 1 MiB. GitLab's `!reference`, `extends` and variable expansion share one work budget: a billion-laughs config sends every job to review instead of exhausting memory.

## Trusting a tasks.yaml locally

`cinema trust` approves the toolchain commands of one exact `tasks.yaml` (by digest). Until then, CInema won't run its probe commands or resolve unpinned images. Any edit needs `trust` again.

## What gets published

Git notes hold receipts only: task id, digests, timing, result, platform. Never logs, never environment values (only names and digests). **Planned:** receipts name only the kind of host (`local`, `ssh`, a provider), never a hostname.

## Reporting a vulnerability

Open a private security advisory on the repository.
