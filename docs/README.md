# CInema docs

CInema runs your CI's Linux jobs before you push, from the workflows you already have, and lets CI skip what's already proven. Anything it can't prove runs in CI as before.

| Doc | Read it for |
|---|---|
| [Getting started](getting-started.md) | From an existing repo to a PR whose jobs skip themselves |
| [How it works](how-it-works.md) | Receipts, the CI check, skip-in-place, what "proven" means |
| [CI integration](ci-integration.md) | What `cinema setup` changes in your workflows, matrices, required checks, undo |
| [Tasks reference](tasks.md) | Every field of `.cinema/tasks.yaml` |
| [Machines and engines](hosts.md) | macOS, Windows and Linux hosts, container engines, emulation, remote machines |
| [Gotchas](gotchas.md) | Everything that changes a result, and how CInema handles it |
| [Security and trust](security.md) | Keys, what CI trusts, forks, credentials |
| [Commands](commands.md) | Every command and flag |

Design and decisions: [plan.md](../plan.md) (the build plan), [docs/adr](adr) (decisions on the false-green boundary), [spikes/lab/RESULTS.md](../spikes/lab/RESULTS.md) (what real projects taught us).

**Status labels.** Each doc marks what exists today and what's **planned**. Planned features are designed in the plan but not built yet; don't rely on them.
