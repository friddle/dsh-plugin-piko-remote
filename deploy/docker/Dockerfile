# NOTE: deliberately NO `# syntax=docker/dockerfile:1` directive here — that
# directive forces a pull of the external frontend image from Docker Hub, which
# is unreachable from this build host. Nothing in this file uses a non-default
# frontend feature.
#
# DSH (DeepSeek Harness) image for the BI interface deployment.
#
# Why this image exists:
#   * upstream publishes no container image (deepseek-ai/deepseek-harness has no
#     Dockerfile/compose file, and there is no ghcr/dockerhub tag),
#   * `dsh web` ships **no** authentication and deliberately refuses to bind
#     0.0.0.0 ("it would expose remote code execution to the network").
#
# So this image runs `dsh web` on loopback and puts a Basic Auth reverse proxy
# (auth-proxy.mjs, zero dependencies) on 0.0.0.0:8080 in front of it.
#
# Toolchain: Node.js (base image) + Go, git, kubectl, mariadb-client/mysqldump,
# vim and the usual build tools, so the harness can actually do work in-pod.

# Docker Hub is unreachable from this build host, so the base image comes from a
# public CN mirror by default; override with --build-arg on other networks.
# (dockermirror.service.code27.cn also works but truncates this image's 211MB
# layer with "short read: unexpected EOF", hence daocloud.)
ARG NODE_IMAGE=docker.m.daocloud.io/library/node:24-bookworm
FROM ${NODE_IMAGE}

ARG TARGETARCH
ARG GO_VERSION=1.27.1
ARG KUBECTL_VERSION=1.33.9
ARG DSH_VERSION=0.1.5-rc.1
# Primary download mirror (fast from CN); Go falls back to go.dev/dl.
ARG MIRROR=https://mirrors.aliyun.com
ARG NPM_REGISTRY=https://registry.npmjs.org

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    TZ=Asia/Shanghai \
    DSH_HOME=/root/.dsh \
    DSH_PORT=3080 \
    DSH_PROXY_PORT=8080 \
    DSH_PROXY_BIND=0.0.0.0 \
    DSH_CONFIG_DIR=/etc/dsh-credentials \
    WORKSPACE_DIR=/workspace \
    PATH=/usr/local/go/bin:/root/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# ---------------------------------------------------------------------------
# OS packages: toolchain + the CLIs the harness uses as a coding agent.
# `mariadb-client` provides both `mariadb`/`mysql` and `mysqldump`.
# ---------------------------------------------------------------------------
RUN set -eux; \
    # Point apt at the LAN-friendly mirror (deb.debian.org works but is slow here).
    if [ -f /etc/apt/sources.list.d/debian.sources ]; then \
        sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list.d/debian.sources; \
    fi; \
    if [ -f /etc/apt/sources.list ]; then \
        sed -i 's|deb.debian.org|mirrors.aliyun.com|g' /etc/apt/sources.list; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl wget gnupg \
        git openssh-client \
        vim less jq unzip zip xz-utils tar gzip rsync \
        build-essential pkg-config automake autoconf libtool \
        python3 python3-venv python3-pip \
        mariadb-client \
        procps psmisc net-tools iproute2 dnsutils iputils-ping \
        telnet socat tmux htop tree file tini locales; \
    sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen || true; \
    locale-gen || true; \
    rm -rf /var/lib/apt/lists/*; \
    git --version; \
    mysqldump --version; \
    vim --version | head -1

# ---------------------------------------------------------------------------
# Go (distro packages are far too old for modern projects).
# ---------------------------------------------------------------------------
RUN set -eux; \
    url="${MIRROR}/golang/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz"; \
    curl -fsSL "$url" -o /tmp/go.tgz \
      || curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm -f /tmp/go.tgz; \
    go version

# ---------------------------------------------------------------------------
# kubectl
# ---------------------------------------------------------------------------
RUN set -eux; \
    curl -fsSL "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
      -o /usr/local/bin/kubectl; \
    chmod 0755 /usr/local/bin/kubectl; \
    kubectl version --client=true

# ---------------------------------------------------------------------------
# pnpm (dsh plugin management forwards to pnpm) and dsh itself.
# `@deepseek-ai/dsh` depends on every bundle (dsh-base, dsh-web-app, ...), and
# the profile resolves them through the install's node_modules fallback, so the
# global install is enough to boot `dsh web` offline.
# ---------------------------------------------------------------------------
RUN set -eux; \
    npm install -g --no-fund --no-audit --registry="$NPM_REGISTRY" \
        "pnpm@latest" "@deepseek-ai/dsh@${DSH_VERSION}"; \
    npm cache clean --force; \
    dsh --version; \
    pnpm --version

# Bake the web profile at build time: prepareProfile() writes
# $DSH_HOME/profiles/web/{package.json,cordis.yml,...} and heals the
# node_modules fallback, so a cold pod boots without touching the network.
RUN set -eux; \
    dsh --profile web --dump-config > /tmp/web-tree.txt; \
    test -s /tmp/web-tree.txt; \
    test -f "$DSH_HOME/profiles/web/cordis.yml"; \
    rm -f /tmp/web-tree.txt

# ---------------------------------------------------------------------------
# Plugins baked into the web profile.
#
# chrome-driverless drives the chrome-driverless service that already runs in
# this cluster (see the DSH_CHROME_BASE_URL default in entrypoint.sh); there is
# no docker socket in the pod, so the plugin never manages a container itself.
# ---------------------------------------------------------------------------
ARG CHROME_PLUGIN_SPEC=github:friddle/dsh-plugin-chrome-driverless#v0.1.0
RUN set -eux; \
    dsh plugin --profile web add "$CHROME_PLUGIN_SPEC"; \
    test ! -e "$DSH_HOME/profiles/web/node_modules/@deepseek-ai/dsh-tools"; \
    dsh --profile web --dump-config > /tmp/web-tree.txt; \
    grep -q 'chrome-driverless' /tmp/web-tree.txt; \
    cat /tmp/web-tree.txt | grep -A3 -i 'chrome-driverless' | head -8; \
    rm -f /tmp/web-tree.txt

# ---------------------------------------------------------------------------
# Login shells (`bash -lc`, which agent tools love to spawn) start from
# /etc/profile and would otherwise lose Go from PATH — ENV alone is not enough.
# ---------------------------------------------------------------------------
RUN set -eux; \
    printf '%s\n' \
        'export PATH=/usr/local/go/bin:/root/go/bin:$PATH' \
        'export GOPATH=/root/go' \
        > /etc/profile.d/go-path.sh; \
    chmod 0644 /etc/profile.d/go-path.sh; \
    env -i bash -lc 'go version'

# ---------------------------------------------------------------------------
# Auth front end
# ---------------------------------------------------------------------------
COPY auth-proxy.mjs /usr/local/lib/dsh-proxy/auth-proxy.mjs
COPY entrypoint.sh /usr/local/bin/dsh-entrypoint
# Patch overlays for the baked plugins (rendered at boot by entrypoint.sh).
COPY patches /opt/dsh/patches
RUN chmod 0755 /usr/local/bin/dsh-entrypoint /usr/local/lib/dsh-proxy/auth-proxy.mjs

# Auth defaults live down here on purpose: adding an ENV before the apt/npm
# layers would invalidate their cache and cost a full ~10min rebuild.
# `token` = cookie session only, no HTTP auth prompts; see auth-proxy.mjs.
ENV DSH_AUTH_MODE=token \
    DSH_SESSION_TTL=2592000

RUN mkdir -p "$WORKSPACE_DIR" "$DSH_HOME"

WORKDIR /workspace
EXPOSE 8080

# tini reaps the dsh/proxy children and forwards SIGTERM properly.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/dsh-entrypoint"]
