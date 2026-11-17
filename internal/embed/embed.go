// Package embed turns pending chunks into vectors through a
// provider.Embedder (Ollama, OpenAI-compatible, ...).
package embed

import (
	"context"
	"encoding/binary"
	"errors"
	"math"

	"github.com/wamra/session-index/internal/provider"
)

const (
	// DocPrefix and QueryPrefix are embeddinggemma's prompt prefixes (the
	// historical defaults; the configured embedder supplies its own).
	DocPrefix   = provider.GemmaDocPrefix
	QueryPrefix = provider.GemmaQueryPrefix
	maxChunk    = provider.MaxChunk
)

var (
	// ErrOllamaDown is the old name of provider.ErrDown.
	ErrOllamaDown = provider.ErrDown
	// ErrEmbedderBroken is the old name of provider.ErrBroken.
	ErrEmbedderBroken      = provider.ErrBroken
	ErrLocked              = errors.New("embed lock held by another process")
	ErrFingerprintMismatch = errors.New("embedder fingerprint mismatch; run rebuild-derived")
)

// Client is the Ollama embedder (kept for callers that build one directly).
type Client = provider.OllamaEmbedder

// NewClient returns an Ollama embedder for model with that model's known
// prefixes and dimensions.
func NewClient(baseURL, model string) *Client {
	e, _ := provider.NewEmbedder(provider.EmbedConfig{Provider: provider.Ollama, BaseURL: baseURL, Model: model})
	return e.(*Client)
}

// Query embeds one search query with e's query prefix.
func Query(ctx context.Context, e provider.Embedder, q string) ([]float32, error) {
	return provider.Query(ctx, e, q)
}

// VecBytes encodes v as little-endian float32.
func VecBytes(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func capRunes(s string, n int) string { return provider.CapRunes(s, n) }
