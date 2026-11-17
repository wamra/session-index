// Package jsonl parses single lines of Claude Code transcript JSONL files.
package jsonl

import (
	"encoding/json"
	"strings"
)

// Record is the parsed form of one transcript line.
type Record struct {
	Type, Subtype, UUID, ParentUUID, SessionID, Timestamp string
	IsSidechain                                           bool
	AITitle, CustomTitle                                  string
	Msg                                                   *Message
}

// Message holds the prose and tool data of a user/assistant line.
type Message struct {
	Role, Model string
	Texts       []string
	HasThinking bool
	ToolUses    []ToolUse
	ToolResults []string
	// ResultRefs describes each tool_result block in order (same length as ToolResults).
	ResultRefs []ResultRef
	Usage      struct{ Input, Output int }
	CostUSD    float64
}

// ResultRef identifies the tool_use a tool_result answers.
type ResultRef struct {
	ToolUseID string
	IsError   bool
}

// ToolUse is a tool_use content block.
type ToolUse struct {
	Name, ID  string
	InputJSON []byte
}

type rawRecord struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	UUID        string          `json:"uuid"`
	ParentUUID  string          `json:"parentUuid"`
	SessionID   string          `json:"sessionId"`
	Timestamp   string          `json:"timestamp"`
	IsSidechain bool            `json:"isSidechain"`
	AITitle     string          `json:"aiTitle"`
	CustomTitle string          `json:"customTitle"`
	CostUSD     float64         `json:"costUSD"`
	Message     json.RawMessage `json:"message"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// Parse decodes one JSONL line. Invalid JSON yields Record{Type:"<unparseable>"}
// and a non-nil error. Unknown types parse with Msg == nil.
func Parse(line []byte) (Record, error) {
	var raw rawRecord
	if err := json.Unmarshal(line, &raw); err != nil {
		// Valid JSON with type-mismatched fields is not an error: Unmarshal
		// keeps decoding past type errors, so use whatever fields decoded.
		if !json.Valid(line) {
			return Record{Type: "<unparseable>"}, err
		}
	}
	rec := Record{
		Type: raw.Type, Subtype: raw.Subtype, UUID: raw.UUID, ParentUUID: raw.ParentUUID,
		SessionID: raw.SessionID, Timestamp: raw.Timestamp, IsSidechain: raw.IsSidechain,
		AITitle: raw.AITitle, CustomTitle: raw.CustomTitle,
	}
	if len(raw.Message) == 0 {
		return rec, nil
	}
	var rm rawMessage
	if err := json.Unmarshal(raw.Message, &rm); err != nil && !isTypeError(err) {
		return rec, nil
	}
	if raw.Message[0] != '{' {
		return rec, nil
	}
	m := &Message{Role: rm.Role, Model: rm.Model, CostUSD: raw.CostUSD}
	m.Usage.Input = rm.Usage.Input
	m.Usage.Output = rm.Usage.Output
	parseContent(m, rm.Content)
	rec.Msg = m
	return rec, nil
}

func excluded(s string) bool {
	return strings.HasPrefix(s, "<system-reminder>") || strings.HasPrefix(s, "<local-command")
}

func addText(m *Message, s string) {
	if s != "" && !excluded(s) {
		m.Texts = append(m.Texts, s)
	}
}

func parseContent(m *Message, content json.RawMessage) {
	if len(content) == 0 {
		return
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		addText(m, s)
		return
	}
	for _, b := range decodeBlocks(content) {
		switch b.Type {
		case "text":
			addText(m, b.Text)
		case "thinking":
			m.HasThinking = true
		case "tool_use":
			m.ToolUses = append(m.ToolUses, ToolUse{Name: b.Name, ID: b.ID, InputJSON: []byte(b.Input)})
		case "tool_result":
			m.ToolResults = append(m.ToolResults, resultText(b.Content))
			m.ResultRefs = append(m.ResultRefs, ResultRef{ToolUseID: b.ToolUseID, IsError: b.IsError})
		}
	}
}

// resultText flattens tool_result content (string or list of text blocks).
func resultText(c json.RawMessage) string {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s
	}
	var parts []string
	for _, b := range decodeBlocks(c) {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// decodeBlocks decodes each array element separately, skipping malformed ones.
func decodeBlocks(c json.RawMessage) []block {
	var elems []json.RawMessage
	if json.Unmarshal(c, &elems) != nil {
		return nil
	}
	blocks := make([]block, 0, len(elems))
	for _, e := range elems {
		var b block
		if json.Unmarshal(e, &b) == nil {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

func isTypeError(err error) bool {
	_, ok := err.(*json.UnmarshalTypeError)
	return ok
}
