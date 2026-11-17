package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.TrimRight(string(b), "\n"))
}

func parse(t *testing.T, name string) Record {
	t.Helper()
	r, err := Parse(load(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return r
}

func TestParseUserString(t *testing.T) {
	r := parse(t, "user_string.jsonl")
	if r.Type != "user" || r.UUID != "u1" || r.ParentUUID != "" || r.SessionID != "s1" || r.IsSidechain {
		t.Fatalf("bad envelope: %+v", r)
	}
	if r.Timestamp != "2026-01-02T03:04:05.678Z" {
		t.Fatalf("timestamp %q", r.Timestamp)
	}
	if r.Msg == nil || r.Msg.Role != "user" || len(r.Msg.Texts) != 1 || r.Msg.Texts[0] != "hello synthetic" {
		t.Fatalf("bad msg: %+v", r.Msg)
	}
}

func TestParseToolResultListContent(t *testing.T) {
	r := parse(t, "tool_result.jsonl")
	if r.Msg == nil || len(r.Msg.ToolResults) != 2 {
		t.Fatalf("bad msg: %+v", r.Msg)
	}
	if r.Msg.ToolResults[0] != "plain result" || r.Msg.ToolResults[1] != "list result" {
		t.Fatalf("results: %q", r.Msg.ToolResults)
	}
	if len(r.Msg.Texts) != 0 {
		t.Fatalf("texts should be empty: %q", r.Msg.Texts)
	}
}

func TestParseAssistantBlocks(t *testing.T) {
	r := parse(t, "assistant.jsonl")
	m := r.Msg
	if m == nil || m.Role != "assistant" || m.Model != "claude-test" {
		t.Fatalf("bad msg: %+v", m)
	}
	if len(m.Texts) != 2 || m.Texts[0] != "first text" || m.Texts[1] != "second text" {
		t.Fatalf("texts: %q", m.Texts)
	}
	if !m.HasThinking {
		t.Fatal("HasThinking false")
	}
	if len(m.ToolUses) != 1 || m.ToolUses[0].Name != "Bash" || m.ToolUses[0].ID != "toolu_x" ||
		string(m.ToolUses[0].InputJSON) != `{"command":"ls"}` {
		t.Fatalf("tool uses: %+v", m.ToolUses)
	}
	if m.Usage.Input != 1 || m.Usage.Output != 2 || m.CostUSD != 0.5 {
		t.Fatalf("usage/cost: %+v %v", m.Usage, m.CostUSD)
	}
	for _, s := range m.Texts {
		if strings.Contains(s, "secret") {
			t.Fatal("thinking leaked into Texts")
		}
	}
}

func TestParseTitles(t *testing.T) {
	if r := parse(t, "ai_title.jsonl"); r.Type != "ai-title" || r.AITitle != "An AI title" || r.SessionID != "s1" {
		t.Fatalf("%+v", r)
	}
	if r := parse(t, "custom_title.jsonl"); r.Type != "custom-title" || r.CustomTitle != "My custom title" {
		t.Fatalf("%+v", r)
	}
}

func TestParseUnknownTypeKeepsTypeOnly(t *testing.T) {
	for name, typ := range map[string]string{
		"attachment.jsonl": "attachment", "file_history_snapshot.jsonl": "file-history-snapshot",
		"queue_operation.jsonl": "queue-operation", "progress.jsonl": "progress",
	} {
		r := parse(t, name)
		if r.Type != typ || r.Msg != nil {
			t.Fatalf("%s: %+v", name, r)
		}
	}
	if r := parse(t, "system.jsonl"); r.Type != "system" || r.Subtype != "compact_boundary" || r.Msg != nil {
		t.Fatalf("system: %+v", r)
	}
}

func TestParseMalformedReturnsError(t *testing.T) {
	r, err := Parse(load(t, "malformed.jsonl"))
	if err == nil {
		t.Fatal("expected error")
	}
	if r.Type != "<unparseable>" {
		t.Fatalf("type %q", r.Type)
	}
}

func TestParseSkipsSystemReminderText(t *testing.T) {
	r := parse(t, "reminder.jsonl")
	if r.Msg == nil || len(r.Msg.Texts) != 1 || r.Msg.Texts[0] != "keep me" {
		t.Fatalf("texts: %+v", r.Msg)
	}
}

func TestParseSystemReminderStringContent(t *testing.T) {
	r, err := Parse([]byte(`{"type":"user","message":{"role":"user","content":"<system-reminder>x</system-reminder>"}}`))
	if err != nil || r.Msg == nil || len(r.Msg.Texts) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestParseBigLine(t *testing.T) {
	b := load(t, "big_line.jsonl")
	if len(b) < 2_000_000 {
		t.Fatalf("fixture too small: %d", len(b))
	}
	r, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg == nil || len(r.Msg.Texts) != 1 || r.Msg.Texts[0] != "after image" {
		t.Fatalf("%+v", r.Msg)
	}
}

func TestParseTypeMismatchIsNotError(t *testing.T) {
	for _, line := range []string{
		`{"type":"progress","message":"x"}`,
		`{"type":"progress","timestamp":123}`,
	} {
		r, err := Parse([]byte(line))
		if err != nil || r.Type != "progress" || r.Msg != nil {
			t.Fatalf("%s: %+v %v", line, r, err)
		}
	}
}

func TestParseSkipsMalformedBlock(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"good"},{"type":"tool_use","id":5,"name":"Bad"},{"type":"tool_use","id":"t2","name":"Ok","input":{}}]}}`
	r, err := Parse([]byte(line))
	if err != nil || r.Msg == nil {
		t.Fatalf("%+v %v", r, err)
	}
	if len(r.Msg.Texts) != 1 || r.Msg.Texts[0] != "good" {
		t.Fatalf("texts: %q", r.Msg.Texts)
	}
	if len(r.Msg.ToolUses) != 1 || r.Msg.ToolUses[0].Name != "Ok" {
		t.Fatalf("tool uses: %+v", r.Msg.ToolUses)
	}
}
