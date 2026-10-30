# Security and trust

## What CI trusts

- **Keys you register, nothing else.** `CINEMA_TRUSTED_KEYS` is a repository variable: one public key per line, one line per machine. A PR can change files in the repo, not repository variables, so it can't add a key.
- **Tasks from the base branch.** A PR that edits `.cinema/tasks.yaml` can't relax its own requirements; its changes count after merge.
- **Exact matches only.** A receipt counts only for this repository, this task contract, this source fingerprint (of the tree CI checked out), this environment, within `max_age`, signed by a trusted key. The parser rejects duplicate JSON keys, unknown fields, wrong types and payloads over 64 KiB. Signatures are checked over the exact signed bytes.

## What a receipt proves, and what it doesn't

A signature says **who claimed** a result, not that the work happened. CInema's trust model is personal: you trust the machines you register, as you trust their owners to push code. Anyone with write access can also edit workflow files, as with any Actions check; review `.github/` and `.cinema/` changes (CODEOWNERS helps).

## Keys

- `cinema keys init` creates an Ed25519 key for this machine (today a file with 0600 permissions in the data dir; OS keychain storage is planned). `cinema keys export` prints the public line.
- One key per machine. Revoking a machine is deleting its line from `CINEMA_TRUSTED_KEYS`; its receipts stop counting on the next CI run. Offboarding a person is deleting their machines' lines.
- **Planned:** short-lived, labeled keys for coding agents that work in remote sandboxes (`--expires 7d --label agent:<name>`).

## Forks

Fork PRs get full CI. Fork contributors can't push notes to your repo, and their keys aren't trusted. CInema never uses `pull_request_target` to read fork evidence, and `setup` leaves workflows triggered by it alone. **Planned (P5):** a maintainer can run a reviewed fork PR's jobs on their own machine and vouch for it with their key.

## The `cinema` job's permissions

It needs to read the repository and its notes. It uses the job's own `github.token` to fetch notes (`CINEMA_FETCH_TOKEN`), and the checkout step uses `persist-credentials: false`. Its actions are pinned by commit SHA.

## Running CI on your machine

Running your CI locally runs its code: your project's tests and install scripts, and anything they download.

- **Today**, tasks run as normal processes in clean checkouts, as your user, with a private `HOME` and `TMPDIR` per run. They can reach whatever your user can.
- **Planned (P4)**, tasks run in containers, which become the boundary: no host environment, SSH agent, `~/.ssh`, cloud credentials, `~/.npmrc` or Docker logins inside, unless a task declares one (`credentials: [npm]`), and no host Docker socket without `docker: host`. CInema's own engine calls use an empty Docker client config, so your registry logins never reach a job's image pulls.
- `cinema analyze` inlines composite actions only when they're pinned to a commit, never by tag: tags can be repointed (the 2025 `tj-actions/changed-files` compromise did exactly that).

## Trusting a tasks.yaml locally

`cinema trust` approves the toolchain commands of one exact `tasks.yaml` (by digest). Until then, CInema won't run its probe commands or resolve unpinned images. Any edit needs `trust` again.

## What gets published

Git notes hold receipts only: task id, digests, timing, result, platform. Never logs, never environment values (only names and digests). **Planned:** receipts name only the kind of host (`local`, `ssh`, a provider), never a hostname.

## Reporting a vulnerability

Open a private security advisory on the repository.
