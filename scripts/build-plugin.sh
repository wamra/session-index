#!/usr/bin/env bash
# Stage the installable plugin tree, with the six binaries inside, into OUT.
# CI force-pushes OUT as the orphan "plugin" branch. Usage: scripts/build-plugin.sh OUT [VERSION]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:?usage: build-plugin.sh OUT [VERSION]}"
VERSION="${2:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
rm -rf "$OUT"; mkdir -p "$OUT/.claude-plugin" "$OUT/hooks" "$OUT/bin" "$OUT/plugins/session-index-search/.claude-plugin" "$OUT/plugins/session-index-search/hooks"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"; ext=""; [ "$os" = windows ] && ext=".exe"
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/bin/session-index-$os-$arch$ext" ./cmd/session-index )
done
cp "$ROOT/scripts/plugin-launcher.sh" "$OUT/bin/session-index"
cp "$ROOT/.claude-plugin/plugin.json" "$OUT/.claude-plugin/"
# Hooks call the bundled launcher; "sh" does not need the executable bit on the script.
sed 's#"session-index ingest#"sh \\"${CLAUDE_PLUGIN_ROOT}/bin/session-index\\" ingest#' "$ROOT/hooks/hooks.json" > "$OUT/hooks/hooks.json"
cp -R "$ROOT/skills/session-search" "$OUT/skills-tmp" && mkdir -p "$OUT/skills" && mv "$OUT/skills-tmp" "$OUT/skills/session-search" && rm -f "$OUT/skills/session-search/"*.go
# The search mod finds the launcher through $.plugin.root, so it carries its own bin/ (git stores identical files once).
S="$ROOT/plugins/session-index-search"
cp "$S/.claude-plugin/plugin.json" "$OUT/plugins/session-index-search/.claude-plugin/"
cp "$S/hooks/hooks.json" "$S/hooks/register.ts" "$OUT/plugins/session-index-search/hooks/"
cp -R "$OUT/bin" "$OUT/plugins/session-index-search/bin"
cp "$ROOT/LICENSE" "$ROOT/README.md" "$OUT/"
chmod +x "$OUT"/bin/* "$OUT"/plugins/session-index-search/bin/*
