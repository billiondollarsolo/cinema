#!/usr/bin/env bash
# Make a lab mirror: upstream's exact tracked tree as one orphan commit (built from
# upstream's own index, so force-added files that .gitignore matches are kept), plus
# the CInema lab config. Run on the evidence machine as the lab user.
# Usage: make-mirror.sh OWNER/REPO NAME TASKS_YAML WORKFLOW_YML
set -euo pipefail
up=$1 name=$2 tasks=$(realpath "$3") wf=$(realpath "$4")
dir=$HOME/lab/$name
rm -rf "$dir"
git clone -q --depth 1 "https://github.com/$up.git" "$dir"
cd "$dir"
branch=$(git symbolic-ref --short HEAD)
sha=$(git rev-parse HEAD)
git config user.name mjtechguy
git config user.email mjtechguy@users.noreply.github.com
git config core.sshCommand "ssh -i ~/.ssh/cinema_lab_$name -o IdentitiesOnly=yes"   # deploy keys are per repo
git checkout -q --orphan lab-snapshot
git commit -q -m "Snapshot of $up $sha"
git branch -M lab-snapshot "$branch"
# Bots that act on the repo (dependency PRs) don't belong in a lab mirror.
git rm -q --ignore-unmatch .github/dependabot.yml .github/dependabot.yaml renovate.json .github/renovate.json .renovaterc.json
mkdir -p .cinema .github/workflows
cp "$tasks" .cinema/tasks.yaml
cp "$wf" .github/workflows/cinema.yml
cat > LAB.md <<EOF
# CInema lab mirror

Snapshot of [$up](https://github.com/$up) at $sha, under its original license (see LICENSE),
used to measure CI reuse with [CInema](https://github.com/billiondollarsolo/cinema).
Not affiliated with or endorsed by the upstream project. Report issues upstream, not here.
EOF
git add -f .cinema/tasks.yaml .github/workflows/cinema.yml LAB.md
git commit -q -m "CInema lab config"
git remote set-url origin "git@github.com:mjtechguy/cinema-lab-$name.git"
git push -q -u origin "$branch"
echo "$name: pushed $branch ($(git rev-parse --short HEAD)), upstream $sha"
