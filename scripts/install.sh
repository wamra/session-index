#!/usr/bin/env bash
# Install session-index from a local checkout (macOS or Linux).
# Builds the binary with Go, puts it in ~/.local/bin, installs the Claude Code
# hooks and the session-search skill, then runs doctor.
# Usage: scripts/install.sh [--bin-dir DIR] [--no-hooks]
set -euo pipefail

BIN_DIR="${HOME}/.local/bin"
HOOKS=1
while [ "$#" -gt 0 ]; do
  case "$1" in
    --bin-dir) BIN_DIR="$2"; shift 2 ;;
    --no-hooks) HOOKS=0; shift ;;
    -h|--help) sed -n '2,6p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v go >/dev/null 2>&1 || { echo "Go is required to build from source: https://go.dev/dl/" >&2; exit 1; }

mkdir -p "$BIN_DIR"
echo "== building session-index"
( cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)" -o "$BIN_DIR/session-index.new" ./cmd/session-index )
mv -f "$BIN_DIR/session-index.new" "$BIN_DIR/session-index"
echo "installed $BIN_DIR/session-index ($("$BIN_DIR/session-index" version))"

case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "NOTE: $BIN_DIR is not on PATH; add it to your shell profile." ;; esac

if [ "$HOOKS" = 1 ]; then
  echo "== installing Claude Code hooks and skill"
  "$BIN_DIR/session-index" install-hooks --skill
fi
echo "== doctor"
"$BIN_DIR/session-index" doctor || true
echo
echo "Next: session-index backfill --embed    (index your existing sessions)"
