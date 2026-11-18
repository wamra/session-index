package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/session-index/internal/keys"
	"github.com/wamra/session-index/internal/store"
)

const testPass = "correct horse battery"

// encEnv is seedRoot with a private key directory and cheap key stretching.
func encEnv(t *testing.T) (root, db string) {
	t.Helper()
	root, db = seedRoot(t)
	t.Setenv("SESSION_INDEX_KEY_DIR", filepath.Join(t.TempDir(), "keys"))
	t.Setenv("SESSION_INDEX_PEERS", filepath.Join(t.TempDir(), "none.sqlite"))
	old := kdfParams
	kdfParams = keys.Params{Time: 1, MemoryKiB: 64, Threads: 1}
	t.Cleanup(func() { kdfParams = old; stdin = os.Stdin })
	return root, db
}

// runWith runs the CLI with text on standard input.
func runWith(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	stdin = strings.NewReader(input)
	return runCLI(t, args...)
}

func fileState(t *testing.T, p string) store.FileState {
	t.Helper()
	s, err := store.Sniff(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCLIEncryptLockUnlockDecrypt(t *testing.T) {
	root, db := encEnv(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}

	// a short passphrase is refused before anything is written
	if code, _, e := runWith(t, "short\n", "encrypt"); code != 1 || !strings.Contains(e, "at least") {
		t.Fatalf("short passphrase: %d %s", code, e)
	}
	if _, err := os.Stat(keys.Path(db)); err == nil || fileState(t, db) != store.FilePlain {
		t.Fatal("a refused encrypt must change nothing")
	}

	code, out, e := runWith(t, testPass+"\n", "encrypt")
	if code != 0 || !strings.Contains(out, "encrypted: "+db) || !strings.Contains(out, "plaintext copy kept: ") {
		t.Fatalf("encrypt: %d\n%s\n%s", code, out, e)
	}
	if strings.Contains(out+e, testPass) {
		t.Fatal("the passphrase was printed")
	}
	if fileState(t, db) != store.FileOpaque {
		t.Fatal("archive is not encrypted")
	}
	left, _ := filepath.Glob(db + ".plaintext-*")
	if len(left) != 1 || fileState(t, left[0]) != store.FilePlain {
		t.Fatalf("plaintext copy: %v", left)
	}
	if code, out, _ := runWith(t, "", "encrypt"); code != 0 || !strings.Contains(out, "already encrypted") {
		t.Fatalf("second encrypt: %d %s", code, out)
	}

	// every command goes on working, and what it writes is encrypted
	if code, out, e := runCLI(t, "search", "zebrafish", "--mode", "fts", "--json"); code != 0 || !strings.Contains(out, tsid) {
		t.Fatalf("search: %d %s %s", code, out, e)
	}
	const sid2 = "33333333-3333-3333-3333-333333333333"
	p2 := filepath.Join(root, "proj", sid2+".jsonl")
	os.WriteFile(p2, []byte(fmt.Sprintf(`{"type":"user","uuid":"u9","sessionId":"%s","timestamp":"2026-01-02T00:00:01Z","cwd":"/w","message":{"role":"user","content":"Tell me about the quokka habitat survey results"}}`, sid2)+"\n"), 0o644)
	hook := fmt.Sprintf(`{"session_id":"%s","transcript_path":%q,"hook_event_name":"Stop"}`, sid2, p2)
	if code, out, e := runWith(t, hook, "ingest", "--stdin-hook"); code != 0 || out != "" {
		t.Fatalf("hook: %d %q %s", code, out, e)
	}
	if code, out, _ := runCLI(t, "search", "quokka", "--mode", "fts", "--json"); code != 0 || !strings.Contains(out, sid2) {
		t.Fatalf("search after hook: %d %s", code, out)
	}
	if code, _, e := runCLI(t, "vacuum"); code != 0 {
		t.Fatalf("vacuum: %d %s", code, e)
	}
	for _, f := range []string{db, db + "-wal", db + "-journal"} {
		if b, err := os.ReadFile(f); err == nil && (bytes.Contains(b, []byte("zebrafish")) || bytes.Contains(b, []byte("quokka"))) {
			t.Fatalf("plaintext in %s", f)
		}
	}
	_, out, _ = runCLI(t, "doctor")
	if !strings.Contains(out, "encryption: on (key ") || !strings.Contains(out, "unlocked on this machine") ||
		!strings.Contains(out, "WARNING: plaintext copy of the archive still present: "+left[0]) {
		t.Fatalf("doctor: %s", out)
	}

	// locked: nothing opens, a Stop hook says so and still exits 0
	if code, out, _ := runCLI(t, "lock"); code != 0 || !strings.Contains(out, "locked (key ") {
		t.Fatalf("lock: %d %s", code, out)
	}
	if code, _, e := runCLI(t, "search", "zebrafish", "--mode", "fts"); code != 1 || !strings.Contains(e, "session-index unlock") {
		t.Fatalf("search while locked: %d %s", code, e)
	}
	code, out, _ = runWith(t, hook, "ingest", "--stdin-hook")
	var msg struct {
		SystemMessage string `json:"systemMessage"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &msg) != nil || !strings.Contains(msg.SystemMessage, "session-index unlock") {
		t.Fatalf("locked Stop hook: %d %q", code, out)
	}
	end := strings.Replace(hook, `"Stop"`, `"SessionEnd"`, 1)
	if code, out, _ := runWith(t, end, "ingest", "--stdin-hook"); code != 0 || out != "" {
		t.Fatalf("locked SessionEnd hook: %d %q", code, out)
	}
	if _, out, _ := runCLI(t, "doctor"); !strings.Contains(out, "LOCKED on this machine") {
		t.Fatalf("doctor while locked: %s", out)
	}

	if code, out, _ := runWith(t, "not the passphrase\n", "unlock"); code != 1 || !strings.Contains(out, "wrong passphrase") {
		t.Fatalf("unlock with a wrong passphrase: %d %s", code, out)
	}
	if code, out, e := runWith(t, testPass+"\n", "unlock"); code != 0 || !strings.Contains(out, "unlocked (key ") {
		t.Fatalf("unlock: %d %s %s", code, out, e)
	}
	if code, out, _ := runWith(t, "", "unlock"); code != 0 || !strings.Contains(out, "already unlocked") {
		t.Fatalf("second unlock: %d %s", code, out)
	}
	if code, out, _ := runCLI(t, "search", "quokka", "--mode", "fts", "--json"); code != 0 || !strings.Contains(out, sid2) {
		t.Fatalf("search after unlock: %d %s", code, out)
	}

	// back to plaintext, and encrypted again with the same key file
	code, out, e = runWith(t, "", "decrypt")
	if code != 0 || !strings.Contains(out, "encrypted copy kept: ") || fileState(t, db) != store.FilePlain {
		t.Fatalf("decrypt: %d\n%s\n%s", code, out, e)
	}
	if code, out, _ := runCLI(t, "search", "quokka", "--mode", "fts", "--json"); code != 0 || !strings.Contains(out, sid2) {
		t.Fatalf("search after decrypt: %d %s", code, out)
	}
	if code, _, e := runWith(t, "not the passphrase\n", "encrypt"); code != 1 || !strings.Contains(e, "wrong passphrase") || fileState(t, db) != store.FilePlain {
		t.Fatalf("encrypt with a wrong passphrase: %d %s", code, e)
	}
	if code, out, e := runWith(t, testPass+"\n", "encrypt"); code != 0 || fileState(t, db) != store.FileOpaque {
		t.Fatalf("encrypt again: %d\n%s\n%s", code, out, e)
	}
	if code, out, _ := runCLI(t, "search", "zebrafish", "--mode", "fts", "--json"); code != 0 || !strings.Contains(out, tsid) {
		t.Fatalf("search after the second encrypt: %d %s", code, out)
	}
}

func TestCLIEncryptBeforeFirstArchive(t *testing.T) {
	_, db := encEnv(t)
	code, out, e := runWith(t, testPass+"\n", "encrypt")
	if code != 0 || !strings.Contains(out, "will be created encrypted") {
		t.Fatalf("encrypt: %d %s %s", code, out, e)
	}
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	if fileState(t, db) != store.FileOpaque {
		t.Fatal("the new archive is not encrypted")
	}
	if code, out, _ := runCLI(t, "search", "zebrafish", "--mode", "fts", "--json"); code != 0 || !strings.Contains(out, tsid) {
		t.Fatalf("search: %d %s", code, out)
	}
}

func TestCLIUnlockCoversPeersAndSearchReadsThem(t *testing.T) {
	_, db := encEnv(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	if code, _, e := runWith(t, testPass+"\n", "encrypt"); code != 0 {
		t.Fatalf("encrypt %d %s", code, e)
	}
	// a second machine's archive with its own key and the same passphrase
	peerRoot := t.TempDir()
	const psid = "44444444-4444-4444-4444-444444444444"
	pp := filepath.Join(peerRoot, "proj", psid+".jsonl")
	os.MkdirAll(filepath.Dir(pp), 0o755)
	os.WriteFile(pp, []byte(fmt.Sprintf(`{"type":"user","uuid":"p1","sessionId":"%s","timestamp":"2026-01-03T00:00:01Z","cwd":"/w","message":{"role":"user","content":"Explain the narwhal tusk measurement spreadsheet"}}`, psid)+"\n"), 0o644)
	peer := filepath.Join(t.TempDir(), "laptop", "cache", "session-archive", "archive.sqlite")
	t.Setenv("SESSION_INDEX_DB", peer)
	t.Setenv("SESSION_INDEX_ROOT", peerRoot)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("peer sweep %d %s", code, e)
	}
	if code, _, e := runWith(t, testPass+"\n", "encrypt"); code != 0 {
		t.Fatalf("peer encrypt %d %s", code, e)
	}
	t.Setenv("SESSION_INDEX_DB", db)
	t.Setenv("SESSION_INDEX_PEERS", peer)

	if code, out, _ := runCLI(t, "lock"); code != 0 || strings.Count(out, "locked (key ") != 2 {
		t.Fatalf("lock: %d %s", code, out)
	}
	code, out, e := runWith(t, testPass+"\n", "unlock")
	if code != 0 || strings.Count(out, "unlocked (key ") != 2 || !strings.Contains(out, "laptop: unlocked") {
		t.Fatalf("unlock: %d %s %s", code, out, e)
	}
	code, out, e = runCLI(t, "search", "narwhal", "--mode", "fts", "--json")
	if code != 0 || !strings.Contains(out, psid) {
		t.Fatalf("search across an encrypted peer: %d %s %s", code, out, e)
	}
}
