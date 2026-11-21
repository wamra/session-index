#!/usr/bin/env bash
# Gate for a staged plugin tree: CI runs it before publishing, so a bad build never replaces the
# good plugin branch. Usage: scripts/check-plugin.sh OUT [EXPECTED_VERSION_TEXT]
set -euo pipefail
OUT="${1:?usage: check-plugin.sh OUT [EXPECTED_VERSION_TEXT]}"; WANT="${2:-}"
fail() { echo "check-plugin: $*" >&2; exit 1; }
for t in darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64.exe windows-arm64.exe; do
  f="$OUT/bin/session-index-$t"; [ -f "$f" ] || fail "missing $f"
  [ "$(wc -c < "$f")" -gt 5000000 ] || fail "$f is smaller than 5 MB"
done
[ -f "$OUT/bin/session-index" ] || fail "missing the launcher"
[ -z "$(find "$OUT/bin" -type f ! -perm -u+x)" ] || fail "a file in bin/ is not executable"
[ ! -e "$OUT/plugins" ] || fail "nested plugins/ must not be shipped"
[ "$(du -sk "$OUT" | cut -f1)" -lt 120000 ] || fail "tree is larger than 120 MB"
node - "$OUT" <<'JS' || fail "manifest check failed"
const fs = require('fs'), out = process.argv[2]
const plugin = JSON.parse(fs.readFileSync(`${out}/.claude-plugin/plugin.json`, 'utf8'))
if ('version' in plugin) throw new Error('plugin.json pins a version')
if (!plugin.userConfig || !plugin.userConfig.binary) throw new Error('plugin.json has no binary option')
const hooks = JSON.parse(fs.readFileSync(`${out}/hooks/hooks.json`, 'utf8'))
if (!Array.isArray(hooks.modules) || !fs.existsSync(`${out}/hooks/${hooks.modules[0]}`)) throw new Error('hooks.json modules do not resolve')
const cmds = Object.values(hooks.hooks).flat().flatMap((g) => g.hooks.map((h) => h.command))
if (cmds.length === 0 || !cmds.every((c) => c.includes('${CLAUDE_PLUGIN_ROOT}/bin/session-index') && c.endsWith('|| true'))) throw new Error('hook commands do not use the bundled launcher')
JS
# With no bundled binary and nothing on PATH the launcher must exit 127, not loop or hang.
iso="$(mktemp -d)"; cp "$OUT/bin/session-index" "$iso/"; code=0
( PATH="$iso:/usr/bin:/bin" "$iso/session-index" version >/dev/null 2>&1 ) || code=$?
[ "$code" = 127 ] || fail "launcher without a binary exited $code, not 127"
got="$("$OUT/bin/session-index" version)" || fail "launcher could not run the bundled binary"
[ -z "$WANT" ] || case "$got" in *"$WANT"*) ;; *) fail "version '$got' does not contain '$WANT'" ;; esac
echo "check-plugin: ok ($got)"
