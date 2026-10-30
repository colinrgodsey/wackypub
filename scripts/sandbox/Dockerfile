# Agent build container - runtime environment for sandboxed agent bash execution
FROM ubuntu:25.10

# Build args - UID/GID from the calling user; name is fixed as "agent"
ARG HOST_UID=1000
ARG HOST_GID=1000

# Install runtime utilities & build tooling
RUN apt-get update && apt-get install -y \
    ca-certificates \
    curl \
    git \
    python3 \
    golang-go \
    nodejs \
    npm \
    bash \
    sudo \
    tini \
    && rm -rf /var/lib/apt/lists/*

# Remove default ubuntu user (if present), create "agent" with host UID/GID
# so mounted volumes have correct file ownership; name is generic to reduce
# cognitive load for agents reading `whoami` output
RUN if id ubuntu >/dev/null 2>&1; then userdel -r ubuntu 2>/dev/null || true; fi \
    && groupadd -g "${HOST_GID}" agent 2>/dev/null || true \
    && useradd -u "${HOST_UID}" -g "${HOST_GID}" -m -s /bin/bash agent \
    && mkdir -p /ws /opt/wackypub \
    && chown -R agent:agent /opt/wackypub /ws \
    && echo "agent ALL=(root) NOPASSWD: /usr/bin/apt-get, /usr/bin/apt" > /etc/sudoers.d/agent \
    && chmod 0440 /etc/sudoers.d/agent

WORKDIR /workspace

USER agent
