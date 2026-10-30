#!/bin/bash
# P4-1: same tasks, amd64 under Rosetta and arm64 native, in a runner-sized container (4 CPUs, 16 GB).
export DOCKER_CONFIG=${DOCKER_CONFIG:-/tmp/cinema-docker} DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
here=$(cd "$(dirname "$0")" && pwd)
for arch in amd64 arm64; do
  for job in "cli|analyze|build-ubuntu-latest lint govulncheck" "eslint|analyze|test-types pnpm-test verify-files" "traefik|analyze|build-amd64-linux"; do
    IFS='|' read -r repo tasks ids <<<"$job"
    docker run --rm --platform linux/$arch --cpus 4 --memory 16g --shm-size 2g \
      --label cinema.spike=1 -v "$here/cinema-$arch:/usr/local/bin/cinema:ro" -v "$here/run.sh:/usr/local/bin/run.sh:ro" \
      cinema-spike-runner:$arch run.sh "$repo" "$tasks" $ids
  done
done
