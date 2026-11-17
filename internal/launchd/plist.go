// Package launchd writes the periodic sweep LaunchAgent plist.
package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

// Label is the LaunchAgent label.
const Label = "session-index.sweep"

func esc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Write writes a LaunchAgent plist at plistPath that runs `<binaryPath> sweep
// --embed` every intervalMin minutes and at load.
func Write(plistPath, binaryPath string, intervalMin int) error {
	if intervalMin <= 0 {
		return fmt.Errorf("launchd: interval must be positive, got %d", intervalMin)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	logPath := filepath.Join(home, ".claude", "cache", "session-archive", "sweep.log")
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>sweep</string>
		<string>--embed</string>
	</array>
	<key>StartInterval</key>
	<integer>%d</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, Label, esc(binaryPath), intervalMin*60, esc(logPath), esc(logPath))
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(plistPath, []byte(body), 0o644)
}
