package store

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestDSNKeepsPathIntact(t *testing.T) {
	q := url.Values{}
	q.Set("mode", "ro")
	for path, want := range map[string]string{
		`/tmp/x/a.sqlite`:       `file:/tmp/x/a.sqlite?mode=ro`,
		`a.sqlite`:              `file:a.sqlite?mode=ro`,
		`C:\Users\x\a.sqlite`:   `file:C:%5CUsers%5Cx%5Ca.sqlite?mode=ro`,
		`\\srv\share\a.sqlite`:  `file:%5C%5Csrv%5Cshare%5Ca.sqlite?mode=ro`,
		`/tmp/50% #1?/a.sqlite`: `file:/tmp/50%25%20%231%3F/a.sqlite?mode=ro`,
	} {
		if got := dsn(path, q); got != want {
			t.Errorf("dsn(%q) = %q, want %q", path, got, want)
		}
	}
}

// A directory name with URI metacharacters must not change which file is
// opened ('#' starts a fragment and '%' an escape in a "file:" URI).
func TestOpenPathWithURICharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "50%41 #1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO meta(key,value) VALUES('k','v')`); err != nil {
		t.Fatal(err)
	}
	s.Checkpoint()
	s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("archive was not created at the requested path: %v", err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var v string
	if err := ro.DB().QueryRow(`SELECT value FROM meta WHERE key='k'`).Scan(&v); err != nil || v != "v" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestJournalModeByOS(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "WAL", "linux": "WAL", "windows": "WAL"} {
		if got := journalMode(goos); got != want {
			t.Errorf("journalMode(%q) = %q, want %q", goos, got, want)
		}
	}
}
