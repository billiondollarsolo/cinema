#!/bin/bash
# P4-1 rerun after fixes: each project twice in one container (cold, then warm caches).
export DOCKER_CONFIG=${DOCKER_CONFIG:-/tmp/cinema-docker} DOCKER_HOST=unix://$HOME/.colima/default/docker.sock
here=$(cd "$(dirname "$0")" && pwd)
for arch in ${ARCHES:-arm64 amd64}; do
  for job in "cli|build-ubuntu-latest lint" "eslint|verify-files" "traefik|build-amd64-linux"; do
    IFS='|' read -r repo ids <<<"$job"
    docker run --rm --platform linux/$arch --cpus 4 --memory 16g --shm-size 2g --label cinema.spike=1 \
      -v "$here/cinema-$arch:/usr/local/bin/cinema:ro" cinema-spike-runner:$arch bash -c "
        git clone -q --filter=blob:none https://github.com/mjtechguy/cinema-lab-$repo r && cd r && mkdir -p .cinema &&
        cinema analyze --yaml > .cinema/tasks.yaml 2>/dev/null && git -c user.email=s@s -c user.name=s add .cinema &&
        git -c user.email=s@s -c user.name=s commit -qm t && cinema trust >/dev/null 2>&1
        for pass in cold warm; do
          cinema run --commit HEAD -j 2 $ids > /tmp/o 2>&1
          grep -E '^[✓✗!] ' /tmp/o | sed \"s/^/$repo \$(uname -m) \$pass /\"
          for id in \$(grep -E '^✗' /tmp/o | awk '{print \$2}'); do cinema log \$id 2>&1 | grep -E 'FAIL|not found|rror' | grep -v '^ok' | head -8 | sed 's/^/LOG /'; done
        done"
  done
done
