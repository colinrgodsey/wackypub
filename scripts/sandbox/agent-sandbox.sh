#!/bin/bash
set -euo pipefail

# Usage: agent-sandbox.sh <workspace-path>
# Creates and starts a persistent sandboxed container for an agent.
#
# Mount strategy (all host paths preserved EXACTLY inside the container):
#   - <workspace-path>/container-ws/  -> same path (agent scratch area)
#   - <workspace>/projects/           -> same path (canonical repo clones)
#   - <workspace>/worktrees/          -> same path (git worktrees)
#
# Same-path mounts mean absolute paths are identical on both sides of the
# boundary: worktree .git files point back to the canonical repo at the same
# path, so git operations inside a worktree resolve correctly in-container.

WS_PATH="${1:?Usage: agent-sandbox.sh <workspace-path> (absolute path to agent workspace)}"
WS_PATH=$(realpath "$WS_PATH")
CONTAINER_NAME=$(echo "$WS_PATH" | sed 's|^/||; s|/$||; s|/|_|g')
CONTAINER_WS="$WS_PATH/container-ws"
PARENT=$(dirname "$WS_PATH")
PROJECTS="$PARENT/projects"
WORKTREES="$PARENT/worktrees"

mkdir -p "$CONTAINER_WS"

# Stop and remove any existing container with the same name
if docker inspect "agent-sandbox-$CONTAINER_NAME" >/dev/null 2>&1; then
    echo "Removing existing container: agent-sandbox-$CONTAINER_NAME"
    docker rm -f "agent-sandbox-$CONTAINER_NAME" >/dev/null
fi

# Build the mount list: base scratch mount + canonical dirs (if they exist)
MOUNTS=(-v "${CONTAINER_WS}:${CONTAINER_WS}")
if [ -d "$PROJECTS" ]; then MOUNTS+=(-v "${PROJECTS}:${PROJECTS}"); fi
if [ -d "$WORKTREES" ]; then MOUNTS+=(-v "${WORKTREES}:${WORKTREES}"); fi

# Start the sandbox container (detached, persistent, survives reboots)
docker run -d \
    --name "agent-sandbox-$CONTAINER_NAME" \
    --restart unless-stopped \
    --workdir "$CONTAINER_WS" \
    "${MOUNTS[@]}" \
    agent-build-container:latest \
    sleep infinity

echo "Sandbox ready: agent-sandbox-$CONTAINER_NAME"
echo "  mounts (same-path):"
echo "    $CONTAINER_WS"
[ -d "$PROJECTS" ]   && echo "    $PROJECTS"
[ -d "$WORKTREES" ] && echo "    $WORKTREES"
echo "  restart policy: unless-stopped (survives reboots)"
echo "  exec:  docker exec -it agent-sandbox-$CONTAINER_NAME /bin/bash"
