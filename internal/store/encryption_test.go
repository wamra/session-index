package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/keys"
)

const needle = "needle-7f3a-plaintext-marker"

var cheapKDF = keys.Params{Time: 1, MemoryKiB: 64, Threads: 1}

// plainArchive creates a plaintext archive holding needle rows and returns
// its path. The machine key directory points at a temp dir.
func plainArchive(t *testing.T) string {
	t.Helper()
	t.Setenv("SESSION_INDEX_KEY_DIR", filepath.Join(t.TempDir(), "keys"))
	path := filepath.Join(t.TempDir(), "archive.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CREATE TABLE note(id INTEGER PRIMARY KEY, body TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if _, err := st.DB().Exec(`INSERT INTO note(body) VALUES (?)`, needle+strings.Repeat(" filler", 40)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// newKey writes a key file next to path and unlocks it on this "machine".
func newKey(t *testing.T, path string) (id string, key []byte) {
	t.Helper()
	id, key, err := keys.Create(keys.Path(path), []byte("correct horse"), cheapKDF)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Store(id, key); err != nil {
		t.Fatal(err)
	}
	return id, key
}

func noteCount(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM note WHERE body LIKE ?`, needle+"%").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// holdsNeedle reports whether any file whose name starts with the archive's
// name contains the plaintext needle.
func holdsNeedle(t *testing.T, path string, skip ...string) []string {
	t.Helper()
	var hits []string
	ents, _ := os.ReadDir(filepath.Dir(path))
next:
	for _, e := range ents {
		p := filepath.Join(filepath.Dir(path), e.Name())
		if e.IsDir() || !strings.HasPrefix(e.Name(), filepath.Base(path)) {
			continue
		}
		for _, s := range skip {
			if p == s {
				continue next
			}
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(needle)) {
			hits = append(hits, e.Name())
		}
	}
	return hits
}

var fast = ConvertOpts{Wait: 2 * time.Second, Settle: -1}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	path := plainArchive(t)
	_, key := newKey(t, path)
	ctx := context.Background()

	backup, err := Encrypt(ctx, path, key, fast)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := Sniff(backup); s != FilePlain {
		t.Fatalf("the plaintext file must be kept as it was, got state %v", s)
	}
	if s, _ := Sniff(path); s != FileOpaque {
		t.Fatalf("archive state %v after encrypt", s)
	}
	if exists(MarkerPath(path)) || exists(tempPath(path)) {
		t.Fatal("marker or temp file left behind")
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("encrypted archive mode %o", fi.Mode().Perm())
	}

	// every open site works unchanged, and writes stay encrypted
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := noteCount(t, st); n != 300 {
		t.Fatalf("%d rows after encrypt", n)
	}
	var mode string
	st.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if !strings.EqualFold(mode, JournalMode) {
		t.Errorf("journal mode %q, want %q", mode, JournalMode)
	}
	if _, err := st.DB().Exec(`INSERT INTO note(body) VALUES (?)`, needle+" written after encrypt"); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := noteCount(t, ro); n != 301 {
		t.Fatalf("read-only sees %d rows", n)
	}
	ro.Close()
	if hits := holdsNeedle(t, path, backup); len(hits) != 0 {
		t.Fatalf("plaintext found in %v", hits)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// and back
	encBackup, err := Decrypt(ctx, path, key, fast)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := Sniff(path); s != FilePlain {
		t.Fatalf("archive state %v after decrypt", s)
	}
	if s, _ := Sniff(encBackup); s != FileOpaque {
		t.Fatalf("encrypted file must be kept, got state %v", s)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := noteCount(t, st); n != 301 {
		t.Fatalf("%d rows after decrypt", n)
	}
}

func TestEncryptRefusesWrongState(t *testing.T) {
	path := plainArchive(t)
	_, key := newKey(t, path)
	ctx := context.Background()
	if _, err := Decrypt(ctx, path, key, fast); err == nil {
		t.Fatal("decrypt of a plaintext archive must fail")
	}
	if _, err := Encrypt(ctx, path, key, fast); err != nil {
		t.Fatal(err)
	}
	if _, err := Encrypt(ctx, path, key, fast); err == nil {
		t.Fatal("encrypt of an encrypted archive must fail")
	}
	if _, err := Encrypt(ctx, filepath.Join(t.TempDir(), "none.sqlite"), key, fast); err == nil {
		t.Fatal("encrypt of a missing archive must fail")
	}
	other := bytes.Repeat([]byte{7}, keys.Size)
	_, err := Decrypt(ctx, path, other, fast)
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("decrypt with another key: %v", err)
	}
	if s, _ := Sniff(path); s != FileOpaque || exists(MarkerPath(path)) {
		t.Fatal("a failed run must leave the archive as it was")
	}
}

func TestLockedWrongKeyAndMissingKeyFile(t *testing.T) {
	path := plainArchive(t)
	id, key := newKey(t, path)
	if _, err := Encrypt(context.Background(), path, key, fast); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Forget(id); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, keys.ErrLocked) {
		t.Fatalf("Open while locked: %v", err)
	}
	if _, err := OpenReadOnly(path); !errors.Is(err, keys.ErrLocked) {
		t.Fatalf("OpenReadOnly while locked: %v", err)
	}
	wrong := bytes.Repeat([]byte{9}, keys.Size)
	if err := keys.Store(id, wrong); err != nil {
		t.Fatal(err)
	}
	for name, open := range map[string]func(string) (*Store, error){"Open": Open, "OpenReadOnly": OpenReadOnly} {
		_, err := open(path)
		if !errors.Is(err, ErrWrongKey) {
			t.Fatalf("%s with a wrong key: %v", name, err)
		}
		if strings.Contains(err.Error(), hex.EncodeToString(wrong)) {
			t.Fatalf("%s error shows the key: %v", name, err)
		}
	}
	if s, _ := Sniff(path); s != FileOpaque {
		t.Fatal("a wrong key must not change the archive")
	}
	if err := os.Remove(keys.Path(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), keys.FileName) {
		t.Fatalf("missing key file: %v", err)
	}
}

func TestNewArchiveIsEncryptedWhenKeyFileExists(t *testing.T) {
	t.Setenv("SESSION_INDEX_KEY_DIR", filepath.Join(t.TempDir(), "keys"))
	path := filepath.Join(t.TempDir(), "archive.sqlite")
	newKey(t, path)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CREATE TABLE note(body TEXT); INSERT INTO note VALUES ('` + needle + `')`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if s, _ := Sniff(path); s != FileOpaque {
		t.Fatalf("state %v", s)
	}
	if hits := holdsNeedle(t, path); len(hits) != 0 {
		t.Fatalf("plaintext found in %v", hits)
	}
	e := Encryption(path)
	if e.State != FileOpaque || !e.KeyFile || !e.Unlocked || e.KeyID == "" || e.Err != nil {
		t.Fatalf("%+v", e)
	}
}

func TestMarkerBlocksOpensAndRecover(t *testing.T) {
	path := plainArchive(t)
	if err := os.WriteFile(MarkerPath(path), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrMigrating) {
		t.Fatalf("Open: %v", err)
	}
	if _, err := OpenReadOnly(path); !errors.Is(err, ErrMigrating) {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if ok, err := Recover(path); !ok || err != nil {
		t.Fatalf("Recover: %v %v", ok, err)
	}
	if ok, err := Recover(path); ok || err != nil {
		t.Fatalf("second Recover: %v %v", ok, err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// interrupted between the two renames: the old file is moved back
	backup := path + ".plaintext-20260101-000000"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(tempPath(path), []byte("half a copy"), 0o600)
	os.WriteFile(MarkerPath(path), []byte(`{"pid":1,"backup":"`+filepath.Base(backup)+`","temp":"x"}`), 0o600)
	if ok, err := Recover(path); !ok || err != nil {
		t.Fatalf("Recover mid-swap: %v %v", ok, err)
	}
	if exists(backup) || exists(tempPath(path)) || exists(MarkerPath(path)) {
		t.Fatal("leftovers after recovery")
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := noteCount(t, st); n != 300 {
		t.Fatalf("%d rows after recovery", n)
	}
	st.Close()

	// nothing to move back: say so and keep the marker
	os.Rename(path, filepath.Join(filepath.Dir(path), "elsewhere"))
	os.WriteFile(MarkerPath(path), []byte(`{"pid":1,"backup":"../../etc/passwd"}`), 0o600)
	if _, err := Recover(path); err == nil || !exists(MarkerPath(path)) {
		t.Fatalf("Recover without a usable copy: %v", err)
	}
}

func TestEncryptWaitsForOtherConnections(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no WAL on Windows: an idle connection holds no lock there")
	}
	path := plainArchive(t)
	_, key := newKey(t, path)
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if n := noteCount(t, other); n != 300 {
		t.Fatal(n)
	}
	_, err = Encrypt(context.Background(), path, key, ConvertOpts{Wait: 600 * time.Millisecond, Settle: -1})
	if err == nil || !strings.Contains(err.Error(), "another process") {
		t.Fatalf("encrypt next to an open connection: %v", err)
	}
	if s, _ := Sniff(path); s != FilePlain || exists(MarkerPath(path)) || exists(tempPath(path)) {
		t.Fatal("a refused run must leave the archive as it was")
	}
	if _, err := other.DB().Exec(`INSERT INTO note(body) VALUES ('still writable')`); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptUndoesSwapWhenAStaleWriterAppears(t *testing.T) {
	path := plainArchive(t)
	_, key := newKey(t, path)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // stands in for a process that still had the old file open
		defer close(done)
		for {
			if m, _ := filepath.Glob(path + ".plaintext-*"); len(m) > 0 {
				os.WriteFile(path+"-wal", []byte("stale"), 0o600)
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	_, err := Encrypt(context.Background(), path, key, ConvertOpts{Wait: 2 * time.Second, Settle: 500 * time.Millisecond})
	close(stop)
	<-done
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("expected the swap to be undone: %v", err)
	}
	if m, _ := filepath.Glob(path + ".plaintext-*"); len(m) != 0 || exists(tempPath(path)) || exists(MarkerPath(path)) {
		t.Fatalf("leftovers: %v", m)
	}
	os.Remove(path + "-wal")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := noteCount(t, st); n != 300 {
		t.Fatalf("%d rows", n)
	}
}

func TestCopyErrorDoesNotShowKey(t *testing.T) {
	path := plainArchive(t)
	_, key := newKey(t, path)
	// a non-empty directory where the copy would go makes VACUUM INTO fail
	if err := os.MkdirAll(filepath.Join(tempPath(path), "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Encrypt(context.Background(), path, key, fast)
	if err == nil {
		t.Fatal("expected a copy error")
	}
	if strings.Contains(err.Error(), hex.EncodeToString(key)) {
		t.Fatalf("error shows the key: %v", err)
	}
	if s, _ := Sniff(path); s != FilePlain || exists(MarkerPath(path)) {
		t.Fatal("a failed run must leave the archive as it was")
	}
}
