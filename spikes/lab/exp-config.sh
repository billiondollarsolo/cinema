#!/usr/bin/env bash
# Apply lab config (tasks + parallel gate workflow, drop bots), commit, make
# clean-checkout evidence with -j 4, push. Usage: exp-config.sh NAME
set -uo pipefail
name=$1
export CINEMA_DATA_DIR=$HOME/.local/share/cinema CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0
cd ~/lab/$name || exit 2
git checkout -q "$(git symbolic-ref --short refs/remotes/origin/HEAD | sed 's|origin/||')" && git pull -q --ff-only
log=~/lab/results/$name.config.$(date +%H%M).log
{
  cp ~/lab/in/$name.yaml .cinema/tasks.yaml
  cp ~/lab/in/cinema-parallel-$name.yml .github/workflows/cinema.yml
  git rm -q --ignore-unmatch .github/dependabot.yml .github/dependabot.yaml renovate.json .github/renovate.json .github/renovate.json5
  git add -A .cinema .github && git commit -qm "lab: parallel gate, new tasks, no dependency bots"
  cinema-lab trust
  s=$(date +%s)
  cinema-lab push --run -j 4
  echo "push_exit=$? local_secs=$(( $(date +%s) - s )) commit=$(git rev-parse HEAD)"
} > "$log" 2>&1
echo "$log"
