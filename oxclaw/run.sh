#!/usr/bin/env bash
# run.sh — Builds (if needed) and runs the oxclaw-channel MCP server.
# Called by Claude Code via .mcp.json.
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$DIR/bin/oxclaw-channel"
SRC="$DIR/src"

# Build if binary doesn't exist or source is newer
if [ ! -f "$BIN" ] || [ "$SRC/main.go" -nt "$BIN" ]; then
  if ! command -v go &>/dev/null; then
    echo "oxclaw-channel: Go is required but not installed. Install from https://go.dev/dl/" >&2
    exit 1
  fi
  mkdir -p "$DIR/bin"
  (cd "$SRC" && go build -o "$BIN" .) >&2
fi

exec "$BIN"
