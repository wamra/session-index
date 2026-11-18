package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"
	"github.com/wamra/session-index/internal/keys"
)

// An encrypted archive is the same SQLite database stored through the
// driver's Adiantum VFS: every page, the journal and the WAL are encrypted
// with the archive key (internal/keys). Which way a file is opened is decided
// from the file itself, so every open site, peers included, needs no setting.

// FileState is what Sniff found at an archive path.
type FileState int

const (
	// FileAbsent: no file, or an empty one.
	FileAbsent FileState = iota
	// FilePlain: a plaintext SQLite database.
	FilePlain
	// FileOpaque: anything else; an encrypted archive looks like this.
	FileOpaque
)

const sqliteMagic = "SQLite format 3\x00"

// Sniff reads the first bytes of path to tell a plaintext database from an
// encrypted one.
func Sniff(path string) (FileState, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return FileAbsent, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	b := make([]byte, len(sqliteMagic))
	n, err := io.ReadFull(f, b)
	switch {
	case n == 0 && err == io.EOF:
		return FileAbsent, nil
	case err != nil && err != io.ErrUnexpectedEOF:
		return 0, err
	case n == len(b) && string(b) == sqliteMagic:
		return FilePlain, nil
	}
	return FileOpaque, nil
}

var (
	// ErrMigrating is returned by the open functions while an encrypt or
	// decrypt run owns the archive (its marker file exists).
	ErrMigrating = errors.New("archive is being encrypted or decrypted")
	// ErrWrongKey is returned when the key cached on this machine does not
	// open the archive.
	ErrWrongKey = errors.New("the key unlocked on this machine does not open this archive")
)

// MarkerPath is the marker file an encrypt or decrypt run keeps next to the
// archive at path.
func MarkerPath(path string) string { return path + ".migrating" }

func checkMarker(path string) error {
	m := MarkerPath(path)
	if _, err := os.Stat(m); err == nil {
		return fmt.Errorf("%w: %s exists (if no such run is active it was interrupted: repair it with `session-index encrypt --recover`)", ErrMigrating, m)
	}
	return nil
}

// keyFor returns the hex key the archive at path is opened with, or "" for a
// plaintext archive. A new archive is encrypted when a key file is already
// there.
func keyFor(path string) (string, error) {
	state, err := Sniff(path)
	if err != nil {
		return "", err
	}
	if state == FilePlain {
		return "", nil
	}
	key, _, err := keys.ForArchive(path)
	switch {
	case errors.Is(err, keys.ErrNoKeyFile) && state == FileAbsent:
		return "", nil
	case errors.Is(err, keys.ErrNoKeyFile):
		return "", fmt.Errorf("%s is not a plaintext SQLite database and its key file %s is missing", path, keys.Path(path))
	case err != nil:
		return "", err
	}
	return hex.EncodeToString(key), nil
}

// withKey adds the encryption settings for hexKey to the query of a DSN.
func withKey(q url.Values, hexKey string) {
	if hexKey == "" {
		// Named explicitly: a file attached from an encrypted connection
		// (VACUUM INTO) would otherwise inherit that connection's VFS.
		q.Set("vfs", "os")
		return
	}
	// Temporary files stay on disk: the VFS encrypts each with a random key,
	// and a VACUUM of a large archive would not fit in memory.
	q.Set("vfs", "adiantum")
	q.Set("hexkey", hexKey)
}

// redact keeps the key out of an error text (SQLite names the file URI in
// some messages).
func redact(err error, hexKey string) error {
	if err == nil || hexKey == "" || !strings.Contains(err.Error(), hexKey) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), hexKey, "<key>"))
}

// probe forces a real read through an encrypted connection: with the wrong
// key SQLite reports "file is not a database" on the first statement only.
func probe(db *sql.DB, path, hexKey string) error {
	var n int
	err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n)
	if err == nil || hexKey == "" {
		return err
	}
	if errors.Is(err, sqlite3.NOTADB) {
		return fmt.Errorf("%w: %s", ErrWrongKey, path)
	}
	return redact(err, hexKey)
}

// EncryptionInfo describes how the archive at a path is protected.
type EncryptionInfo struct {
	State    FileState
	KeyFile  bool   // a key file sits next to the archive
	KeyID    string // its id
	Unlocked bool   // that key is cached on this machine
	Err      error  // an unreadable archive or key file
}

// Encryption reports the protection of the archive at path without opening it.
func Encryption(path string) EncryptionInfo {
	var e EncryptionInfo
	e.State, e.Err = Sniff(path)
	_, id, err := keys.ForArchive(path)
	switch {
	case errors.Is(err, keys.ErrNoKeyFile):
	case err == nil:
		e.KeyFile, e.KeyID, e.Unlocked = true, id, true
	case errors.Is(err, keys.ErrLocked):
		e.KeyFile, e.KeyID = true, id
	default:
		e.KeyFile = true
		if e.Err == nil {
			e.Err = err
		}
	}
	return e
}
