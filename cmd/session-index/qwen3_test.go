package main

import (
	"strings"
	"testing"

	"github.com/wamra/session-index/internal/provider"
)

const wantQwen3Query = "Instruct: Given a search query about a past coding session, retrieve the relevant conversation passages\nQuery:"

func TestExplicitQwen3ModelUsesInstructionPrefix(t *testing.T) {
	useConfig(t, `{"embed":{"model":"qwen3-embedding:0.6b"}}`)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Embed
	if e.Model != "qwen3-embedding:0.6b" || *e.DocPrefix != "" || *e.QueryPrefix != wantQwen3Query || e.Dims != 0 {
		t.Fatalf("%+v", e)
	}
}

func TestDefaultEmbedModelStaysEmbeddingGemma(t *testing.T) {
	useConfig(t, "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Embed
	if e.Model != "embeddinggemma" || e.Dims != 768 || *e.DocPrefix != provider.GemmaDocPrefix || *e.QueryPrefix != provider.GemmaQueryPrefix {
		t.Fatalf("%+v", e)
	}
}

func TestQwen3AgainstStoredGemmaArchiveIsAMismatch(t *testing.T) {
	seedRoot(t)
	useConfig(t, "")
	srv := fakeOllama(t)
	t.Setenv("SESSION_INDEX_OLLAMA", srv.URL)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	if fp := metaOf(t, "embedder_fingerprint"); !strings.HasPrefix(fp, "ollama|embeddinggemma|768|") {
		t.Fatalf("stored %q", fp)
	}
	useConfig(t, `{"embed":{"model":"qwen3-embedding:0.6b"}}`)
	code, _, e := runCLI(t, "embed")
	if code != 1 || !strings.Contains(e, "fingerprint mismatch") || !strings.Contains(e, "rebuild-derived") {
		t.Fatalf("embed %d %q", code, e)
	}
}

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
