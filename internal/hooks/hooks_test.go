package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStdinHook(t *testing.T) {
	in, err := ParseStdin(strings.NewReader(`{"session_id":"abc","transcript_path":"/x/y/abc.jsonl","cwd":"/w","hook_event_name":"Stop","extra":1} trailing garbage`))
	if err != nil {
		t.Fatal(err)
	}
	if in.SessionID != "abc" || in.TranscriptPath != "/x/y/abc.jsonl" || in.HookEventName != "Stop" {
		t.Fatalf("%+v", in)
	}
	if _, err := ParseStdin(strings.NewReader("")); err == nil {
		t.Fatal("empty input must error")
	}
	if _, err := ParseStdin(strings.NewReader(`{"session_id":"abc"}`)); err == nil {
		t.Fatal("missing transcript_path must error")
	}
}

const fixture = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls)"]},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "python3 /a/snap.py"}]}],
    "SessionEnd": [{"matcher": "", "hooks": [{"type": "command", "command": "echo end", "timeout": 5}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "python3 /a/start.py"}]}]
  }
}`

func cmds(t *testing.T, m map[string]any, event string) []string {
	t.Helper()
	var out []string
	for _, e := range m["hooks"].(map[string]any)[event].([]any) {
		for _, h := range e.(map[string]any)["hooks"].([]any) {
			out = append(out, h.(map[string]any)["command"].(string))
		}
	}
	return out
}

func TestInstallHooksIdempotentAndPreserves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(p, []byte(fixture), 0o600)
	bin := "/opt/bin/session-index"
	want := bin + " ingest --stdin-hook --embed"

	changed, err := Install(p, bin, true)
	if err != nil || !changed {
		t.Fatalf("dry run: %v %v", changed, err)
	}
	if b, _ := os.ReadFile(p); string(b) != fixture {
		t.Fatal("dry run wrote the file")
	}

	changed, err = Install(p, bin, false)
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	var m map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "opus" || m["permissions"] == nil {
		t.Fatalf("unrelated keys lost: %v", m)
	}
	for ev, old := range map[string]string{"Stop": "python3 /a/snap.py", "SessionEnd": "echo end", "SessionStart": "python3 /a/start.py"} {
		cs := cmds(t, m, ev)
		if cs[0] != old {
			t.Errorf("%s: existing entry lost: %v", ev, cs)
		}
		n := 0
		for _, c := range cs {
			if c == want {
				n++
			}
		}
		if ev == "SessionStart" && n != 0 {
			t.Errorf("SessionStart must not get our hook")
		}
		if ev != "SessionStart" && n != 1 {
			t.Errorf("%s: our entry count %d: %v", ev, n, cs)
		}
	}
	se := m["hooks"].(map[string]any)["SessionEnd"].([]any)[0].(map[string]any)
	if se["matcher"] != "" || se["hooks"].([]any)[0].(map[string]any)["timeout"] != float64(5) {
		t.Errorf("existing entry fields altered: %v", se)
	}
	if cs := cmds(t, m, "SubagentStop"); len(cs) != 1 || cs[0] != want {
		t.Errorf("SubagentStop: %v", cs)
	}
	ours := m["hooks"].(map[string]any)["Stop"].([]any)[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if ours["type"] != "command" || ours["timeout"] != float64(10) || ours["async"] != true {
		t.Errorf("our entry: %v", ours)
	}

	changed, err = Install(p, bin, false)
	if err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	b2, _ := os.ReadFile(p)
	if string(b2) != string(b) {
		t.Fatal("second install modified file")
	}
}

func TestInstallCreatesMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "settings.json")
	if changed, err := Install(p, "/b/session-index", false); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
}

func TestInstallPreservesNumbersNullsAndUnknown(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	orig := `{"big": 12345678901234, "f": 1000000.5, "n": null, "unknown": {"a": {"b": [1, null, 2.50, "x"]}},
 "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "other"}]}]}}`
	os.WriteFile(p, []byte(orig), 0o600)
	if changed, err := Install(p, "/b/session-index", false); err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	dec := func(b []byte) map[string]any {
		var m map[string]any
		d := json.NewDecoder(strings.NewReader(string(b)))
		d.UseNumber()
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	after, _ := os.ReadFile(p)
	a, o := dec(after), dec([]byte(orig))
	for _, k := range []string{"big", "f", "n", "unknown"} {
		x, _ := json.Marshal(a[k])
		y, _ := json.Marshal(o[k])
		if string(x) != string(y) {
			t.Errorf("%s: %s != %s", k, x, y)
		}
	}
	if !strings.Contains(string(after), "12345678901234") || !strings.Contains(string(after), "1000000.5") {
		t.Errorf("numbers rewritten: %s", after)
	}
	if bak, err := os.ReadFile(p + ".bak-session-index"); err != nil || string(bak) != orig {
		t.Errorf("backup: %v %q", err, bak)
	}
}

func TestInstallBackupOnceAndSymlinkAndLegacyMarker(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "settings.json")
	os.WriteFile(real, []byte(`{"a":1}`), 0o600)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(link, "/b/session-index", false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), "ingest --stdin-hook") {
		t.Fatalf("real file not updated: %s", b)
	}
	// Second install with a different binary path: already present, so no change and backup untouched.
	if changed, _ := Install(link, "/other/session-index", false); changed {
		t.Fatal("changed on second install")
	}
	if b, _ := os.ReadFile(real + ".bak-session-index"); string(b) != `{"a":1}` {
		t.Fatalf("backup %q", b)
	}
	// A command that only matches "ingest --stdin-hook" counts as installed.
	p2 := filepath.Join(dir, "s2.json")
	os.WriteFile(p2, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/sa ingest --stdin-hook"}]}],"SubagentStop":[{"hooks":[{"type":"command","command":"/x/sa ingest --stdin-hook"}]}],"SessionEnd":[{"hooks":[{"type":"command","command":"/x/sa ingest --stdin-hook"}]}]}}`), 0o600)
	if changed, err := Install(p2, "/b/session-index", false); err != nil || changed {
		t.Fatalf("%v %v", changed, err)
	}
}

func TestCommandQuotesPathsWithSpaces(t *testing.T) {
	if got := Command("/a b/session-index"); got != `"/a b/session-index" ingest --stdin-hook --embed` {
		t.Fatalf("got %q", got)
	}
	if got := Command("/usr/bin/session-index"); got != "/usr/bin/session-index ingest --stdin-hook --embed" {
		t.Fatalf("got %q", got)
	}
}
