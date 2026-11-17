package store

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/wamra/session-index/internal/provider"
)

// Opts configures OpenWith / MigrateWith.
type Opts struct {
	// Profile backfills NULL profile columns when non-empty.
	Profile string
	// Dims is the configured embedding size. A new archive creates chunk_vec
	// with it (0 = VecDim). On an existing archive the stored size wins and a
	// non-zero, different Dims is a *DimsMismatchError unless AllowDimsChange.
	Dims int
	// AllowDimsChange skips the mismatch check (rebuild-derived, doctor).
	AllowDimsChange bool
	// Fingerprint is stored as meta.embedder_fingerprint on a new archive
	// (default: the default embedder's fingerprint).
	Fingerprint string
}

// DimsMismatchError reports a configured embedding size that differs from the
// archive's chunk_vec.
type DimsMismatchError struct{ Stored, Config int }

func (e *DimsMismatchError) Error() string {
	return fmt.Sprintf("embedding dimensions mismatch: the archive stores %d, the configuration wants %d; revert the embedder configuration, or run `session-index rebuild-derived` to recreate chunk_vec at %d and then `session-index embed` to re-embed", e.Stored, e.Config, e.Config)
}

// CreateChunkVecSQL returns the statement that creates chunk_vec with dims.
func CreateChunkVecSQL(dims int) string {
	return fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS chunk_vec USING vec0(
  embedding float[%d],
  session_id text,
  project text,
  kind text,
  role text,
  ts integer
)`, dims)
}

var vecDimsRe = regexp.MustCompile(`(?i)float\[(\d+)\]`)

// VecDims returns meta.vec_dims (0 when unset).
func VecDims(db *sql.DB) (int, error) {
	var v string
	err := db.QueryRow(`SELECT value FROM meta WHERE key='vec_dims'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("meta.vec_dims %q: %w", v, err)
	}
	return n, nil
}

func ensureVec(db *sql.DB, o Opts) error {
	stored, err := VecDims(db)
	if err != nil {
		return err
	}
	if stored == 0 {
		var ddl string
		err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='chunk_vec'`).Scan(&ddl)
		switch {
		case err == sql.ErrNoRows: // new archive
			stored = o.Dims
			if stored <= 0 {
				stored = VecDim
			}
			if _, err := db.Exec(CreateChunkVecSQL(stored)); err != nil {
				return err
			}
			fp := o.Fingerprint
			if fp == "" {
				fp = provider.NormalizedLegacyFingerprint
			}
			if _, err := db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('embedder_fingerprint', ?)`, fp); err != nil {
				return err
			}
		case err != nil:
			return err
		default: // archive that predates meta.vec_dims
			m := vecDimsRe.FindStringSubmatch(ddl)
			if m == nil {
				return fmt.Errorf("cannot read the vector size from chunk_vec: %s", ddl)
			}
			stored, _ = strconv.Atoi(m[1])
		}
		if _, err := db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES ('vec_dims', ?)`, strconv.Itoa(stored)); err != nil {
			return err
		}
	}
	if err := normalizeFingerprint(db); err != nil {
		return err
	}
	if o.Dims > 0 && o.Dims != stored && !o.AllowDimsChange {
		return &DimsMismatchError{Stored: stored, Config: o.Dims}
	}
	return nil
}

// normalizeFingerprint rewrites the pre-provider fingerprint to its
// equivalent current form (once).
func normalizeFingerprint(db *sql.DB) error {
	res, err := db.Exec(`UPDATE meta SET value=? WHERE key='embedder_fingerprint' AND value=?`, provider.NormalizedLegacyFingerprint, provider.LegacyFingerprint)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		fmt.Fprintf(os.Stderr, "session-index: embedder fingerprint rewritten %q -> %q\n", provider.LegacyFingerprint, provider.NormalizedLegacyFingerprint)
	}
	return nil
}

// EffectiveVecDims is meta.vec_dims, or when that is unset (an archive not yet
// opened by a writer after the upgrade, e.g. through OpenReadOnly) the size
// parsed from the chunk_vec DDL in sqlite_master. 0 when neither is known.
func EffectiveVecDims(db *sql.DB) (int, error) {
	if n, err := VecDims(db); err != nil || n > 0 {
		return n, err
	}
	var ddl string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='chunk_vec'`).Scan(&ddl)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if m := vecDimsRe.FindStringSubmatch(ddl); m != nil {
		return strconv.Atoi(m[1])
	}
	return 0, nil
}
