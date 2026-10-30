#!/bin/bash
# Inside the container: clone a lab mirror, run its tasks in a clean checkout, print one line per task.
# usage: run.sh <repo> <tasks.yaml url or ""> <task>...
set -u
repo=$1; tasks=$2; shift 2
git clone -q --filter=blob:none "https://github.com/mjtechguy/cinema-lab-$repo" "$repo" || exit 1
cd "$repo"
if [ "$tasks" = analyze ]; then mkdir -p .cinema && cinema analyze --yaml > .cinema/tasks.yaml 2>/dev/null && git -c user.email=s@s -c user.name=s add .cinema && git -c user.email=s@s -c user.name=s commit -qm tasks
elif [ -n "$tasks" ]; then mkdir -p .cinema && curl -sSfL "$tasks" -o .cinema/tasks.yaml && git -c user.email=s@s -c user.name=s commit -qam tasks; fi
cinema trust >/dev/null 2>&1
start=$(date +%s)
cinema run --commit HEAD -j 2 "$@" > /tmp/run.out 2>&1
grep -E "^[✓✗!] " /tmp/run.out | sed "s/^/$repo $(uname -m) /"
for id in $(grep -E "^✗" /tmp/run.out | awk '{print $2}'); do echo "--- $repo $(uname -m) log $id"; cinema log "$id" 2>&1 | grep -v "^\s*$" | tail -25 | sed "s/^/LOG /"; done
echo "$repo $(uname -m) total $(( $(date +%s) - start ))s"
