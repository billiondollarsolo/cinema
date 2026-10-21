#!/usr/bin/env bash
# P0-5: per PR run, compare existing CI vs the CInema gate.
#   green  = run created → run finished (what the PR author waits; includes queue + runner start)
#   wall   = first job start → last job end (parallel jobs overlap; excludes queue)
#   billed = Σ ceil(job minutes)  (GitHub per-job rounding; only matters for private repos)
# Usage: spikes/p0-5-measure.sh OWNER/REPO
set -euo pipefail
repo=$1

jobs_json() { gh api "repos/$repo/actions/runs/$1/jobs" --jq '.jobs'; }

printf '%-10s %-9s %7s %7s %7s\n' branch workflow green_s wall_s billed
for branch in $(gh pr list -R "$repo" --state all --json headRefName --jq '.[].headRefName' | sort); do
  for wf in ci p0-gate; do
    id=$(gh run list -R "$repo" --branch "$branch" --workflow "$wf.yml" --event pull_request \
      --status success --limit 1 --json databaseId --jq '.[0].databaseId // empty')
    [ -z "$id" ] && continue
    green=$(gh run view "$id" -R "$repo" --json createdAt,updatedAt --jq '(.updatedAt|fromdate) - (.createdAt|fromdate)')
    jobs_json "$id" | jq -r --arg b "$branch" --arg w "$wf" --arg g "$green" '
      map({s: (.started_at|fromdate), e: (.completed_at|fromdate)}) as $j
      | [$b, $w, $g, (($j|map(.e)|max) - ($j|map(.s)|min)),
         ($j|map(((.e - .s) / 60) | ceil | if . < 1 then 1 else . end)|add)]
      | @tsv' | awk -F'\t' '{printf "%-10s %-9s %7d %7d %7d\n", $1, $2, $3, $4, $5}'
  done
done

# Step costs that a partially-reused gate would still pay inline.
echo
echo "slowest steps across CI jobs on the newest PR run (seconds):"
id=$(gh run list -R "$repo" --workflow ci.yml --event pull_request --status success --limit 1 --json databaseId --jq '.[0].databaseId')
jobs_json "$id" | jq -r '.[] | .name as $j | .steps[]
  | select(.completed_at != null)
  | [((.completed_at|fromdate) - (.started_at|fromdate)), $j, .name] | @tsv' \
  | sort -rn | head -12
