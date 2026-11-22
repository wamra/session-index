// Package store owns the SQLite archive: connection setup and schema.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/wamra/session-index/internal/provider"
)

// The sqlite-vec wasm build uses atomics, which wazero gates behind the
// threads feature flag. Must be set before the first connection is opened.
func init() { sqlite3.RuntimeConfig = baseConfig() }

func baseConfig() wazero.RuntimeConfig {
	return wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads)
}

var cacheOnce sync.Once

// setupCompilationCache points the wazero runtime at an on-disk compilation
// cache (<db dir>/wazero-cache, else <user cache dir>/session-index/wazero-cache)
// so the sqlite wasm is compiled once, not on every process start. Failures
// are non-fatal: the process continues uncached. It runs once, before the
// first connection is opened.
func setupCompilationCache(dbPath string) {
	cacheOnce.Do(func() {
		dir := ""
		if dbPath != "" {
			dir = filepath.Join(filepath.Dir(dbPath), "wazero-cache")
		} else if uc, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(uc, "session-index", "wazero-cache")
		}
		if dir == "" {
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "session-index: compilation cache disabled: %v\n", err)
			return
		}
		cache, err := wazero.NewCompilationCacheWithDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "session-index: compilation cache disabled: %v\n", err)
			return
		}
		sqlite3.RuntimeConfig = baseConfig().WithCompilationCache(cache)
	})
}

// VecDim is the default embedding dimensionality of chunk_vec, used when an
// archive is created without a configured size (see Opts.Dims).
const VecDim = provider.DefaultDims

//go:embed schema.sql
var schemaSQL string

// Store wraps the archive database handle.
type Store struct {
	db   *sql.DB
	path string
}

// JournalMode is the journal mode every writable connection asks for: WAL on
// every platform.
//
// Windows used a rollback journal while the pinned SQLite driver had three
// WAL defects there (docs/REFERENCE.md, Design note). go.mod
// now replaces the driver with a fork of the same version that fixes them.
var JournalMode = journalMode(runtime.GOOS)

func journalMode(goos string) string {
	return "WAL"
}

// dsn builds the "file:" URI for path. The path is percent-encoded and
// written without a "//" authority, so a Windows path (C:\...) and a path
// containing '%', '#' or '?' reach SQLite unchanged.
func dsn(path string, q url.Values) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Open opens (creating if needed) the archive at path and applies the schema.
// An encrypted archive is recognized and opened with its key (encryption.go).
// Rows that predate the profile columns are left with a NULL profile; use
// OpenProfile to backfill them.
func Open(path string) (*Store, error) { return OpenProfile(path, "") }

// OpenProfile is Open that also backfills files.profile and sessions.profile
// with profile (when non-empty) for rows where it is NULL.
func OpenProfile(path, profile string) (*Store, error) {
	return OpenWith(path, Opts{Profile: profile})
}

// OpenWith is OpenProfile with embedder settings (see Opts).
func OpenWith(path string, o Opts) (*Store, error) {
	if err := checkMarker(path); err != nil {
		return nil, err
	}
	key, err := keyFor(path)
	if err != nil {
		return nil, err
	}
	setupCompilationCache(path)
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(30000)")
	q.Add("_pragma", "journal_mode("+JournalMode+")")
	q.Add("_pragma", "synchronous(FULL)")
	if key != "" {
		withKey(q, key)
	}
	db, err := sql.Open("sqlite3", dsn(path, q))
	if err != nil {
		return nil, redact(err, key)
	}
	if key != "" {
		if err := probe(db, path, key); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := MigrateWith(db, o); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", redact(err, key))
	}
	return &Store{db: db, path: path}, nil
}

// OpenReadOnly opens an existing archive read-only (no migration, no writes,
// query_only). It is used for peer archives synced from other machines.
func OpenReadOnly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	if err := checkMarker(path); err != nil {
		return nil, err
	}
	key, err := keyFor(path)
	if err != nil {
		return nil, err
	}
	setupCompilationCache("")
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	if key != "" {
		withKey(q, key)
	}
	db, err := sql.Open("sqlite3", dsn(path, q))
	if err != nil {
		return nil, redact(err, key)
	}
	// Force a real open and schema read so a broken file fails here.
	if err := probe(db, path, key); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, path: path}, nil
}

// ErrCheckpointBusy is returned by Checkpoint when another connection (for
// example a running embed) prevented the WAL from being truncated.
var ErrCheckpointBusy = errors.New("wal checkpoint busy")

// Checkpoint runs PRAGMA wal_checkpoint(TRUNCATE) so the main database file is
// complete on its own (-wal empty). It waits at most ~2 s for other
// connections; a busy result is reported as an error. Without a WAL (see
// JournalMode) there is nothing to fold in and it returns nil.
func (s *Store) Checkpoint() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=2000`); err != nil {
		return err
	}
	var busy, logFrames, ckpt int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &ckpt); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("%w (log=%d checkpointed=%d)", ErrCheckpointBusy, logFrames, ckpt)
	}
	return nil
}

// HasColumn reports whether table has a column named col.
func HasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
		if n == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path given to Open.
func (s *Store) Path() string { return s.path }

// DB returns the underlying handle.
func (s *Store) DB() *sql.DB { return s.db }

// Migrate applies schema.sql idempotently and adds the profile and mode columns.
func Migrate(db *sql.DB) error { return MigrateWith(db, Opts{}) }

// MigrateProfile is Migrate that also backfills NULL profile values with
// profile when it is non-empty.
func MigrateProfile(db *sql.DB, profile string) error { return MigrateWith(db, Opts{Profile: profile}) }

// MigrateWith applies the schema and column migrations, creates chunk_vec
// (new archives), validates the vector size and normalizes the legacy
// embedder fingerprint, as described by o.
func MigrateWith(db *sql.DB, o Opts) error {
	profile := o.Profile
	if _, err := db.Exec(schemaSQL); err != nil {
		return err
	}
	if err := ensureVec(db, o); err != nil {
		return err
	}
	ok, err := HasColumn(db, "files", "mode")
	if err != nil {
		return err
	}
	if !ok {
		if _, err := db.Exec(`ALTER TABLE files ADD COLUMN mode INTEGER`); err != nil {
			return err
		}
	}
	for _, col := range []string{"failed_at", "fail_reason", "next_retry_at"} {
		has, err := HasColumn(db, "assets", col)
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE assets ADD COLUMN ` + col + ` TEXT`); err != nil {
				return err
			}
		}
	}
	ok, err = HasColumn(db, "summaries", "change_id")
	if err != nil {
		return err
	}
	if !ok {
		if _, err := db.Exec(`ALTER TABLE summaries ADD COLUMN change_id INTEGER NULL REFERENCES file_changes(id)`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS summaries_change ON summaries(change_id)`); err != nil {
		return err
	}
	for _, t := range []string{"files", "sessions"} {
		ok, err := HasColumn(db, t, "profile")
		if err != nil {
			return err
		}
		if !ok {
			if _, err := db.Exec(`ALTER TABLE ` + t + ` ADD COLUMN profile TEXT`); err != nil {
				return err
			}
		}
		if profile != "" {
			if _, err := db.Exec(`UPDATE `+t+` SET profile=? WHERE profile IS NULL`, profile); err != nil {
				return err
			}
		}
	}
	return nil
}
