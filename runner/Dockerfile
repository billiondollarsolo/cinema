# cinema's runner image (plan §9.2): Ubuntu like GitHub's ubuntu-24.04 runner,
# slim, with the base tools tests call without any workflow naming them (and
# what pinned runtimes need from the OS: ICU for .NET, fonts for Java's AWT), and a
# non-root "runner" user with passwordless sudo. No language runtimes: cinema
# mounts the pinned ones (plan §6.5). Tools a job runs from the image without
# setting them up (python3, mvn) are GitHub's versions, probed on both sides. Published multi-arch; never built on a
# developer's machine for another arch (P4-1). One image per GitHub runner
# release: --build-arg UBUNTU=22.04 | 24.04 | 26.04.
ARG UBUNTU=24.04
FROM ubuntu:${UBUNTU}
ARG UBUNTU
# Python as GitHub's install-python.sh: the release's apt python3 is what a job
# without setup-python runs (python, pip), and pip may install outside a venv
# (break-system-packages, except on 22.04).
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      bash ca-certificates curl wget git git-lfs openssh-client gnupg sudo tzdata locales \
      xz-utils zstd unzip zip bzip2 tar gzip file jq rsync less \
      make gcc g++ libc6-dev pkg-config \
      python3 python3-dev python3-pip python3-venv python-is-python3 \
      procps psmisc lsof iproute2 netcat-openbsd \
      fontconfig fonts-dejavu-core \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      $(apt-cache pkgnames libicu | grep -E '^libicu[0-9]+$' | sort -V | tail -1) \
      $(apt-cache pkgnames liblttng-ust | grep -E '^liblttng-ust1(t64)?$' | tail -1) \
    && rm -rf /var/lib/apt/lists/* \
    && if [ "$UBUNTU" != 22.04 ]; then printf '[global]\nbreak-system-packages = true\n' > /etc/pip.conf; fi \
    && (userdel -r ubuntu 2>/dev/null || true) \
    && useradd -m -u 1001 -s /bin/bash runner \
    && echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner && chmod 0440 /etc/sudoers.d/runner \
    && mkdir -p /home/runner/work && chown runner:runner /home/runner/work
# Maven as GitHub's install-java-tools.sh: Apache's bin zip unpacked in
# /usr/share, mvn linked into /usr/bin, no MAVEN_HOME. Their toolset-2204/2404
# say "3" (the newest 3.x when the image is built: 3.10.0 in images of
# 2026-10-26), toolset-2604 3.9.15. Jobs calling mvn probe mvn --version on
# both sides: when GitHub's moves, bump it here (SHA-512 from Apache's .sha512).
RUN case "$UBUNTU" in \
      22.04|24.04) v=3.10.0 sum=22d31676d5b92ed53308c19ecded3245d0efd0cddfec51ec7bab62f4f13ad70ddddceff8e62940e0e8e2421f3cde8106737f4becec0aa034ffeff2a754e88337 ;; \
      26.04) v=3.9.15 sum=251c676eb88684d6dd86c666817979d256a335c31ad34ab0e1df17eeca1cd504a6c890e906a46d04e51cc7a4fb02f33a75971ef750558034e0a81c257be02267 ;; \
      *) echo "no Maven version for ubuntu $UBUNTU" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o /tmp/maven.zip "https://archive.apache.org/dist/maven/maven-3/$v/binaries/apache-maven-$v-bin.zip" \
    && echo "$sum  /tmp/maven.zip" | sha512sum -c - \
    && unzip -qq -d /usr/share /tmp/maven.zip && rm /tmp/maven.zip \
    && ln -s /usr/share/apache-maven-$v/bin/mvn /usr/bin/mvn
LABEL org.opencontainers.image.source=https://github.com/billiondollarsolo/cinema \
      org.opencontainers.image.description="cinema runner image: runs GitHub Actions Linux jobs locally, like GitHub's ubuntu runners"
# The docker CLI with buildx and compose, as GitHub's images have (from Docker's
# signed apt repository; 26.04 falls back to 24.04's repository until Docker
# publishes one). Used by docker: host tasks, which get the engine's socket.
RUN set -e; . /etc/os-release; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc; \
    gpg --show-keys --with-colons /etc/apt/keyrings/docker.asc | grep -q '^fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:'; \
    code=$VERSION_CODENAME; curl -fsSL -o /dev/null "https://download.docker.com/linux/ubuntu/dists/$code/Release" || code=noble; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $code stable" > /etc/apt/sources.list.d/docker.list; \
    apt-get update; DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends docker-ce-cli docker-buildx-plugin docker-compose-plugin; \
    rm -rf /var/lib/apt/lists/*
USER runner
WORKDIR /home/runner/work
