# ADR 0001: Exclude `.cinema/tasks.yaml` from the source fingerprint

- Status: accepted, 2026-10-21
- Touches: plan.md §5.1 (false-green boundary)

## Context

The whole-source fingerprint covers every tracked and untracked non-ignored file, including `.cinema/tasks.yaml`. Editing any part of that file, even another task's definition or a `max_age`, then changed `source_digest` for every task. All recorded runs went stale with `INPUTS_CHANGED`. That contradicts §6.2: acceptance policy must not change what the work was.

## Decision

`snapshot.Take` leaves out exactly one path: `.cinema/tasks.yaml`.

## Why this cannot produce a false green

- Everything in the file that affects a task's work is already in that task's **contract digest**: argv, cwd, platform, toolchain, env names, extra inputs, vcs mode and tool-cache setting. Changing any of them still invalidates that task's runs, via `TASK_DEFINITION_CHANGED`.
- What's left out is other tasks' definitions and acceptance policy (`reuse.allowed`, `max_age`). Neither changes the bytes a task's command sees.
- In CI, the gate reads tasks from the **base** branch (§11.2), so a PR can't loosen policy by editing the file either way.
- Residual risk: a task whose command reads `.cinema/tasks.yaml` itself. No real task does this. If one ever does, it can list the file in `extra_inputs`: declared extra inputs are always fingerprinted, including this path.

## Consequences

Editing one task, or tightening a policy, no longer forces every other task to rerun.
