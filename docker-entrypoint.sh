#!/usr/bin/env bash
set -e

WS_DIR="/ws"
TEMPLATE_DIR="/opt/wackypub/template_ws"

# 1. Bootstrap workspace if empty or uninitialized
if [ ! -f "$WS_DIR/WACKYPUB_ROOT" ]; then
    echo "🌱 Initializing new WackyPub workspace in $WS_DIR from template..."
    mkdir -p "$WS_DIR"
    if [ -d "$TEMPLATE_DIR" ]; then
        cp -rn "$TEMPLATE_DIR"/* "$WS_DIR/" 2>/dev/null || cp -r "$TEMPLATE_DIR"/* "$WS_DIR/"
    fi
    touch "$WS_DIR/WACKYPUB_ROOT"
fi

# Ensure workspace toolsets directory exists and has standard symlinks
mkdir -p "$WS_DIR/toolsets"
for bin in wackypub files-rw wackyproc wackydiscord; do
    BIN_PATH=$(command -v "$bin" || true)
    if [ -n "$BIN_PATH" ] && [ ! -e "$WS_DIR/toolsets/$bin" ]; then
        ln -sf "$BIN_PATH" "$WS_DIR/toolsets/$bin"
    fi
done

for sysbin in bash git curl python3 node npm sudo; do
    SYS_PATH=$(command -v "$sysbin" || true)
    if [ -n "$SYS_PATH" ] && [ ! -e "$WS_DIR/toolsets/$sysbin" ]; then
        ln -sf "$SYS_PATH" "$WS_DIR/toolsets/$sysbin"
    fi
done

# Ensure director tools symlinks exist
if [ -d "$WS_DIR/director" ]; then
    mkdir -p "$WS_DIR/director/tools"
    for tool in "$WS_DIR/toolsets"/*; do
        if [ -e "$tool" ]; then
            toolname=$(basename "$tool")
            if [ ! -e "$WS_DIR/director/tools/$toolname" ]; then
                ln -sf "../../toolsets/$toolname" "$WS_DIR/director/tools/$toolname"
            fi
        fi
    done
fi

# 2. Check for custom desired-entrypoint.sh
DESIRED_ENTRYPOINT="$WS_DIR/desired-entrypoint.sh"
if [ -f "$DESIRED_ENTRYPOINT" ]; then
    chmod +x "$DESIRED_ENTRYPOINT"
    echo "🚀 Executing $DESIRED_ENTRYPOINT..."
    set +e
    "$DESIRED_ENTRYPOINT" "$@"
    EXIT_CODE=$?
    set -e
    
    if [ $EXIT_CODE -eq 0 ]; then
        echo "✅ Entrypoint script exited cleanly (exit code 0)."
        exit 0
    else
        echo "⚠️ Warning: Entrypoint script exited with non-zero code ($EXIT_CODE)."
        echo "🔧 Falling back to Director REPL for interactive inspection and recovery..."
    fi
fi

# 3. Default: Launch Director REPL
cd "$WS_DIR"
exec wackypub --ws "$WS_DIR" agent director repl
