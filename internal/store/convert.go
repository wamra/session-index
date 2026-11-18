package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/wamra/session-index/internal/filelock"
	"github.com/wamra/session-index/internal/keys"
)

// ConvertOpts tunes Encrypt and Decrypt. The zero value is ready to use.
type ConvertOpts struct {
	// Wait is how long to wait for other processes to leave the archive
	// (default 30 s).
	Wait time.Duration
	// Settle is the pause after the swap during which a process that was
	// still holding the old file would show itself (default 1 s; negative:
	// none).
	Settle time.Duration
	// Progress, when set, receives one line per step.
	Progress func(string)
}

func (o ConvertOpts) say(format string, a ...any) {
	if o.Progress != nil {
		o.Progress(fmt.Sprintf(format, a...))
	}
}

// Encrypt replaces the plaintext archive at path with an encrypted copy and
// returns where the plaintext file was moved to. Nothing is deleted: removing
// the plaintext file is left to the owner.
//
// The copy is made with VACUUM INTO while this process holds the archive
// exclusively, verified (integrity_check, same tables, same row counts), and
// only then swapped in. While it runs a marker file makes every other open of
// the archive fail (ErrMigrating), so a hook cannot write to the old file
// after the copy was taken.
func Encrypt(ctx context.Context, path string, key []byte, o ConvertOpts) (backup string, err error) {
	if len(key) != keys.Size {
		return "", errors.New("invalid key length")
	}
	return convert(ctx, path, "", hex.EncodeToString(key), o)
}

// Decrypt is the reverse of Encrypt: the encrypted archive at path is replaced
// by a plaintext copy, and the encrypted file is kept under the returned name.
func Decrypt(ctx context.Context, path string, key []byte, o ConvertOpts) (backup string, err error) {
	if len(key) != keys.Size {
		return "", errors.New("invalid key length")
	}
	return convert(ctx, path, hex.EncodeToString(key), "", o)
}

// Recover repairs an encrypt or decrypt run that was interrupted (a marker
// file left behind) and reports whether there was one. The archive is put
// back as it was before that run.
func Recover(path string) (bool, error) {
	if _, err := os.Stat(MarkerPath(path)); err != nil {
		return false, nil
	}
	lk, err := lockMarker(path)
	if err != nil {
		return false, err
	}
	if err := recoverStale(path, lk); err != nil {
		lk.Release()
		return false, err
	}
	lk.Release()
	return true, os.Remove(MarkerPath(path))
}

// markerState is what a run records in its marker file.
type markerState struct {
	PID     int    `json:"pid"`
	Started string `json:"started"`
	Backup  string `json:"backup,omitempty"` // base name the old file is moved to
	Temp    string `json:"temp,omitempty"`   // base name of the copy being built
}

func lockMarker(path string) (*filelock.Lock, error) {
	lk, err := filelock.Try(MarkerPath(path))
	if errors.Is(err, filelock.ErrLocked) {
		return nil, fmt.Errorf("another encrypt or decrypt run is active (%s)", MarkerPath(path))
	}
	return lk, err
}

func writeMarker(lk *filelock.Lock, s markerState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f := lk.File()
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(append(b, '\n'), 0); err != nil {
		return err
	}
	return f.Sync()
}

func tempPath(path string) string { return path + ".converting" }

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// sideFiles are the files SQLite keeps next to a database.
var sideFiles = []string{"-wal", "-shm", "-journal"}

// recoverStale undoes what an interrupted run left: the half-built copy is
// removed, and when the old file had already been moved away and nothing took
// its place it is moved back.
func recoverStale(path string, lk *filelock.Lock) error {
	var s markerState
	if b, err := io.ReadAll(io.NewSectionReader(lk.File(), 0, 1<<16)); err == nil {
		json.Unmarshal(b, &s) // an empty marker: the run stopped before the swap
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	if !exists(path) {
		// Only names this package generates are trusted.
		b := filepath.Base(s.Backup)
		if s.Backup == "" || !strings.HasPrefix(b, base+".") || !exists(filepath.Join(dir, b)) {
			return fmt.Errorf("interrupted run left no archive at %s and its marker %s names no usable copy; look for %s.plaintext-* or %s.encrypted-* in %s", path, MarkerPath(path), base, base, dir)
		}
		if err := os.Rename(filepath.Join(dir, b), path); err != nil {
			return err
		}
	}
	removeDB(tempPath(path))
	return nil
}

func removeDB(p string) {
	os.Remove(p)
	for _, s := range sideFiles {
		os.Remove(p + s)
	}
}

// openRaw opens path as it is (no schema migration, no journal mode change),
// plaintext when hexKey is empty.
func openRaw(path, hexKey string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(2000)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("mode", "rw")
	}
	withKey(q, hexKey)
	db, err := sql.Open("sqlite3", dsn(path, q))
	if err != nil {
		return nil, redact(err, hexKey)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// holdExclusively makes conn the only user of its database and keeps it so
// until conn is closed. Leaving WAL mode succeeds only when no other
// connection has the database open; after that the connection takes the
// exclusive file lock and, in exclusive locking mode, never gives it back.
func holdExclusively(ctx context.Context, conn *sql.Conn, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	retry := func(step func() (bool, error)) error {
		for {
			ok, err := step()
			if ok {
				return nil
			}
			if err != nil && !errors.Is(err, sqlite3.BUSY) && !errors.Is(err, sqlite3.LOCKED) {
				return err
			}
			if time.Now().After(deadline) {
				return errors.New("another process has the archive open (a running session-index command, an embed run or a hook); try again when it has finished")
			}
			if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
				return err
			}
		}
	}
	if err := retry(func() (bool, error) {
		var mode string
		err := conn.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode)
		return err == nil && strings.EqualFold(mode, "delete"), err
	}); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA locking_mode=EXCLUSIVE`); err != nil {
		return err
	}
	return retry(func() (bool, error) {
		if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
			return false, err
		}
		_, err := conn.ExecContext(ctx, `COMMIT`)
		return err == nil, err
	})
}

// tableCounts returns the row count of every table (virtual and shadow tables
// included) of the database behind q.
func tableCounts(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(names))
	for _, n := range names {
		var c int64
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM "`+strings.ReplaceAll(n, `"`, `""`)+`"`).Scan(&c); err != nil {
			return nil, fmt.Errorf("count %s: %w", n, err)
		}
		out[n] = c
	}
	return out, nil
}

// verifyCopy checks the finished copy at tmp against the source counts.
func verifyCopy(ctx context.Context, tmp, hexKey string, want map[string]int64) error {
	db, err := openRaw(tmp, hexKey, true)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := probe(db, tmp, hexKey); err != nil {
		return err
	}
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return redact(err, hexKey)
	}
	if res != "ok" {
		return fmt.Errorf("integrity_check of the copy: %s", res)
	}
	got, err := tableCounts(ctx, db)
	if err != nil {
		return redact(err, hexKey)
	}
	if len(got) != len(want) {
		return fmt.Errorf("the copy has %d tables, the archive %d", len(got), len(want))
	}
	for n, w := range want {
		if g, ok := got[n]; !ok || g != w {
			return fmt.Errorf("table %s: the copy has %d rows, the archive %d", n, g, w)
		}
	}
	return nil
}

func syncFile(p string) error {
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// convert rewrites the archive at path from srcKey to dstKey ("" = plaintext).
func convert(ctx context.Context, path, srcKey, dstKey string, o ConvertOpts) (backup string, err error) {
	if o.Wait == 0 {
		o.Wait = 30 * time.Second
	}
	if o.Settle == 0 {
		o.Settle = time.Second
	}
	redactAll := func(e error) error { return redact(redact(e, srcKey), dstKey) }
	setupCompilationCache(path)

	marker := MarkerPath(path)
	stale := exists(marker)
	lk, err := lockMarker(path)
	if err != nil {
		return "", err
	}
	// The marker outlives this call only when the archive could not be put
	// back in a usable state; Recover reads it then.
	keepMarker := false
	defer func() {
		lk.Release()
		if !keepMarker {
			os.Remove(marker)
		}
	}()
	if stale {
		o.say("repairing an interrupted run")
		if err := recoverStale(path, lk); err != nil {
			keepMarker = true
			return "", err
		}
	}
	state := markerState{PID: os.Getpid(), Started: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMarker(lk, state); err != nil {
		return "", err
	}

	have, err := Sniff(path)
	if err != nil {
		return "", err
	}
	switch {
	case have == FileAbsent:
		return "", fmt.Errorf("no archive at %s", path)
	case srcKey == "" && have != FilePlain:
		return "", fmt.Errorf("%s is not a plaintext archive (already encrypted?)", path)
	case srcKey != "" && have != FileOpaque:
		return "", fmt.Errorf("%s is not encrypted", path)
	}

	src, err := openRaw(path, srcKey, false)
	if err != nil {
		return "", err
	}
	defer src.Close()
	conn, err := src.Conn(ctx)
	if err != nil {
		return "", redactAll(err)
	}
	defer conn.Close()
	if srcKey != "" {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
			if errors.Is(err, sqlite3.NOTADB) {
				return "", fmt.Errorf("%w: %s", ErrWrongKey, path)
			}
			return "", redactAll(err)
		}
	}
	o.say("waiting for sole access to the archive")
	if err := holdExclusively(ctx, conn, o.Wait); err != nil {
		return "", redactAll(err)
	}

	tmp := tempPath(path)
	removeDB(tmp)
	failed := true
	defer func() {
		if failed && !keepMarker {
			removeDB(tmp)
		}
	}()
	o.say("copying")
	q := url.Values{}
	withKey(q, dstKey)
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, dsn(tmp, q)); err != nil {
		return "", fmt.Errorf("copy: %w", redactAll(err))
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		return "", err
	}
	if got, err := Sniff(tmp); err != nil {
		return "", err
	} else if (dstKey == "") != (got == FilePlain) {
		return "", errors.New("the copy is not in the expected form")
	}
	o.say("verifying the copy")
	want, err := tableCounts(ctx, conn)
	if err != nil {
		return "", redactAll(err)
	}
	if err := verifyCopy(ctx, tmp, dstKey, want); err != nil {
		return "", fmt.Errorf("verify: %w", redactAll(err))
	}

	// Swap. The old file keeps its content and is only renamed.
	kind := "plaintext"
	if srcKey != "" {
		kind = "encrypted"
	}
	backup = fmt.Sprintf("%s.%s-%s", path, kind, time.Now().UTC().Format("20060102-150405"))
	if exists(backup) {
		return "", fmt.Errorf("%s already exists", backup)
	}
	state.Backup, state.Temp = filepath.Base(backup), filepath.Base(tmp)
	if err := writeMarker(lk, state); err != nil {
		return "", err
	}
	release := func() { conn.Close(); src.Close() }
	if runtime.GOOS == "windows" {
		release() // an open file cannot be renamed there
	}
	o.say("swapping")
	if err := os.Rename(path, backup); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		if rerr := os.Rename(backup, path); rerr != nil {
			keepMarker = true
			return "", fmt.Errorf("swap failed (%v) and the archive could not be moved back (%v): it is at %s", err, rerr, backup)
		}
		return "", err
	}
	failed = false
	// Up to here this connection held the old file exclusively. A process
	// that opened the old file before that and was waiting for the lock goes
	// on with it now, under the archive's name for its -wal and -shm files,
	// which the new file must never share. Let it show itself, and if it
	// does give it its file back.
	release()
	sleepCtx(context.Background(), o.Settle)
	for _, s := range sideFiles {
		if !exists(path + s) {
			continue
		}
		err1 := os.Rename(path, tmp)
		err2 := os.Rename(backup, path)
		if err1 != nil || err2 != nil {
			keepMarker = true
			return "", fmt.Errorf("another process used the archive during the swap and undoing it failed (%v, %v); the previous archive is %s", err1, err2, backup)
		}
		removeDB(tmp)
		return "", errors.New("another process started using the archive during the swap; nothing was changed, try again when no session-index command or hook is running")
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync() // best effort: make the renames durable
		d.Close()
	}
	return backup, nil
}
