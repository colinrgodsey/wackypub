#!/bin/bash
set -euo pipefail

# Build args capture the CALLING user's UID/GID (must run as the actual user,
# NOT under sudo - otherwise id -u returns 0/root and the container user mismatches)
docker build \
  --build-arg HOST_UID="$(id -u)" \
  --build-arg HOST_GID="$(id -g)" \
  -t agent-build-container:latest \
  "$(dirname "$0")"
