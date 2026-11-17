package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/session-index/internal/ingest"
	"github.com/wamra/session-index/internal/store"
)

const sid = "11111111-1111-1111-1111-111111111111"

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func userLine(n int, text string) string {
	return fmt.Sprintf(`{"type":"user","uuid":"u%d","sessionId":"%s","timestamp":"2026-01-01T00:00:%02dZ","cwd":"/work/x","gitBranch":"main","message":{"role":"user","content":%q}}`, n, sid, n%60, text) + "\n"
}

func asstLine(n int, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"a%d","sessionId":"%s","timestamp":"2026-01-01T00:01:%02dZ","costUSD":0.5,"message":{"role":"assistant","model":"m1","usage":{"input_tokens":10,"output_tokens":20,"service_tier":"standard"},"content":[{"type":"text","text":%q},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo zebrafish"}}]}}`, n, sid, n%60, text) + "\n"
}

const longText = "This is a sufficiently long paragraph of synthetic text for chunking."

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fixture writes a main file and a subagent file under root, ingests both,
// and returns their paths.
func fixture(t *testing.T, st *store.Store, root string) (mainPath, subPath string) {
	t.Helper()
	mainPath = filepath.Join(root, "proj", sid+".jsonl")
	subPath = filepath.Join(root, "proj", sid, "subagents", "agent-x.jsonl")

	crlf := strings.TrimSuffix(userLine(3, "crlf terminated "+longText), "\n") + "\r\n"
	mainContent := strings.Join([]string{
		userLine(1, longText),
		"", // blank line
		userLine(2, "héllo wörld ✓ 日本語 "+longText),
		crlf,
		userLine(4, strings.Repeat("a", 8<<20)),
		asstLine(5, longText),
	}, "\n") + "\n"
	writeFile(t, mainPath, mainContent)
	writeFile(t, subPath, userLine(1, "subagent "+longText)+asstLine(2, "subagent reply "+longText))

	ctx := context.Background()
	for _, p := range []string{mainPath, subPath} {
		if _, err := ingest.File(ctx, st, root, p, ""); err != nil {
			t.Fatalf("ingest %s: %v", p, err)
		}
	}
	return mainPath, subPath
}

func TestRestoreRoundTripSHA256(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	mainPath, subPath := fixture(t, st, root)

	out := t.TempDir()
	written, err := Session(context.Background(), st, sid, out, false)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if len(written) != 2 {
		t.Fatalf("written=%v want 2 paths", written)
	}
	for _, src := range []string{mainPath, subPath} {
		rel, err := filepath.Rel(root, src)
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(out, rel)
		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("missing restored file %s: %v", dst, err)
		}
		if got, want := fileSHA(t, dst), fileSHA(t, src); got != want {
			t.Fatalf("%s sha256=%s want %s", rel, got, want)
		}
	}
}

func TestSessionUnknownIDErrors(t *testing.T) {
	st := openStore(t)
	out := t.TempDir()
	if _, err := Session(context.Background(), st, sid, out, false); err == nil {
		t.Fatal("want error for session with no files")
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatalf("wrote %d entries for empty session", len(entries))
	}
}

func TestInPlaceRestoreRewritesOriginals(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	mainPath, subPath := fixture(t, st, root)
	wantMain, wantSub := fileSHA(t, mainPath), fileSHA(t, subPath)
	os.Remove(mainPath)
	os.Remove(subPath)

	if _, err := Session(context.Background(), st, sid, "", true); err != nil {
		t.Fatalf("Session in place: %v", err)
	}
	if got := fileSHA(t, mainPath); got != wantMain {
		t.Fatalf("main sha256=%s want %s", got, wantMain)
	}
	if got := fileSHA(t, subPath); got != wantSub {
		t.Fatalf("subagent sha256=%s want %s", got, wantSub)
	}
}

func TestInPlaceRefusesWhenAnyTargetExists(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	mainPath, subPath := fixture(t, st, root)
	wantSub := fileSHA(t, subPath)
	os.Remove(mainPath) // main target is free; subagent (second in path order) exists

	if _, err := Session(context.Background(), st, sid, "", true); err == nil {
		t.Fatal("want error when a target file already exists")
	}
	if _, err := os.Stat(mainPath); !os.IsNotExist(err) {
		t.Fatalf("main file written despite refusal (stat err=%v)", err)
	}
	if got := fileSHA(t, subPath); got != wantSub {
		t.Fatalf("existing subagent was modified: sha256=%s want %s", got, wantSub)
	}
}

func TestToDirRefusesExistingTarget(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	fixture(t, st, root)

	out := t.TempDir()
	existing := filepath.Join(out, "proj", sid+".jsonl")
	writeFile(t, existing, "keep me\n")

	if _, err := Session(context.Background(), st, sid, out, false); err == nil {
		t.Fatal("want error when a restore target already exists")
	}
	b, err := os.ReadFile(existing)
	if err != nil || string(b) != "keep me\n" {
		t.Fatalf("existing target modified: %q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(out, "proj", sid, "subagents", "agent-x.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("subagent written despite refusal (stat err=%v)", err)
	}
}

func TestZeroLiveRecordsErrors(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	const sid2 = "22222222-2222-2222-2222-222222222222"
	p := filepath.Join(root, "proj", sid2+".jsonl")
	writeFile(t, p, "")
	if _, err := ingest.File(context.Background(), st, root, p, ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	out := t.TempDir()
	_, err := Session(context.Background(), st, sid2, out, false)
	if err == nil || !strings.Contains(err.Error(), "no live records") {
		t.Fatalf("err=%v want no-live-records error", err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatalf("wrote %d entries for file with no live records", len(entries))
	}
}

func TestPrefixHashMismatchErrors(t *testing.T) {
	st := openStore(t)
	root := t.TempDir()
	mainPath, _ := fixture(t, st, root)
	if _, err := st.DB().Exec(`UPDATE files SET sha256_prefix=? WHERE path=?`, strings.Repeat("0", 64), mainPath); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	_, err := Session(context.Background(), st, sid, out, false)
	if err == nil || !strings.Contains(err.Error(), mainPath) || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err=%v want prefix mismatch naming %s", err, mainPath)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatalf("wrote %d entries despite prefix mismatch", len(entries))
	}
}

func smallSession(t *testing.T, st *store.Store) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "proj", sid+".jsonl")
	root := filepath.Dir(filepath.Dir(p))
	writeFile(t, p, userLine(1, longText)+asstLine(2, longText)+userLine(3, longText)+asstLine(4, longText))
	if _, err := ingest.File(context.Background(), st, root, p, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsLineNoGap(t *testing.T) {
	st := openStore(t)
	smallSession(t, st)
	if _, err := st.DB().Exec(`DELETE FROM chunks WHERE record_id IN (SELECT id FROM records WHERE line_no=2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM messages WHERE record_id IN (SELECT id FROM records WHERE line_no=2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM records WHERE line_no=2`); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	_, err := Session(context.Background(), st, sid, out, false)
	if err == nil || !strings.Contains(err.Error(), "line_no gap") || !strings.Contains(err.Error(), sid+".jsonl") {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Fatalf("wrote files despite error: %v", ents)
	}
}

func TestRestoreRejectsOffsetMismatch(t *testing.T) {
	st := openStore(t)
	smallSession(t, st)
	// Missing trailing record: line_no stays contiguous, byte total falls short.
	for _, q := range []string{
		`DELETE FROM chunks WHERE record_id IN (SELECT id FROM records WHERE line_no=4)`,
		`DELETE FROM messages WHERE record_id IN (SELECT id FROM records WHERE line_no=4)`,
		`DELETE FROM records WHERE line_no=4`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	_, err := Session(context.Background(), st, sid, out, false)
	if err == nil || !strings.Contains(err.Error(), "archived offset") || !strings.Contains(err.Error(), sid+".jsonl") {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(out); len(ents) != 0 {
		t.Fatalf("wrote files despite error: %v", ents)
	}
}
