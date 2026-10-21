#!/usr/bin/env bash
# P0-5: how often is a merged PR's head up to date with base at merge time?
# Proxy for "PR-head receipts can match the merge commit" under whole-source fingerprints (P0-3).
#   up to date = merge commit's first parent (base tip at merge) is an ancestor of the PR head
# Rebase-merged PRs are skipped (their merge_commit_sha isn't a merge/squash of the head).
# Usage: spikes/p0-5-uptodate.sh OWNER/REPO [N=30]
set -euo pipefail
repo=$1 n=${2:-30}

check() { # merge_sha head_sha → up|behind|skip
  local parents base status
  parents=$(gh api "repos/$repo/commits/$1" --jq '.parents|length') || { echo skip; return; }
  base=$(gh api "repos/$repo/commits/$1" --jq '.parents[0].sha')
  # squash: 1 parent (base tip). merge: 2 parents, [0] = base tip. Either way [0] is base at merge time.
  [ "$base" = "$2" ] && { echo skip; return; }   # rebase/fast-forward: head is the base tip
  status=$(gh api "repos/$repo/compare/$base...$2" --jq .status 2>/dev/null) || { echo skip; return; }
  case $status in ahead|identical) echo up ;; diverged) echo behind ;; *) echo skip ;; esac
}
export -f check; export repo

gh pr list -R "$repo" --state merged --limit "$n" --json mergeCommit,headRefOid \
  --jq '.[] | select(.mergeCommit != null) | "\(.mergeCommit.oid) \(.headRefOid)"' \
  | xargs -P 8 -n 2 bash -c 'check "$0" "$1"' \
  | sort | uniq -c | awk -v r="$repo" '{c[$2]=$1} END {
      t = c["up"] + c["behind"]
      printf "%-32s up=%d behind=%d skip=%d  up-to-date=%s\n", r, c["up"], c["behind"], c["skip"], (t ? sprintf("%.0f%%", 100*c["up"]/t) : "n/a") }'
