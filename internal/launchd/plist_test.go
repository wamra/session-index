package launchd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "LaunchAgents", "x.plist")
	if err := Write(p, "/opt/bin/session-index", 10); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, w := range []string{
		"<string>session-index.sweep</string>",
		"<string>/opt/bin/session-index</string>",
		"<string>sweep</string>", "<string>--embed</string>",
		"<key>StartInterval</key>\n\t<integer>600</integer>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"StandardOutPath", "StandardErrorPath",
		"/.claude/cache/session-archive/sweep.log",
	} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
	if strings.Contains(s, "~") {
		t.Error("log path must be expanded")
	}
}
