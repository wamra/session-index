#!/usr/bin/env bash
# Stage the installable plugin into OUT: one plugin with the archive hooks, the search skill, the
# search mod and the six binaries. CI force-pushes OUT as the orphan "plugin" branch.
# Usage: scripts/build-plugin.sh OUT [VERSION]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:?usage: build-plugin.sh OUT [VERSION]}"
VERSION="${2:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
MOD="$ROOT/plugins/session-index-search"
rm -rf "$OUT"; mkdir -p "$OUT/.claude-plugin" "$OUT/hooks" "$OUT/bin" "$OUT/skills/session-search"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"; ext=""; [ "$os" = windows ] && ext=".exe"
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/bin/session-index-$os-$arch$ext" ./cmd/session-index )
done
cp "$ROOT/scripts/plugin-launcher.sh" "$OUT/bin/session-index"
cp "$MOD/hooks/register.ts" "$OUT/hooks/"
# The engine names a mod's tools mcp__<plugin>__<name> and the mod ships inside the "session-index"
# plugin, so the tool.call matchers (and the option name in the error text) follow that name.
perl -pi -e 's/mcp__session-index-search__/mcp__session-index__/g; s/session-index-search "binary"/session-index "binary"/g' "$OUT/hooks/register.ts"
cp "$ROOT/skills/session-search/SKILL.md" "$OUT/skills/session-search/"
cp "$ROOT/LICENSE" "$ROOT/README.md" "$OUT/"
# plugin.json: the archive plugin plus the mod's "binary" option, with no pinned version (the
# commit SHA is then the version, so a rebuilt branch reaches users). hooks.json: the archive
# hooks call the bundled launcher and never fail the session, plus the mod.
node - "$ROOT" "$MOD" "$OUT" <<'JS'
const fs = require('fs'), [root, mod, out] = process.argv.slice(2)
const read = (p) => JSON.parse(fs.readFileSync(p, 'utf8'))
const plugin = read(`${root}/.claude-plugin/plugin.json`)
delete plugin.version
plugin.description = 'Lossless, searchable index of your Claude Code sessions: hooks keep it current, a skill and the search and show tools query it, /sessions prints hits. Binaries for macOS, Linux and Windows are bundled.'
plugin.userConfig = read(`${mod}/.claude-plugin/plugin.json`).userConfig
fs.writeFileSync(`${out}/.claude-plugin/plugin.json`, JSON.stringify(plugin, null, 2) + '\n')
const hooks = read(`${root}/hooks/hooks.json`)
for (const groups of Object.values(hooks.hooks)) for (const g of groups) for (const h of g.hooks) {
  if (!h.command.startsWith('session-index ')) throw new Error(`unexpected hook command: ${h.command}`)
  h.command = `sh "\${CLAUDE_PLUGIN_ROOT}/bin/session-index" ${h.command.slice('session-index '.length)} || true`
}
hooks.modules = read(`${mod}/hooks/hooks.json`).modules
fs.writeFileSync(`${out}/hooks/hooks.json`, JSON.stringify(hooks, null, 2) + '\n')
JS
chmod +x "$OUT"/bin/*
