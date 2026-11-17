// Package summarize generates short summaries of tool outputs, tool-result
// files and file changes with a provider.Summarizer (Ollama, OpenAI-compatible,
// ...) and stores them in the archive (summaries table plus a chunks row for
// search).
package summarize

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wamra/session-index/internal/provider"
)

// DefaultModel is the default summarizer model.
const DefaultModel = provider.DefaultSummarizeModel

// Prompts (system messages).
const (
	ToolPrompt   = "Summarize this tool output for a developer's search index in 2-3 plain sentences: what command or action produced it, what it shows (key files, errors, numbers), and the outcome. No preamble, no markdown."
	ChangePrompt = "Summarize this single file change for a developer's change log in one sentence: the file, what was changed (name functions/sections), and the intent if evident. No preamble, no markdown."
)

// ErrItem marks a failure of one item (not the server): the item is parked.
type ErrItem = provider.ItemError

// Client is the Ollama summarizer (kept for callers that build one directly).
type Client = provider.OllamaSummarizer

// NewClient returns an Ollama summarizer with the default temperature.
func NewClient(baseURL, model string) *Client {
	s, _ := provider.NewSummarizer(provider.SummarizeConfig{Provider: provider.Ollama, BaseURL: baseURL, Model: model})
	return s.(*Client)
}

// generate asks c for a summary of user under system. The reply is trimmed
// and capped to 800 characters; an empty reply is an *ErrItem. Server-side
// failures keep the provider's classification (provider.ErrDown).
func generate(ctx context.Context, c provider.Summarizer, system, user string, maxTokens int) (string, error) {
	s, err := c.Summarize(ctx, system, user, maxTokens)
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", &ErrItem{Reason: "empty"}
	}
	if r := []rune(s); len(r) > 800 {
		s = string(r[:800])
	}
	return s, nil
}

// Down reports whether err means the provider is unreachable.
func Down(err error) bool { return errors.Is(err, provider.ErrDown) }

// Clip keeps the first 6,000 and last 2,000 characters of text longer than
// 8,000 characters (the default max_input_chars).
func Clip(text string) string { return ClipTo(text, provider.DefaultMaxInputChars) }

// ClipTo keeps the first 3/4 and the last 1/4 of the first max characters
// budget of text longer than max characters (max 8000 keeps 6,000 + 2,000).
// max <= 0 means the default.
func ClipTo(text string, max int) string {
	if max <= 0 {
		max = provider.DefaultMaxInputChars
	}
	r := []rune(text)
	if len(r) <= max {
		return text
	}
	head := max * 3 / 4
	return string(r[:head]) + "\n[...]\n" + string(r[len(r)-(max-head):])
}

// KindFromConfig maps a summarize.kinds name (tool_output, file_change, asset)
// to the internal kind.
func KindFromConfig(name string) (string, bool) {
	switch name {
	case "tool_output":
		return KindTool, true
	case "file_change":
		return KindChange, true
	case "asset":
		return KindAsset, true
	}
	return "", false
}

// KindsFromConfig maps summarize.kinds; an empty list means all kinds.
func KindsFromConfig(names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		k, ok := KindFromConfig(n)
		if !ok {
			return nil, fmt.Errorf("unknown summarize kind %q", n)
		}
		out = append(out, k)
	}
	return out, nil
}
