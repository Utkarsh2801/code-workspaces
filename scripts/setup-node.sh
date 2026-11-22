#!/bin/sh
set -eu

TOOLS_DIR=/workspace/tools/node
MARKER=/workspace/.node-22-installed

if [ -f "$MARKER" ] && [ -x "$TOOLS_DIR/bin/node" ]; then
  "$TOOLS_DIR/bin/node" --version
  exit 0
fi

mkdir -p "$TOOLS_DIR/bin"
cp -f /usr/local/bin/node "$TOOLS_DIR/bin/node"

"$TOOLS_DIR/bin/node" --version
: > "$MARKER"
