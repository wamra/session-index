// Package hooks parses Claude Code hook stdin and installs the session-index
// hook entries into a settings.json.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// HookInput is the subset of the hook JSON object that session-index uses.
type HookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	HookEventName  string `json:"hook_event_name"`
}

// ParseStdin decodes exactly one JSON value from r (it does not read to EOF).
func ParseStdin(r io.Reader) (HookInput, error) {
	var in HookInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return HookInput{}, fmt.Errorf("hook input: %w", err)
	}
	if in.TranscriptPath == "" {
		return HookInput{}, errors.New("hook input: missing transcript_path")
	}
	return in, nil
}

// Events are the hook events that receive the ingest hook.
var Events = []string{"Stop", "SubagentStop", "SessionEnd"}

// Markers identify our entries in existing commands.
var Markers = []string{"session-index ingest", "session-archive ingest", "ingest --stdin-hook"}

// Command is the hook command line for binaryPath.
func Command(binaryPath string) string {
	if strings.ContainsAny(binaryPath, " \t") {
		binaryPath = `"` + binaryPath + `"`
	}
	return binaryPath + " ingest --stdin-hook --embed"
}

// Install adds the ingest hook to settingsPath for each of Events unless that
// event already has a command containing Marker. All other content is kept.
// With dryRun nothing is written. A missing file is treated as empty.
func Install(settingsPath, binaryPath string, dryRun bool) (changed bool, err error) {
	out, changed, err := Render(settingsPath, binaryPath)
	if err != nil || !changed || dryRun {
		return changed, err
	}
	// Write through a symlinked settings.json rather than replacing the link.
	if r, err := filepath.EvalSymlinks(settingsPath); err == nil {
		settingsPath = r
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return false, err
	}
	if err := backupOnce(settingsPath); err != nil {
		return false, err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(settingsPath); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(settingsPath), ".settings-*.json")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), settingsPath)
}

// Render returns the settings JSON that Install would write and whether it
// differs in meaning from the current file.
func Render(settingsPath, binaryPath string) (out []byte, changed bool, err error) {
	root := map[string]any{}
	b, err := os.ReadFile(settingsPath)
	switch {
	case err == nil && len(bytes.TrimSpace(b)) > 0:
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return nil, false, fmt.Errorf("%s: %w", settingsPath, err)
		}
	case err != nil && !os.IsNotExist(err):
		return nil, false, err
	}
	hk, ok := root["hooks"].(map[string]any)
	if root["hooks"] != nil && !ok {
		return nil, false, fmt.Errorf("%s: \"hooks\" is not an object", settingsPath)
	}
	if hk == nil {
		hk = map[string]any{}
	}
	for _, ev := range Events {
		list, ok := hk[ev].([]any)
		if hk[ev] != nil && !ok {
			return nil, false, fmt.Errorf("%s: hooks.%s is not an array", settingsPath, ev)
		}
		if hasMarker(list) {
			continue
		}
		list = append(list, map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": Command(binaryPath), "timeout": 10, "async": true,
		}}})
		hk[ev] = list
		changed = true
	}
	root["hooks"] = hk
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), changed, nil
}

func hasMarker(list []any) bool {
	for _, e := range list {
		em, _ := e.(map[string]any)
		hs, _ := em["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			c, _ := hm["command"].(string)
			for _, m := range Markers {
				if strings.Contains(c, m) {
					return true
				}
			}
		}
	}
	return false
}

// HasHook reports whether settingsPath contains our ingest hook in any event.
func HasHook(settingsPath string) bool {
	b, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	var root struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if json.Unmarshal(b, &root) != nil {
		return false
	}
	for _, ev := range Events {
		if hasMarker(root.Hooks[ev]) {
			return true
		}
	}
	return false
}

// backupOnce copies an existing settings file to <path>.bak-session-index
// unless that backup already exists.
func backupOnce(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	bak := path + ".bak-session-index"
	if _, err := os.Stat(bak); err == nil {
		return nil
	}
	return os.WriteFile(bak, b, 0o600)
}
