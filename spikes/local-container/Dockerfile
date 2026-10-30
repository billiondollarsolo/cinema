# P4-1 spike: a slim runner-like image (Ubuntu 24.04, non-root "runner" with sudo, runner paths).
FROM ubuntu:24.04
ARG TARGETARCH
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-client sudo xz-utils make gcc g++ libc6-dev unzip zip jq python3 \
    && rm -rf /var/lib/apt/lists/* \
    && userdel -r ubuntu 2>/dev/null; useradd -m -u 1001 runner && echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner
# A bootstrap Go, as on GitHub's image: cinema's runtime pin then fetches the exact version via GOTOOLCHAIN.
RUN curl -sSfL https://go.dev/dl/go1.25.5.linux-${TARGETARCH}.tar.gz | tar -xz -C /usr/local
ENV PATH=/usr/local/go/bin:$PATH
RUN mkdir -p /home/runner/work && chown runner:runner /home/runner/work
USER runner
ENV CI=true TZ=UTC LANG=C.UTF-8
WORKDIR /home/runner/work
