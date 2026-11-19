#!/bin/sh
# Picks the session-index binary built for this OS and CPU and runs it.
dir=$(cd "$(dirname "$0")" && pwd)
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  MINGW*|MSYS*|CYGWIN*) os=windows ;;
  *) echo "session-index: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|AMD64|amd64) arch=amd64 ;;
  *) echo "session-index: unsupported CPU $(uname -m)" >&2; exit 1 ;;
esac
bin="$dir/session-index-$os-$arch"
[ "$os" = windows ] && bin="$bin.exe"
[ -f "$bin" ] || { echo "session-index: no binary for $os/$arch in $dir" >&2; exit 1; }
exec "$bin" "$@"
