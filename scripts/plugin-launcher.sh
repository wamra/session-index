#!/bin/sh
# Runs the session-index binary bundled for this OS and CPU; else session-index from PATH.
# Exit 127 when neither can run, so callers can fall back or report it. Hook commands append
# "|| true", so a missing binary never fails a Claude Code session.
dir=$(cd "$(dirname "$0")" && pwd)
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  MINGW*|MSYS*|CYGWIN*) os=windows ;;
  *) os= ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|AMD64|amd64) arch=amd64 ;;
  *) arch= ;;
esac
if [ -n "$os" ] && [ -n "$arch" ]; then
  bin="$dir/session-index-$os-$arch"
  [ "$os" = windows ] && bin="$bin.exe"
  if [ -f "$bin" ]; then
    # A synced or copied plugin directory can lose the executable bit.
    [ -x "$bin" ] || chmod +x "$bin" 2>/dev/null
    [ -x "$bin" ] && exec "$bin" "$@"
  fi
fi
# This directory is on PATH too (plugin bin/), so skip it or the launcher would run itself.
oldifs=$IFS; IFS=:
for p in $PATH; do
  [ -n "$p" ] || continue
  [ "$(cd "$p" 2>/dev/null && pwd)" = "$dir" ] && continue
  for c in "$p/session-index" "$p/session-index.exe"; do
    if [ -f "$c" ] && [ -x "$c" ]; then IFS=$oldifs; exec "$c" "$@"; fi
  done
done
IFS=$oldifs
echo "session-index: no bundled binary for ${os:-unknown}/${arch:-unknown} in $dir and no session-index on PATH. Install session-index and put it on PATH, or set the plugin option \"binary\" to its full path." >&2
exit 127
