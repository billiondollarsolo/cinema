#!/usr/bin/env bash
# Raw-draft PR experiment: tasks.yaml is exactly `cinema analyze --yaml` (no
# edits), the gate is the generic runtime workflow, then a code change, clean-
# checkout evidence and push. The caller opens the PR.
# Usage: exp-draft.sh NAME SOURCE_FILE COMMENT   (COMMENT: "//" or "#")
set -uo pipefail
name=$1 file=$2 c=$3
export CINEMA_DATA_DIR=$HOME/.local/share/cinema CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0
cd ~/lab/$name || exit 2
base=$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|origin/||')
git checkout -q "$base" && git pull -q --ff-only
branch=lab/draft-$(date +%H%M)
log=~/lab/results/$name.draft.$(date +%H%M).log
{
  echo "== $name raw-draft PR $branch from $base $(date -u +%FT%TZ)"
  git checkout -q -b "$branch"
  cinema-lab analyze --yaml > .cinema/tasks.yaml
  cp ~/lab/in/cinema-parallel-node-runtime.yml .github/workflows/cinema.yml
  git add .cinema/tasks.yaml .github/workflows/cinema.yml && git commit -qm "lab: tasks.yaml is the raw cinema analyze draft; generic gate"
  printf "\n%s lab: raw-draft PR experiment %s\n" "$c" "$(date -u +%FT%TZ)" >> "$file" && git commit -qam "lab: PR experiment (code change)"
  grep -c "^  [a-z0-9]" .cinema/tasks.yaml | sed 's/^/draft tasks: /'
  cinema-lab trust >/dev/null
  start=$(date +%s)
  cinema-lab push --run -j 4
  echo "push_exit=$? local_secs=$(( $(date +%s) - start )) branch=$branch base=$base commit=$(git rev-parse HEAD)"
} > "$log" 2>&1
echo "$log"
