# Stage 1: Build binaries
FROM golang:1.25.7 AS builder

# wackyacp (github.com/colinrgodsey/wackyacp) is a separate repo, not a submodule of
# this one - pin to a specific commit SHA, not a branch, so a rebuild months from now
# reproduces the same binary rather than floating whatever main has drifted to.
#
# To bump: pick a new commit SHA from https://github.com/colinrgodsey/wackyacp/commits/main,
# pass it at build time (docker build --build-arg WACKYACP_REF=<new-sha> ...), or update
# the default below and commit that change deliberately.
ARG WACKYACP_REF=89ca0f3b78ffb3cf7a175e86e33927a96d0446ec

WORKDIR /build

# Copy repository source files
COPY . .

# Build all binaries into ./bin
RUN make build

# Clone and build wackyacp at the pinned ref, outside /build so it never mixes into this
# repo's own module (`make build`/go.mod above) - just another binary landing in ./bin
# for the final stage's existing `COPY --from=builder /build/bin/*` to pick up.
RUN git clone --quiet https://github.com/colinrgodsey/wackyacp.git /tmp/wackyacp-src \
    && git -C /tmp/wackyacp-src checkout --quiet "$WACKYACP_REF" \
    && go build -C /tmp/wackyacp-src/cmd/wackyacp -o /build/bin/wackyacp . \
    && rm -rf /tmp/wackyacp-src

# Stage 2: Final Runtime Image
FROM ubuntu:25.10

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

# Install WackyPub suite binaries
COPY --from=builder /build/bin/* /usr/local/bin/

# Assemble the default workspace template into /opt/wackypub/template_ws
RUN mkdir -p /opt/wackypub/template_ws/toolsets \
             /opt/wackypub/template_ws/skillsets \
             /opt/wackypub/template_ws/runtimes \
             /opt/wackypub/template_ws/director/tools \
             /opt/wackypub/template_ws/director/skills \
    && touch /opt/wackypub/template_ws/WACKYPUB_ROOT \
    && touch /opt/wackypub/template_ws/director/WACKYPUB_ALLOWED_AGENTS \
    && ln -sf ../runtimes/openrouter-auto.json /opt/wackypub/template_ws/director/runtime.json

# Copy bundled skills and runtimes from repository
COPY skills/ /opt/wackypub/template_ws/skillsets/
COPY tools/files-rw/skills/ /opt/wackypub/template_ws/skillsets/
COPY tools/wackyproc/skills/ /opt/wackypub/template_ws/skillsets/
COPY examples/runtimes/ /opt/wackypub/template_ws/runtimes/
# Recipes: the director agent operating in this container can read and follow them.
COPY recipes/ /opt/wackypub/template_ws/recipes/
COPY agents/director/AGENTS.md /opt/wackypub/template_ws/director/AGENTS.md

# Populate toolsets with symlinks to binaries
RUN for bin in wackypub files-rw wackyproc wackydiscord wackyacp; do \
        ln -sf "/usr/local/bin/$bin" "/opt/wackypub/template_ws/toolsets/$bin"; \
        ln -sf "../../toolsets/$bin" "/opt/wackypub/template_ws/director/tools/$bin"; \
    done \
    && for sysbin in bash git curl python3 node npm sudo; do \
        SYS_PATH=$(command -v "$sysbin" || true); \
        if [ -n "$SYS_PATH" ]; then \
            ln -sf "$SYS_PATH" "/opt/wackypub/template_ws/toolsets/$sysbin"; \
            ln -sf "../../toolsets/$sysbin" "/opt/wackypub/template_ws/director/tools/$sysbin"; \
        fi \
    done \
    && for skill in /opt/wackypub/template_ws/skillsets/*; do \
        if [ -d "$skill" ]; then \
            skillname=$(basename "$skill"); \
            ln -sf "../../skillsets/$skillname" "/opt/wackypub/template_ws/director/skills/$skillname"; \
        fi \
    done

# Create wackypub non-root user (UID 1000) and set permissions
RUN if id -u ubuntu >/dev/null 2>&1; then \
        usermod -l wackypub -d /home/wackypub -m ubuntu && groupmod -n wackypub ubuntu; \
    else \
        groupadd -g 1000 wackypub && useradd -u 1000 -g wackypub -m -s /bin/bash wackypub; \
    fi \
    && mkdir -p /ws /opt/wackypub \
    && chown -R wackypub:wackypub /opt/wackypub /ws \
    && echo "wackypub ALL=(root) NOPASSWD: /usr/bin/apt-get, /usr/bin/apt" > /etc/sudoers.d/wackypub \
    && chmod 0440 /etc/sudoers.d/wackypub

# Install entrypoint script
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

WORKDIR /ws

USER wackypub

# tini as PID 1: a shell PID 1 never reaps orphaned processes, so defunct entries accumulate (wackyproc#22, defunct-process-accumulation)
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/docker-entrypoint.sh"]