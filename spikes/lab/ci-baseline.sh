#!/usr/bin/env bash
# Upstream PR CI cost from the public Actions API, per workflow run:
#   jobs     number of jobs
#   job_min  Σ job durations (runner-minutes actually used)
#   billed   Σ ceil(job minutes), GitHub's per-job rounding (what private repos pay)
#   wall_min first job start → last job end (time to green, excluding queue)
# Usage: ci-baseline.sh OWNER/REPO [RUNS=20]   → one TSV line per workflow run (column 3 = head commit)
set -euo pipefail
repo=$1 n=${2:-20}
ids=$(gh api "repos/$repo/actions/runs?event=pull_request&status=completed&per_page=$n" \
  --jq '.workflow_runs[] | select(.conclusion=="success" or .conclusion=="failure") | "\(.id)\t\(.head_sha)"')
while IFS=$'\t' read -r id name; do  # name = head commit; runs for one push share it
  gh api --paginate "repos/$repo/actions/runs/$id/jobs?per_page=100" --jq '.jobs[]
    | select(.started_at != null and .completed_at != null and .conclusion != "skipped")
    | [(.started_at|fromdate), (.completed_at|fromdate)] | @tsv' |
  awk -F'\t' -v id="$id" -v name="$name" -v repo="$repo" '
    { d = $2 - $1; if (d < 0) d = 0; n++; sum += d; b += (d > 0 ? int((d + 59) / 60) : 1)
      if (!s || $1 < s) s = $1; if ($2 > e) e = $2 }
    END { if (n) printf "%s\t%s\t%s\t%d\t%.1f\t%d\t%.1f\n", repo, id, name, n, sum/60, b, (e-s)/60 }'
done <<< "$ids"
