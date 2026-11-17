#!/usr/bin/env bash
# Cross-build release archives and checksums into dist/ with plain Go tooling.
# Same targets and archive layout as .goreleaser.yaml; use this where goreleaser
# is not installed. Usage: scripts/build-release.sh [VERSION]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="$ROOT/dist"
rm -rf "$OUT"; mkdir -p "$OUT"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"
  name="session-index_${VERSION}_${os}_${arch}"
  stage="$OUT/$name"; mkdir -p "$stage"
  exe=session-index; [ "$os" = windows ] && exe=session-index.exe
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$stage/$exe" ./cmd/session-index )
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$stage/"
  cp "$ROOT/skills/session-search/SKILL.md" "$stage/SKILL.md"
  if [ "$os" = windows ]; then ( cd "$OUT" && zip -qr "$name.zip" "$name" ); else tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name"; fi
  rm -rf "$stage"
done
( cd "$OUT" && shasum -a 256 *.tar.gz *.zip > checksums.txt )
ls -l "$OUT"
