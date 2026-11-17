package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithLegacyFallsBackToOldEnvName(t *testing.T) {
	env := map[string]string{"SESSION_ARCHIVE_DB": "/old", "SESSION_ARCHIVE_EMBED_MODEL": "old-model", "SESSION_INDEX_EMBED_MODEL": "new-model"}
	g := WithLegacy(func(k string) string { return env[k] })
	if got := g("SESSION_INDEX_DB"); got != "/old" {
		t.Fatalf("legacy fallback: got %q", got)
	}
	if got := g("SESSION_INDEX_EMBED_MODEL"); got != "new-model" {
		t.Fatalf("new name must win: got %q", got)
	}
	if got := g("HOME"); got != "" {
		t.Fatalf("non-prefixed key must not fall back: got %q", got)
	}
}

func TestPathUsesOldConfigOnlyWhenNewIsMissing(t *testing.T) {
	home := t.TempDir()
	none := func(string) string { return "" }
	old := filepath.Join(home, ".config", "session-archive", "config.json")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Path(home, none); got != old {
		t.Fatalf("old config should be used when new is missing: got %s", got)
	}
	nw := filepath.Join(home, ".config", "session-index", "config.json")
	if err := os.MkdirAll(filepath.Dir(nw), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nw, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Path(home, none); got != nw {
		t.Fatalf("new config must win: got %s", got)
	}
}
