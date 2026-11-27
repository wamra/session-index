package main

import (
	"strings"
	"testing"
)

func TestNewQwen3ArchiveWithOllamaDownIsSizedFromTheModelName(t *testing.T) {
	seedRoot(t)
	useConfig(t, `{"embed":{"model":"qwen3-embedding:4b"}}`)
	t.Setenv("SESSION_INDEX_OLLAMA", "http://127.0.0.1:1")
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	if metaOf(t, "vec_dims") != "2560" || !strings.HasPrefix(metaOf(t, "embedder_fingerprint"), "ollama|qwen3-embedding:4b|2560|") {
		t.Fatalf("meta %s %s", metaOf(t, "vec_dims"), metaOf(t, "embedder_fingerprint"))
	}
}
