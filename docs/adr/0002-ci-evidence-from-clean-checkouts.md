# ADR 0002: CI evidence must come from clean checkouts

- Status: accepted, 2026-10-21
- Touches: plan.md §5.2, §7, §11.2 (false-green boundary)

## Context

The lab produced a real false green. I mirrored cli/cli into a new repo. Its `.gitignore` contains `gh`, which matches `internal/gh/`, a directory upstream tracks by force-adding it. The mirror's `git add -A` left it out of the commit, but it stayed on disk in the VPS working tree, ignored.

- On the VPS, `build` and `test` passed: the files were there.
- The whole-source fingerprint skips ignored files, so the working tree and the CI checkout fingerprinted **identically**.
- The gate REUSEd `build` and `test` for a commit that doesn't compile in CI.

This isn't exotic. Ignored files that a working-tree run can read but a CI checkout won't contain include:
- force-tracked paths that `.gitignore` also matches;
- stale generated code (protobuf outputs, `dist/`, embedded assets);
- `node_modules` from an older install;
- local config and `.env` files.

Plan §5.1 already warned that `.gitignore` is not proof an ignored file doesn't matter. This case shows that relying on users to declare `extra_inputs` doesn't hold up.

## Decision

1. Receipts carry `source.capture`: `clean-checkout` or `workspace-observed`.
2. By default, `cinema push` signs only runs from a clean checkout (`cinema run --commit REV`). A clean checkout contains exactly the committed bytes, so nothing ignored can leak into the result.
3. By default, the gate rejects `workspace-observed` receipts with `WORKSPACE_EVIDENCE` and the task runs.
4. A task may opt out with `reuse: { capture: workspace }`. That's an explicit, reviewable claim, like `platform: any`, for repos where nothing ignored can affect the result. It's policy, so it isn't part of the contract digest.
5. Working-tree runs still drive local `status` (fast feedback). `status --json` reports `ci_ready` per task. `cinema push --run` runs whatever lacks clean evidence for HEAD in a clean checkout before signing.

## Consequences

- Correct by construction for the whole class: nothing ignored is present in the run.
- Costs a clean checkout per pre-push run. A shared clone plus checkout takes seconds. Tasks must include their own setup (dependency install), which is exactly what CI does. Warm local caches (Go module cache, pnpm store) keep that fast.
- It's the mode the VPS flow (§9) already used.
