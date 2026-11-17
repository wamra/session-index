package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/wamra/session-index/internal/filelock"
	"path/filepath"
	"time"

	"github.com/wamra/session-index/internal/provider"
	"github.com/wamra/session-index/internal/store"
)

const (
	maxBatch   = 128
	failedMark = "failed"
)

// maxBatchBytes caps the text bytes per batch (var so tests can lower it).
var maxBatchBytes = 1 << 20

type pending struct {
	id                        int64
	text, session, kind, role string
	ts                        string
}

// ChunksPending counts chunks awaiting an embedding.
func ChunksPending(ctx context.Context, st *store.Store) (int, error) {
	var n int
	err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`).Scan(&n)
	return n, err
}

// Drain embeds all pending chunks and returns how many were embedded.
// It refuses with ErrFingerprintMismatch unless meta.embedder_fingerprint
// equals c.Fingerprint() (the pre-provider fingerprint counts as its
// equivalent), and fails with provider.ErrBroken when c's vector size differs
// from the archive's chunk_vec.
func Drain(ctx context.Context, st *store.Store, c provider.Embedder, batch int) (int, error) {
	return DrainCapped(ctx, st, c, batch, 0, nil)
}

// ErrDailyCap is returned by DrainCapped when embed.max_items_per_day is
// reached; the remaining chunks stay pending.
type ErrDailyCap struct{ Used, Cap int }

func (e *ErrDailyCap) Error() string {
	return fmt.Sprintf("daily cap reached: %d of %d chunks embedded today (UTC); remaining chunks stay pending", e.Used, e.Cap)
}

// DrainCapped is Drain that also stops with *ErrDailyCap once maxPerDay chunks
// (0 = unlimited) were sent to the provider on the current UTC day, counted in
// the archive's meta table across runs. now is the clock (nil = time.Now).
func DrainCapped(ctx context.Context, st *store.Store, c provider.Embedder, batch, maxPerDay int, now func() time.Time) (int, error) {
	if now == nil {
		now = time.Now
	}
	if batch <= 0 || batch > maxBatch {
		batch = maxBatch
	}
	lf, err := lockFile(st)
	if err != nil {
		return 0, err
	}
	defer lf.release()

	var fp string
	if err := st.DB().QueryRowContext(ctx, `SELECT value FROM meta WHERE key='embedder_fingerprint'`).Scan(&fp); err != nil {
		return 0, err
	}
	if c.Dims() == 0 { // unknown size: detect it with one probe request
		if _, err := c.Embed(ctx, []string{"dimension probe"}, ""); err != nil {
			return 0, err
		}
	}
	if provider.NormalizeFingerprint(fp) != c.Fingerprint() {
		return 0, ErrFingerprintMismatch
	}
	dims, err := store.VecDims(st.DB())
	if err != nil {
		return 0, err
	}
	if dims == 0 {
		dims = store.VecDim
	}
	if c.Dims() != dims {
		return 0, fmt.Errorf("%w: embedder dimension %d, archive chunk_vec has %d (run rebuild-derived)", ErrEmbedderBroken, c.Dims(), dims)
	}

	total := 0
	for ctx.Err() == nil {
		limit := batch
		if maxPerDay > 0 {
			used, err := store.UsageToday(ctx, st.DB(), store.UsageEmbed, now())
			if err != nil {
				return total, err
			}
			if used >= maxPerDay {
				if n, err := ChunksPending(ctx, st); err != nil {
					return total, err
				} else if n > 0 {
					return total, &ErrDailyCap{Used: used, Cap: maxPerDay}
				}
				break
			}
			limit = min(limit, maxPerDay-used)
		}
		rows, err := loadPending(ctx, st.DB(), limit)
		if err != nil {
			return total, err
		}
		if len(rows) == 0 {
			break
		}
		// "items sent": counted before the request, in its own transaction, so a
		// failed or killed batch still counts (an upper bound of billed items).
		if err := store.AddUsage(context.WithoutCancel(ctx), st.DB(), store.UsageEmbed, len(rows), now()); err != nil {
			return total, err
		}
		good, vecs, bad, err := embedBisect(ctx, c, rows)
		if err != nil {
			return total, err
		}
		if err := commitBatch(ctx, st.DB(), good, vecs, bad); err != nil {
			return total, err
		}
		total += len(good)
	}
	return total, ctx.Err()
}

func loadPending(ctx context.Context, db *sql.DB, limit int) ([]pending, error) {
	rs, err := db.QueryContext(ctx, `SELECT c.id, c.text, c.session_id, c.kind, c.role, c.ts
FROM chunks c WHERE c.embedded_at IS NULL ORDER BY c.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []pending
	size := 0
	for rs.Next() {
		var p pending
		var sid, kind, role, ts sql.NullString
		if err := rs.Scan(&p.id, &p.text, &sid, &kind, &role, &ts); err != nil {
			return nil, err
		}
		p.session, p.kind, p.role, p.ts = sid.String, kind.String, role.String, ts.String
		p.text = capRunes(p.text, maxChunk)
		if len(out) > 0 && size+len(p.text) > maxBatchBytes {
			break
		}
		size += len(p.text)
		out = append(out, p)
	}
	return out, rs.Err()
}

// embedBisect embeds rows; on a row-level error it bisects to isolate bad rows.
// Systemic failures (server down, wrong dimension, or no row in the batch
// succeeding) return an error and park nothing.
func embedBisect(ctx context.Context, c provider.Embedder, rows []pending) (good []pending, vecs [][]float32, bad []pending, err error) {
	good, vecs, bad, err = bisect(ctx, c, rows)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(good) == 0 && len(bad) > 1 {
		return nil, nil, nil, fmt.Errorf("%w: every row in a batch of %d failed", ErrEmbedderBroken, len(rows))
	}
	return good, vecs, bad, nil
}

func bisect(ctx context.Context, c provider.Embedder, rows []pending) (good []pending, vecs [][]float32, bad []pending, err error) {
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.text
	}
	v, err := c.Embed(ctx, texts, provider.DocPrefixOf(c))
	if err == nil {
		for _, x := range v {
			if len(x) != c.Dims() {
				return nil, nil, nil, fmt.Errorf("%w: embedding dimension %d, expected %d", ErrEmbedderBroken, len(x), c.Dims())
			}
		}
		return rows, v, nil, nil
	}
	if provider.IsSystemic(err) {
		return nil, nil, nil, err
	}
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	if len(rows) == 1 {
		return nil, nil, rows, nil
	}
	mid := len(rows) / 2
	g1, v1, b1, err := bisect(ctx, c, rows[:mid])
	if err != nil {
		return nil, nil, nil, err
	}
	g2, v2, b2, err := bisect(ctx, c, rows[mid:])
	if err != nil {
		return nil, nil, nil, err
	}
	// fresh slices: g1/b1 may alias rows, so appending in place would corrupt it
	return append(append([]pending{}, g1...), g2...), append(append([][]float32{}, v1...), v2...), append(append([]pending{}, b1...), b2...), nil
}

func commitBatch(ctx context.Context, db *sql.DB, good []pending, vecs [][]float32, bad []pending) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	for i, p := range good {
		var project sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT project FROM sessions WHERE session_id=?`, p.session).Scan(&project); err != nil && err != sql.ErrNoRows {
			return err
		}
		var ts int64
		if t, err := time.Parse(time.RFC3339, p.ts); err == nil {
			ts = t.Unix()
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vec WHERE rowid=?`, p.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_vec(rowid, embedding, session_id, project, kind, role, ts) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.id, VecBytes(vecs[i]), p.session, project.String, p.kind, p.role, ts); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedded_at=? WHERE id=?`, now, p.id); err != nil {
			return err
		}
	}
	for _, p := range bad {
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedded_at=? WHERE id=?`, failedMark, p.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type lock struct{ l *filelock.Lock }

func lockFile(st *store.Store) (*lock, error) {
	l, err := filelock.Try(filepath.Join(filepath.Dir(st.Path()), "embed.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return nil, ErrLocked
	}
	if err != nil {
		return nil, err
	}
	return &lock{l}, nil
}

func (l *lock) release() { l.l.Release() }

// Lock is a held embed lock (see TryLock).
type Lock struct{ l *lock }

// TryLock takes the embed flock without blocking; it returns ErrLocked when
// another process (an embedder or rebuild-derived) holds it.
func TryLock(st *store.Store) (*Lock, error) {
	l, err := lockFile(st)
	if err != nil {
		return nil, err
	}
	return &Lock{l}, nil
}

// Release drops the lock.
func (k *Lock) Release() { k.l.release() }
