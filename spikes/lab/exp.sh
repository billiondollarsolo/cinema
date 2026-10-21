#!/usr/bin/env bash
# One CInema lab experiment: new commit → clean-checkout runs of every task → push code + receipts.
# Usage: exp.sh NAME [GOTOOLCHAIN]
set -uo pipefail
name=$1
[ -n "${2:-}" ] && export GOTOOLCHAIN=$2
export CINEMA_DATA_DIR=$HOME/.local/share/cinema CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0
mkdir -p ~/lab/results && cd ~/lab/$name || exit 2
log=~/lab/results/$name.$(date +%H%M).log
{
  echo "== $name $(date -u +%FT%TZ) go=$(go version)"
  cinema-lab trust >/dev/null
  echo "Experiment $(date -u +%FT%TZ)" >> LAB.md && git commit -qam "lab: experiment"
  start=$(date +%s)
  cinema-lab push --run
  echo "push_exit=$? local_secs=$(( $(date +%s) - start )) commit=$(git rev-parse HEAD)"
} > "$log" 2>&1
echo "$log"
