package store

import (
	"os"
	"path/filepath"
	"runtime"
)

// Archive directory and file modes. The archive holds every transcript line, so it is private
// to the user. Windows has no Unix permission bits; there the user profile's ACLs apply.
const (
	dirMode  = 0o700
	fileMode = 0o600
)

// prepareFiles creates the archive's parent directory (0700 when it is new; an existing
// directory keeps its mode, since it may be shared on purpose) and an empty database file
// (0600) when none exists, so SQLite's -wal and -shm files, which copy the database file's
// mode, are private from their first write.
func prepareFiles(dbPath string) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), dirMode); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return err
	}
	return f.Close()
}

// tightenFiles makes an existing database and its -wal and -shm files owner-only. Errors are
// ignored: a file that is missing or owned by someone else is not a reason to fail an open.
func tightenFiles(dbPath string) {
	if runtime.GOOS == "windows" {
		return
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := dbPath + suffix
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&^fileMode != 0 {
			_ = os.Chmod(p, fileMode)
		}
	}
}
