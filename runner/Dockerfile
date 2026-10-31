# cinema's runner image (plan §9.2): Ubuntu like GitHub's ubuntu-24.04 runner,
# slim, with the base tools tests call without any workflow naming them (and
# what pinned runtimes need from the OS: ICU for .NET, fonts for Java's AWT), and a
# non-root "runner" user with passwordless sudo. No language runtimes: cinema
# mounts the pinned ones (plan §6.5). Published multi-arch; never built on a
# developer's machine for another arch (P4-1). One image per GitHub runner
# release: --build-arg UBUNTU=22.04 | 24.04 | 26.04.
ARG UBUNTU=24.04
FROM ubuntu:${UBUNTU}
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bash ca-certificates curl wget git git-lfs openssh-client gnupg sudo tzdata locales \
      xz-utils zstd unzip zip bzip2 tar gzip file jq rsync \
      make gcc g++ libc6-dev pkg-config \
      python3 python3-venv \
      procps psmisc lsof iproute2 netcat-openbsd \
      fontconfig fonts-dejavu-core \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      $(apt-cache pkgnames libicu | grep -E '^libicu[0-9]+$' | sort -V | tail -1) \
      $(apt-cache pkgnames liblttng-ust | grep -E '^liblttng-ust1(t64)?$' | tail -1) \
    && rm -rf /var/lib/apt/lists/* \
    && (userdel -r ubuntu 2>/dev/null || true) \
    && useradd -m -u 1001 -s /bin/bash runner \
    && echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner && chmod 0440 /etc/sudoers.d/runner \
    && mkdir -p /home/runner/work && chown runner:runner /home/runner/work
LABEL org.opencontainers.image.source=https://github.com/billiondollarsolo/cinema \
      org.opencontainers.image.description="cinema runner image: runs GitHub Actions Linux jobs locally, like GitHub's ubuntu runners"
USER runner
WORKDIR /home/runner/work
