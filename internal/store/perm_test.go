package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestNewArchiveIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "archive")
	db := filepath.Join(dir, "a.sqlite")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// a write makes sure the -wal and -shm files exist while the store is open
	if _, err := s.DB().Exec(`insert into meta(key,value) values('perm-test','1')`); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("archive dir mode %o, want 700", got)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if got := mode(t, db+suffix); got != 0o600 {
			t.Errorf("%s mode %o, want 600", filepath.Base(db+suffix), got)
		}
	}
}

func TestOpenTightensAnExistingArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits")
	}
	db := filepath.Join(t.TempDir(), "a.sqlite")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := mode(t, db); got != 0o600 {
		t.Errorf("mode %o, want 600", got)
	}
}
