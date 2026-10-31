# GitLab CI

CInema reads `.gitlab-ci.yml` the same way it reads GitHub Actions workflows. It drafts each Linux job as a task, runs the task locally in the job's own image, and signs a receipt into git notes. In the pipeline, a `cinema` job then writes a **child pipeline** that holds only the covered jobs that still need to run. A job proven by a signed local run never gets a runner. If CInema can't tell whether a job is proven, the job runs.

**Status:**
- Built and tested (unit and end-to-end tests against a local bare remote): the analyzer, the child-pipeline generator, `cinema setup` and `setup --undo` for GitLab, and the gate's view of GitLab pipelines.
- Checked against a local GitLab CE, not a real pipeline run: notes refs push and fetch; GitLab's CI lint with a dry-run pipeline accepted setup's parent and the generated children.
- Not run on gitlab.com yet. The [live spike list](#not-yet--needs-a-live-spike) says what that run has to confirm.

## What works

- **Jobs, as GitLab resolves them.** CInema applies the config in the same order GitLab does:
  - local `include:` (file lists and `*` / `**` globs) are merged in, and the including file wins;
  - `extends:` is resolved, multi-level, with deep merge, and the job wins;
  - `!reference` tags are replaced (nested script lists are flattened);
  - `default:` and the legacy top-level `image:` / `services:` / `before_script:` are applied, honoring `inherit:`;
  - global and job `variables:` are combined, with the job winning;
  - YAML anchors and `<<` merge keys are resolved.

  Hidden jobs (`.name`) are templates, not jobs.
- **The environment.** A job's `image:` makes it a container task in that image. The image digest is part of the environment fingerprint, so a moved tag is a different environment. `services:` become task services. They are named by `alias:`, or by the image name as GitLab derives it, and get every job variable, as on GitLab.
- **The script.** `before_script` and `script` run in one shell, as the runner runs them: bash with errexit and pipefail where the image has bash, `sh -e` where it doesn't (alpine, busybox). `after_script` is left out, because on GitLab it doesn't change the job's result.
- **Predefined variables.** The task exports what a run can honestly set:
  - `CI`, `GITLAB_CI`, `CI_PROJECT_DIR`, `CI_JOB_NAME`, `CI_JOB_NAME_SLUG`, `CI_JOB_STAGE`;
  - `CI_NODE_INDEX` and `CI_NODE_TOTAL` for parallel jobs;
  - `CI_DEFAULT_BRANCH` and `CI_PROJECT_PATH` / `_NAMESPACE` / `_NAME`, taken from `origin`.

  `$VAR` references between variables are expanded the way GitLab expands them (in any order; `$$` is a literal `$`; `expand: false` respected).
- **Legs.** `parallel: N` gives N legs (`test 2/3`). `parallel: matrix:` gives one leg per combination (`test: [ruby, 3.3]`), with the matrix variables set.
- **When a job runs.** CInema evaluates `rules:` (`if:` with `==`, `!=`, `=~`, `!~`, `&&`, `||`, parentheses and `null`, plus `exists:` for literal paths, `when:`, `allow_failure:` and `variables:`), `only:` / `except:` (refs keywords and `variables:`) and `workflow: rules:`. It checks two pipelines:
  - a merge request pipeline;
  - a push to a feature branch with no open merge request.

  A job that runs in neither (tags, schedules, the default branch only) isn't a check. A job whose rules give it different variables in the two pipelines, or whose script reads `$CI_PIPELINE_SOURCE` or other branch/MR variables, gets `event:` (receipts count for that event only).
- **Runners.** A job with no tags on gitlab.com runs on `saas-linux-small-amd64` (2 vCPU, 8 GB). The `saas-linux-*-amd64` tags map to their sizes, and the task is drafted with that `cpus:` / `memory:`.
- **Timeouts.** `timeout:` in GitLab's human form (`1h 30m`, `90 minutes`) becomes the task's timeout. The default is 60 minutes.
- **Outputs.** `artifacts: paths:` become `outputs:`.

## What goes to review, and what stays in CI

Each of these sends the job to review: a person must look before the task counts.

- An include cinema can't read: `remote`, `project`, `template`, `component`, a conditional (`include: rules:`) or one with `inputs:`. Every job goes to review, because such a file can change any job.
- Rules cinema can't decide:
  - `changes:`;
  - `exists:` with wildcards;
  - a comparison with a branch name or regex it can't know;
  - a variable from the project's CI/CD settings.
- `when: delayed`, `allow_failure: exit_codes`, a `resource_group`, `hooks:`.
- `needs:` with artifacts, non-empty `dependencies:`, or (with neither) artifacts from jobs in earlier stages, which GitLab hands to every later job by default. Add `dependencies: []` to jobs that don't use them.
- No `image:` (the runner's default image isn't known), an image or tag with variables cinema can't resolve, an `entrypoint:` other than `[""]`, Docker-in-Docker, `GIT_STRATEGY: none`, submodules.
- Runner tags cinema doesn't know (self-managed runners): set `cpus:` and `memory:` in the task.
- A script that reads a predefined variable a local run doesn't have (`$CI_COMMIT_SHA`, `$CI_COMMIT_REF_NAME`, `$CI_MERGE_REQUEST_IID`…), or a variable `.gitlab-ci.yml` doesn't define (from the project's settings).

These jobs stay in CI and are never drafted:

- `trigger:` jobs.
- `environment:` (deploys), `release:`, `pages`.
- `id_tokens:` or `secrets:`.
- `when: manual` and `when: on_failure` jobs.
- macOS, Windows, GPU and ARM runners.
- Scripts that publish (`npm publish`, `docker push`, `release-cli`, uploads to GitLab's package registry).
- Scripts that use `$CI_JOB_TOKEN`, registry credentials, or a variable whose name says it's a secret.

The job digest (`job_digest:`) covers the job's whole resolved definition: the job, every template it extends, `default:`, global variables, included files and `workflow:`. Changing any of them makes it a new job, which runs and goes back to shadow (plan §11.5).

## How skipping works: a child pipeline

GitLab fixes `rules:` when it creates a pipeline, so a rule can't read receipts. `cinema setup` changes `.gitlab-ci.yml` like this:

1. Each covered job's key gets the prefix `.cinema:` (`unit:` → `.cinema:unit:`). That turns it into a hidden job, a template that doesn't run in the parent pipeline. Nothing else in the file changes.
2. Two jobs are appended, between `# cinema: begin` and `# cinema: end`. Their `rules: [{when: on_success}]` is written only when the project already runs jobs in merge request pipelines. Otherwise, like the project's own jobs, they run in branch pipelines only, so they never create merge request pipelines the project didn't have:

```yaml
cinema:
  stage: <your first stage>
  image: golang:1.26
  needs: []
  inherit: {default: false, variables: false}
  rules: [{when: on_success}]
  variables: {GIT_DEPTH: "0"}
  timeout: 10m
  script:
    - go install github.com/billiondollarsolo/cinema/cmd/cinema@<version>
    - export PATH="$(go env GOPATH)/bin:$PATH"
    - cinema verify --gitlab-child .cinema-child.yml
  artifacts:
    paths: [.cinema-child.yml]
    expire_in: 1 week
cinema:child:
  stage: <your last stage>
  needs: [cinema]
  rules: [{when: on_success}]
  trigger:
    include:
      - artifact: .cinema-child.yml
        job: cinema
    strategy: depend
    forward: {yaml_variables: false, pipeline_variables: true}
```

`cinema verify --gitlab-child` judges the receipts exactly as the GitHub gate does: base-branch tasks, trusted keys, fingerprints, graduation and spot checks. It then writes a child pipeline that:

- includes your `.gitlab-ci.yml`, so GitLab resolves includes, `extends`, `!reference`, `default:` and variables exactly as for the parent;
- switches every job of it off;
- turns each hidden job that still needs to run back on, as `<job>: {extends: .cinema:<job>}`. The job's rules were decided in the parent, because in a child pipeline `$CI_PIPELINE_SOURCE` is `parent_pipeline`. A matrix job lists only its legs that weren't reused;
- holds one tiny `cinema:proven` job if nothing else runs, because GitLab refuses to create an empty child pipeline.

The design choices:

- **`strategy: depend` makes the trigger job mirror the child's result**, so "Pipelines must succeed" sees the covered jobs' failures. GitLab 18.2+ also has `strategy: mirror`.
- **The trigger is in your last stage with `needs: [cinema]`.** It starts as soon as the cinema job finishes, and no other job waits for the child.
- **`forward: yaml_variables: false`.** Forwarded YAML variables would become *pipeline* variables in the child, which outrank job variables and would change what jobs see. `pipeline_variables: true` passes on what the parent got from a manual or API run, as the parent's jobs would have seen it.
- **The trusted keys aren't read in the child.** All verification happens in the parent.

The trade-off: a covered job that still has to run starts after the cinema job (about a minute: image pull, `go install`, verify), and it shows inside the child pipeline instead of in the parent's graph.

Setup leaves a job as it is (not covered) when:
- other jobs use it by name (`extends`, `needs`, `dependencies`, `!reference`): hiding it would break them;
- it has `needs:` or `dependencies:` of its own (a child pipeline can't wait for the parent's jobs);
- it's defined in an included file;
- it runs on other runner tags than the other covered jobs. The cinema job measures its own runner, so it uses the covered jobs' tags.

Setup refuses the whole file when the config has includes cinema can't read.

`cinema setup --undo` removes the appended block and gives the hidden jobs their names back. The file comes back byte-for-byte.

## Setting up

1. `cinema setup` on a repo with `.gitlab-ci.yml`. It shows what changes and asks. Review `.cinema/tasks.yaml`, then `cinema trust`.
2. `cinema run --commit HEAD --all`, then `cinema keys init && cinema keys export` once per machine.
3. Add the exported lines as a project CI/CD variable `CINEMA_TRUSTED_KEYS` (Settings → CI/CD → Variables):
   - **Visibility: Visible.** A key line has spaces, so it can't be masked, and it isn't secret.
   - **Not protected.** Merge request pipelines don't get protected variables.

   Only Maintainers can edit project variables, which is the protection that matters.
4. Set **Settings → CI/CD → Variables → Minimum role to use pipeline variables** to `no_one_allowed` or Maintainer. Pipeline variables (Run pipeline, the API, `git push -o ci.variable=…`) outrank project variables, so without this setting a Developer could pass their own `CINEMA_TRUSTED_KEYS`. gitlab.com's default for new projects is `no_one_allowed`. Self-managed instances default to Maintainer.
5. Commit, then `cinema push` pushes the branch and `refs/notes/cinema/v1/<key>` together. GitLab accepts pushes to `refs/notes/*` from any Developer: notes aren't branches or tags, so protected-branch and push rules don't apply to them.
6. Jobs skip once the setup is on the default branch and graduated (`cinema graduate`, which reads CI's history with `glab api`).

Which pipelines can skip:
- **Merge request pipelines** (Free): judged against the merge request's head commit.
- **Merged results pipelines** (Premium): judged against GitLab's merge commit. Its tree is what `cinema push` tests as the merge result (plan §5.5).
- **Branch pushes.**

Pipelines started from the web, the API, a trigger or a schedule run every task. They can carry variables cinema can't see. Fork merge requests run every task too.

## Not yet / needs a live spike

- A run on gitlab.com, to confirm what docs and source say:
  - a notes fetch with the job token, including with `FF_GIT_URLS_WITHOUT_TOKENS`;
  - the child as a real `parent_pipeline`, and `CI_COMMIT_BRANCH` in it under an MR parent;
  - `strategy: depend` with "Pipelines must succeed";
  - merged-results SHAs.
- Live agreement pairs (`cinema graduate` on merge request pipelines), job logs, minutes and prices, all GitLab-side: TODO (plan §11.7).
- A config path other than `.gitlab-ci.yml` (Settings → CI/CD → CI/CD configuration file).
- `include:` of templates and components, `spec: inputs:`, and `rules: changes:`.
