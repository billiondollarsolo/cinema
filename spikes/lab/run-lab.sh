#!/usr/bin/env bash
# CInema lab: run one project's CI tasks on this box and check CInema's claims.
#   1. run every task (forced), timing each
#   2. list files the tasks wrote into the tree (edge cases for whole-source fingerprints)
#   3. cinema status: which tasks are reusable now
#   4. cinema run --all again: must find nothing to run (stable fingerprints)
# Usage: run-lab.sh PROJECT_DIR TASKS_YAML OUT_DIR
set -uo pipefail
mkdir -p "$3"
dir=$(realpath "$1") tasks=$(realpath "$2") out=$(realpath "$3")
name=$(basename "$dir")
export CINEMA_DATA_DIR=${CINEMA_DATA_DIR:-/root/cinema-lab/data} CI=true COREPACK_ENABLE_DOWNLOAD_PROMPT=0
cd "$dir" || exit 2

mkdir -p .cinema && cp "$tasks" .cinema/tasks.yaml
cinema trust >/dev/null || { echo "$name: trust failed"; exit 2; }
git status --porcelain --untracked-files=all | grep -v '^?? .cinema/' > "$out/$name.dirty-before"

start=$(date +%s)
cinema run --all --force > "$out/$name.run1.out" 2> "$out/$name.run1.err"
echo "run1_exit=$? run1_secs=$(( $(date +%s) - start ))" > "$out/$name.summary"

git status --porcelain --untracked-files=all | grep -v '^?? .cinema/' > "$out/$name.dirty-after"
comm -13 "$out/$name.dirty-before" "$out/$name.dirty-after" > "$out/$name.written"
echo "files_written=$(wc -l < "$out/$name.written")" >> "$out/$name.summary"

cinema status --json > "$out/$name.status.json"
echo "status_exit=$?" >> "$out/$name.summary"

start=$(date +%s)
cinema run --all > "$out/$name.run2.out" 2> "$out/$name.run2.err"
echo "run2_exit=$? run2_secs=$(( $(date +%s) - start ))" >> "$out/$name.summary"
cat "$out/$name.summary"
