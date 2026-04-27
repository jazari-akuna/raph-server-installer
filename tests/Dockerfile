# raph-server-installer test harness — simulated VPS.
#
# Ubuntu 24.04 base. We deliberately do NOT install Docker, AmneziaWG, or
# certbot here — the bootstrap pipeline will (try to) install those itself,
# and TEST_MODE short-circuits the irreversible parts so the harness can
# observe state mutations without a real VPS.
#
# What's pre-installed:
#   - bash, coreutils, findutils, grep, sed, gawk, procps   (basic shell)
#   - curl, wget, ca-certificates                           (HTTP probes)
#   - sudo                                                  (visudo for sudoers drop-in)
#   - openssh-server                                        (smoke-test ssh probes)
#   - python3                                               (chnroutes calc, mock DNS)
#   - jq, gettext-base                                      (bootstrap deps)
#   - git                                                   (clone fallback path; harness uses TEST_REPO_SRC bind)
#   - iproute2, iptables                                    (network state introspection)
#   - file, tree                                            (assertion helpers)
#
# A non-root `tester` user is created for the SSH probe scenarios.
#
# The repo under test is bind-mounted in at /opt/raph-server-installer-src
# by docker-compose; the harness sets TEST_REPO_SRC so bootstrap.sh symlinks
# it instead of cloning from GitHub.

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash \
      coreutils \
      findutils \
      grep \
      sed \
      gawk \
      procps \
      curl \
      wget \
      ca-certificates \
      sudo \
      openssh-server \
      python3 \
      jq \
      gettext-base \
      git \
      iproute2 \
      iptables \
      file \
      tree \
      tzdata \
      lsb-release \
      gnupg \
 && rm -rf /var/lib/apt/lists/* \
 && apt-get clean

# Go 1.23 — needed by the wizard_walkthrough scenario (Wave 4B), which
# builds stacks/enrol/ inside the container and runs the binary so it
# can curl through the setup wizard's HTTP routes. Ubuntu 24.04's
# golang-go package is 1.22, which is older than enrol's go.mod
# (`go 1.23`); install the upstream tarball instead.
ARG GO_VERSION=1.23.4
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) goarch=amd64 ;; \
      arm64) goarch=arm64 ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${goarch}.tar.gz" \
      -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz; \
    /usr/local/go/bin/go version
ENV PATH="/usr/local/go/bin:${PATH}"
# /tmp/go is writable by every user inside the container — the harness
# runs scenarios as root but the wizard scenario could run anything; a
# shared cache path keeps the second `go build` instant without forcing
# the caller to think about ownership.
ENV GOPATH=/tmp/go
ENV GOCACHE=/tmp/go-cache

# Non-root tester user for ssh-probe scenarios. Locked password (key only).
# Wave 4B: ensure /tmp/go and /tmp/go-cache exist with permissive perms so
# the wizard_walkthrough scenario's `go build` can write its cache
# regardless of which user the harness runs scenarios as.
RUN useradd -m -s /bin/bash tester \
 && mkdir -p /home/tester/.ssh \
 && chown -R tester:tester /home/tester/.ssh \
 && chmod 700 /home/tester/.ssh \
 && install -d -m 1777 /tmp/go /tmp/go-cache

# /run/sshd needs to exist for sshd to start.
RUN mkdir -p /run/sshd

# A stub /usr/local/bin tree of commands the bootstrap touches but cannot
# meaningfully exercise inside a container. systemctl/docker/cryptsetup are
# the big three. Each stub logs to /tmp/test-stubs.log so assertions can
# verify "this was invoked with these args" after the fact.
COPY stubs/ /usr/local/bin/
RUN chmod +x /usr/local/bin/systemctl \
            /usr/local/bin/docker \
            /usr/local/bin/cryptsetup \
            /usr/local/bin/mkfs.ext4 \
            /usr/local/bin/modinfo \
            /usr/local/bin/modprobe \
            /usr/local/bin/dpkg-reconfigure \
            /usr/local/bin/debconf-set-selections \
            /usr/local/bin/ufw \
            /usr/local/bin/fail2ban-client \
            /usr/local/bin/swapon \
            /usr/local/bin/mkswap \
            /usr/local/bin/sysctl

# The harness itself — copied into /opt/test so tests/run-tests.sh can call
# tests/assertions.sh and friends regardless of which side of the bind mount
# is the source of truth.
COPY assertions.sh /opt/test/assertions.sh
COPY scenarios.sh  /opt/test/scenarios.sh
COPY mock-gw0.sh   /opt/test/mock-gw0.sh
COPY entrypoint.sh /opt/test/entrypoint.sh
RUN chmod +x /opt/test/*.sh

# /opt/raph-server-installer-src is the bind mount of the working tree.
# /var/log/raph-installer is created by bootstrap.sh; pre-create so the
# harness can tail it from a sidecar even if bootstrap fails before the
# install -d call.
RUN mkdir -p /opt/raph-server-installer-src \
 && mkdir -p /var/log/raph-installer \
 && mkdir -p /srv/store \
 && mkdir -p /etc/raph-installer

ENV TEST_MODE=1
ENV TEST_REPO_SRC=/opt/raph-server-installer-src
ENV TEST_STUB_LOG=/tmp/test-stubs.log
ENV TEST_COMPOSE_LOG=/tmp/test-compose.log

WORKDIR /opt/test
ENTRYPOINT ["/opt/test/entrypoint.sh"]
