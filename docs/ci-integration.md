# CI integration

`cinema setup` edits your workflows so covered jobs skip themselves. This page is exactly what it changes, and what it won't touch.

## What setup writes

For each workflow with jobs this machine can take over:

**A `cinema` job,** before the first job:

```yaml
  cinema:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    outputs:
      skip_test: ${{ steps.verify.outputs.skip_test }}
      exclude_build: ${{ steps.verify.outputs.exclude_build }}
    steps:
      - uses: actions/checkout@<pinned sha> # v7.0.1
        with: { fetch-depth: 0, persist-credentials: false }
      - name: install cinema
        run: |
          go install github.com/billiondollarsolo/cinema/cmd/cinema@<version>
          sudo mv "$(go env GOPATH)/bin/cinema" /usr/local/bin/cinema
      - id: verify
        name: skip jobs that already passed (signed cinema receipts)
        env:
          CINEMA_TRUSTED_KEYS: "${{ vars.CINEMA_TRUSTED_KEYS }}"
          CINEMA_FETCH_TOKEN: "${{ github.token }}"
        run: cinema verify --event "$GITHUB_EVENT_PATH" --skip-outputs ci.yml
```

**On each covered job that runs whole:**

```yaml
  test:
    needs: cinema
    if: "${{ !cancelled() && needs.cinema.outputs.skip_test != 'true' && (<the job's own if>) }}"
```

If the job had `needs:`, `cinema` is added to them and the condition also requires those jobs to have succeeded, as `needs:` would without `!cancelled()`.

**On a matrix job some of whose legs are covered:**

```yaml
    strategy:
      matrix:
        os: [ubuntu-latest, windows-latest, macos-latest]
        exclude: ${{ fromJSON(needs.cinema.outputs.exclude_build || '[]') }}
```

Reused legs are dropped; the others (here Windows and macOS) run as before. When every leg is covered and reused, the whole job is skipped instead.

Everything else in the file stays byte-for-byte. `setup` parses its own output back and checks every edit; if anything is off, it leaves that workflow alone and says why.

## What it leaves alone

`setup` says "left as is" with the reason for:

- workflows triggered by `pull_request_target` (they check out the base, not the PR);
- workflows that already have a job named `cinema`;
- jobs other jobs `need` (skipping them would skip their dependents);
- `if:` conditions that use status functions (`always()`, `failure()`) or span several lines;
- matrices with object values, non-string values, their own `exclude:`, or an `include:` entry that extends one of the plain legs (excluding that leg would also drop the include).

## Required status checks

- A job skipped through `if:` reports its check as **passed**. Required checks on whole jobs keep working.
- An **excluded matrix leg reports no check at all**, and a matrix job skipped whole reports one check under the job's name. If branch protection requires a leg by name (`build (ubuntu-latest)`), the PR could never merge.
- So `setup` reads the default branch's required checks (rulesets, and classic branch protection when you're an admin) through `gh`, and leaves any required leg in CI. If it can't read them, it prints a note instead: require the job's other checks rather than its legs.

## Things that surprise people

- **The PR that adds CInema runs everything.** CI reads tasks from the base branch. Jobs start skipping after that PR merges.
- **Editing a workflow job makes it run.** Each task records the job it was drafted from (`job_digest`). Change the job's steps and the task no longer matches until you rerun `cinema setup`.
- **Merging main into your branch usually makes jobs run.** CI tests the merge commit. If main changed any file, the source fingerprint differs from your receipt's. (Planned: `cinema push` will also test the merge result, §5.5 of the plan.)
- **`include:`-only matrix legs can't be excluded.** GitHub applies `exclude` before `include`, so a leg added by `include:` always runs unless the whole job skips.
- **The `cinema` job costs about a minute** of runner time per workflow, also when nothing is reused.
- **Its install step needs a published CInema.** `setup` pins the version of the binary that wrote it; an untagged build pins `@latest` and says so.

## Undo

```sh
cinema setup --undo
```

Removes the `cinema` job (only the one setup generated, recognized by its `cinema verify --skip-outputs` step), `needs: cinema`, the skip conditions (restoring each job's own `if:`) and the matrix excludes. `.cinema/tasks.yaml` stays. Rerunning `cinema setup` always starts from the original workflows, so it's safe to run again after editing them.

## The older gate forms

Before skip-in-place, CInema used a separate gate workflow (one job, or a matrix job per task, that verified receipts and ran the rest itself with `cinema verify --execute`). It still works for repos that want a single required check, but it doesn't reduce what the original workflows bill, and its `--execute` runs tasks on the bare runner. Examples are in [`spikes/lab/workflows`](../spikes/lab/workflows).
