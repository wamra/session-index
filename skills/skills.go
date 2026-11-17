// Package skills embeds the Claude Code skill files installed by
// `session-index install-hooks --skill`.
package skills

import _ "embed"

// SessionSearch is skills/session-search/SKILL.md.
//
//go:embed session-search/SKILL.md
var SessionSearch []byte
