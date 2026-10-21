#!/usr/bin/env bash
# PR experiment: branch off the default branch, change a file, make clean-checkout
# evidence, push branch + receipts. The caller opens the PR.
# Usage: exp-pr.sh NAME [GOTOOLCHAIN]
set -uo pipefail
name=$1
[ -n "${2:-}" ] && export GOTOOLCHAIN=$2
export CINEMA_DATA_DIR=$HOME/.local/share/cinema CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0
cd ~/lab/$name || exit 2
base=$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's|origin/||'); base=${base:-$(git branch --show-current)}
git checkout -q "$base" && git pull -q --ff-only
branch=lab/pr-$(date +%H%M)
log=~/lab/results/$name.pr.$(date +%H%M).log
{
  echo "== $name PR $branch from $base $(date -u +%FT%TZ) go=$(go version)"
  git checkout -q -b "$branch"
  echo "PR experiment $(date -u +%FT%TZ)" >> LAB.md && git commit -qam "lab: PR experiment"
  cinema-lab trust >/dev/null
  start=$(date +%s)
  cinema-lab push --run
  echo "push_exit=$? local_secs=$(( $(date +%s) - start )) branch=$branch base=$base commit=$(git rev-parse HEAD)"
} > "$log" 2>&1
echo "$log"
