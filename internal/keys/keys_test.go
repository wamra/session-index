package keys

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// cheap keeps Argon2id fast in tests.
var cheap = Params{Time: 1, MemoryKiB: 64, Threads: 1}

func TestCreateUnwrapRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	id, key, err := Create(p, []byte("correct horse"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != Size || !idRe.MatchString(id) {
		t.Fatalf("id %q key len %d", id, len(key))
	}
	b, _ := os.ReadFile(p)
	if bytes.Contains(b, []byte(strings.ToLower(hexOf(key)))) {
		t.Fatal("key file holds the raw key")
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	gotID, got, err := Unwrap(p, []byte("correct horse"))
	if err != nil || gotID != id || !bytes.Equal(got, key) {
		t.Fatalf("unwrap: %v id %q", err, gotID)
	}
	if fid, err := ID(p); err != nil || fid != id {
		t.Fatalf("ID: %q %v", fid, err)
	}
	if _, _, err := Unwrap(p, []byte("wrong horse!")); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	// a second Create must not replace the file
	if _, _, err := Create(p, []byte("another one"), cheap); err == nil {
		t.Fatal("Create replaced an existing key file")
	}
	// two keys differ
	_, key2, err := Create(filepath.Join(t.TempDir(), FileName), []byte("correct horse"), cheap)
	if err != nil || bytes.Equal(key, key2) {
		t.Fatalf("second key: %v equal=%v", err, bytes.Equal(key, key2))
	}
}

func hexOf(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, d[c>>4], d[c&15])
	}
	return string(out)
}

func TestShortPassphraseRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if _, _, err := Create(p, []byte("short"), cheap); err == nil {
		t.Fatal("expected refusal")
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("key file written for a refused passphrase")
	}
}

func TestEditedKeyFileFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if _, _, err := Create(p, []byte("correct horse"), cheap); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(p)
	edit := func(f func(k *keyFile)) {
		var k keyFile
		if err := json.Unmarshal(orig, &k); err != nil {
			t.Fatal(err)
		}
		f(&k)
		b, _ := json.Marshal(k)
		os.WriteFile(p, b, 0o600)
	}
	edit(func(k *keyFile) { k.ID = "0123456789abcdef" })
	if _, _, err := Unwrap(p, []byte("correct horse")); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("edited id: %v", err)
	}
	edit(func(k *keyFile) { k.ID = "../../../etc/pw" })
	if _, err := ID(p); err == nil {
		t.Error("path-like id accepted")
	}
	edit(func(k *keyFile) { k.MemoryKiB = 1 << 30 })
	if _, _, err := Unwrap(p, []byte("correct horse")); err == nil || errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("huge memory setting must be refused before deriving: %v", err)
	}
	edit(func(k *keyFile) { k.Version = 2 })
	if _, err := ID(p); err == nil {
		t.Error("unknown version accepted")
	}
	if _, err := ID(filepath.Join(t.TempDir(), FileName)); !errors.Is(err, ErrNoKeyFile) {
		t.Errorf("missing file: %v", err)
	}
}

func TestCacheAndForArchive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	t.Setenv("SESSION_INDEX_KEY_DIR", dir)
	db := filepath.Join(t.TempDir(), "archive.sqlite")
	if _, _, err := ForArchive(db); !errors.Is(err, ErrNoKeyFile) {
		t.Fatalf("no key file: %v", err)
	}
	id, key, err := Create(Path(db), []byte("correct horse"), cheap)
	if err != nil {
		t.Fatal(err)
	}
	if _, gotID, err := ForArchive(db); !errors.Is(err, ErrLocked) || gotID != id {
		t.Fatalf("locked: %v id %q", err, gotID)
	}
	if err := Store(id, key); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		di, _ := os.Stat(dir)
		fi, _ := os.Stat(filepath.Join(dir, id+".key"))
		if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
			t.Errorf("modes dir %o file %o", di.Mode().Perm(), fi.Mode().Perm())
		}
	}
	got, gotID, err := ForArchive(db)
	if err != nil || gotID != id || !bytes.Equal(got, key) {
		t.Fatalf("unlocked: %v", err)
	}
	if ok, err := Forget(id); !ok || err != nil {
		t.Fatalf("forget: %v %v", ok, err)
	}
	if ok, err := Forget(id); ok || err != nil {
		t.Fatalf("second forget: %v %v", ok, err)
	}
	if _, err := Load(id); !errors.Is(err, ErrLocked) {
		t.Fatalf("after forget: %v", err)
	}
	if err := Store("../x", key); err == nil {
		t.Fatal("bad id accepted")
	}
}
