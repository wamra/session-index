package launchd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
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
		filepath.Join(home, ".claude", "cache", "session-archive", "sweep.log"),
	} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
	// ">~", not "~": a Windows temp dir can hold a short name such as RUNNER~1
	if strings.Contains(s, ">~") {
		t.Error("log path must be expanded")
	}
}
