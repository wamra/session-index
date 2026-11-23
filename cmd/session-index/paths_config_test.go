package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/ingest"
	"github.com/wamra/session-index/internal/search"
)

func writeConfig(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SESSION_INDEX_CONFIG", p)
	t.Setenv("SESSION_INDEX_DB", "")
	t.Setenv("SESSION_ARCHIVE_DB", "")
	t.Setenv("SESSION_INDEX_ROOT", "")
	t.Setenv("SESSION_ARCHIVE_ROOT", "")
	t.Setenv("SESSION_INDEX_KEY_DIR", "")
}

func TestConfigFilePathsMoveTheArchiveAndTheRoot(t *testing.T) {
	setHome(t, t.TempDir())
	want := filepath.Join(t.TempDir(), "elsewhere", "a.sqlite")
	writeConfig(t, `{"paths":{"db":`+jsonString(want)+`,"claude_dir":"~/prof"}}`)
	if got := dbPath(); got != want {
		t.Fatalf("db %s", got)
	}
	home, _ := os.UserHomeDir()
	if got := defaultRoot(); got != filepath.Join(home, "prof", "projects") {
		t.Fatalf("root %s", got)
	}
	if got := claudeDir(); got != filepath.Join(home, "prof") {
		t.Fatalf("claude dir %s", got)
	}
	t.Setenv("SESSION_INDEX_DB", "/env/a.sqlite")
	if got := dbPath(); got != "/env/a.sqlite" {
		t.Fatalf("the environment must win: %s", got)
	}
}

func TestBrokenEmbedSectionDoesNotMoveTheArchive(t *testing.T) {
	setHome(t, t.TempDir())
	want := filepath.Join(t.TempDir(), "a.sqlite")
	writeConfig(t, `{"paths":{"db":`+jsonString(want)+`},"embed":{"provider":"nope"}}`)
	if got := dbPath(); got != want {
		t.Fatalf("a bad embed section must not change paths.db, got %s", got)
	}
}

func TestInvalidJSONRefusesInsteadOfCreatingASecondArchive(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	writeConfig(t, `{"paths":`)
	if _, err := openStoreStrict(); err == nil {
		t.Fatal("a strict open must fail on a config that is not JSON")
	}
	var note bytes.Buffer
	if _, err := openStoreWith(dbPath(), true, &note); err == nil || !strings.Contains(err.Error(), "no archive at the default location") {
		t.Fatalf("no default archive: want a refusal, got %v", err)
	}
	if _, err := os.Stat(dbPath()); err == nil {
		t.Fatal("a second archive was created")
	}
}

func TestConfigShowListsPathsSearchAndIngest(t *testing.T) {
	setHome(t, t.TempDir())
	writeConfig(t, `{"search":{"rrf_k":30}}`)
	var out, errw bytes.Buffer
	if code := run([]string{"config"}, &out, &errw); code != 0 {
		t.Fatal(code, errw.String())
	}
	for _, w := range []string{`"paths"`, `"db"`, `"rrf_k": 30`, `"backoff_seconds": 30`, `"default_k": 10`} {
		if !strings.Contains(out.String(), w) {
			t.Fatalf("config lacks %s:\n%s", w, out.String())
		}
	}
}

func jsonString(s string) string {
	b := bytes.NewBuffer(nil)
	b.WriteByte('"')
	for _, r := range s {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func TestSearchAndIngestKnobsComeFromTheFile(t *testing.T) {
	setHome(t, t.TempDir())
	t.Cleanup(func() {
		search.SetTuning(10, 60, 3*time.Second, 24)
		ingest.SetBackoff(30 * time.Second)
	})
	writeConfig(t, `{"search":{"default_k":3,"rrf_k":30,"embed_timeout_ms":500,"max_terms":8},"ingest":{"backoff_seconds":5}}`)
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	k, rrf, wait, terms := search.Tuning()
	if k != 3 || rrf != 30 || wait != 500*time.Millisecond || terms != 8 || ingest.Backoff() != 5*time.Second {
		t.Fatalf("%d %d %v %d %v", k, rrf, wait, terms, ingest.Backoff())
	}
}
