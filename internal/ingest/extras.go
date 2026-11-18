package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wamra/session-index/internal/jsonl"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/zst"
)

// ChangeChunkText is the chunk text of a file-change summary.
func ChangeChunkText(filePath, tool, ts, summary string) string {
	d := ts
	if len(d) > 10 {
		d = d[:10]
	}
	return fmt.Sprintf("%s (%s %s): %s", filePath, tool, d, summary)
}

// rebuildExtras recreates what Rederive wiped but cannot derive from records:
// memory sessions and chunks (from assets), the summary chunks of tool
// outputs, tool-result assets and file changes, and the summaries -> file_changes
// links. Assets and summaries themselves are not modified.
func rebuildExtras(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `UPDATE summaries SET change_id=(
			SELECT fc.id FROM _sc JOIN file_changes fc ON fc.record_id=_sc.record_id
				AND COALESCE(fc.tool_use_id,'')=COALESCE(_sc.tool_use_id,'') AND fc.file_path=_sc.file_path
			WHERE _sc.id=summaries.id)
		WHERE id IN (SELECT id FROM _sc)`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM summaries WHERE id IN (SELECT id FROM _sc) AND change_id IS NULL`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS _sc`); err != nil {
		return err
	}
	if _, err := ResolvePointers(ctx, conn); err != nil {
		return err
	}

	if err := rederiveAssetChunks(ctx, conn); err != nil {
		return err
	}

	type sc struct {
		kind  string
		asset bool
		rec   int64
		sid   string
		ts    string
		text  string
		seq   int
	}
	var cs []sc
	collect := func(q string, f func(*sql.Rows) (sc, error)) error {
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := f(rows)
			if err != nil {
				return err
			}
			cs = append(cs, c)
		}
		return rows.Err()
	}
	if err := collect(`SELECT s.record_id, COALESCE(r.session_id,''), COALESCE(r.ts,''), s.text, s.idx FROM summaries s
		JOIN records r ON r.id=s.record_id WHERE s.text<>'' AND r.superseded=0 ORDER BY s.id`, func(r *sql.Rows) (sc, error) {
		var c sc
		var idx sql.NullInt64
		err := r.Scan(&c.rec, &c.sid, &c.ts, &c.text, &idx)
		c.kind, c.seq = "summary", 1000+int(idx.Int64)
		return c, err
	}); err != nil {
		return err
	}
	if err := collect(`SELECT a.id, COALESCE(a.session_id,''), a.mtime, s.text FROM summaries s
		JOIN assets a ON a.id=s.asset_id WHERE s.text<>'' ORDER BY s.id`, func(r *sql.Rows) (sc, error) {
		var c sc
		var mt int64
		err := r.Scan(&c.rec, &c.sid, &mt, &c.text)
		c.ts = time.Unix(0, mt).UTC().Format(time.RFC3339)
		c.asset = true
		c.kind, c.seq = "summary", 1000
		return c, err
	}); err != nil {
		return err
	}
	if err := collect(`SELECT fc.record_id, COALESCE(fc.session_id,''), COALESCE(fc.ts,''), s.text, fc.tool || char(0) || fc.file_path FROM summaries s
		JOIN file_changes fc ON fc.id=s.change_id WHERE s.text<>'' ORDER BY s.id`, func(r *sql.Rows) (sc, error) {
		var c sc
		var tp, sum string
		err := r.Scan(&c.rec, &c.sid, &c.ts, &sum, &tp)
		tool, path := splitNul(tp)
		c.text = ChangeChunkText(path, tool, c.ts, sum)
		c.kind, c.seq = "filechange", 1000
		return c, err
	}); err != nil {
		return err
	}
	for _, c := range cs {
		if c.asset {
			rid, err := AssetRecord(ctx, conn, c.rec, c.sid)
			if err != nil {
				return err
			}
			c.rec = rid
		}
		if _, err := AddChunk(ctx, conn, c.rec, c.sid, c.kind, "", c.ts, c.seq, c.text); err != nil {
			return err
		}
	}
	return nil
}

func baseName(p string) string { return filepath.Base(p) }

func splitNul(s string) (a, b string) {
	if i := strings.IndexByte(s, 0); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

const extrasFlag = "extras_derived_v1"

// BackfillExtras derives file_changes and tool_outputs for records archived
// before those tables existed, once (meta key extras_derived_v1), in one
// transaction. Records that already have rows are skipped. It returns the
// number of records inspected (0 when already done). Chunks, vectors and
// summaries are not touched.
func BackfillExtras(ctx context.Context, st *store.Store) (int, error) {
	var v string
	if st.DB().QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, extrasFlag).Scan(&v) == nil {
		return 0, nil
	}
	conn, err := st.DB().Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if st.DB().QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, extrasFlag).Scan(&v) == nil {
		return 0, nil // another process finished first
	}
	stm, err := prepare(ctx, conn)
	if err != nil {
		return 0, err
	}
	defer stm.close()
	done := map[int64]bool{}
	for _, q := range []string{`SELECT DISTINCT record_id FROM tool_outputs`, `SELECT DISTINCT record_id FROM file_changes`} {
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			done[id] = true
		}
		rows.Close()
	}
	type fileRow struct {
		id      int64
		sid     string
		profile sql.NullString
	}
	frows, err := conn.QueryContext(ctx, `SELECT id, session_id, profile FROM files WHERE session_id IS NOT NULL AND session_id<>'' AND kind<>'asset' ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var files []fileRow
	for frows.Next() {
		var f fileRow
		if err := frows.Scan(&f.id, &f.sid, &f.profile); err != nil {
			frows.Close()
			return 0, err
		}
		files = append(files, f)
	}
	frows.Close()
	n := 0
	for _, f := range files {
		rows, err := conn.QueryContext(ctx, `SELECT id, raw FROM records WHERE file_id=? AND superseded=0 AND byte_len>0 ORDER BY line_no`, f.id)
		if err != nil {
			return n, err
		}
		type rec struct {
			id  int64
			raw []byte
		}
		var recs []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.id, &r.raw); err != nil {
				rows.Close()
				return n, err
			}
			if !done[r.id] {
				recs = append(recs, r)
			}
		}
		rows.Close()
		info := paths.Info{SessionID: f.sid, Profile: f.profile.String}
		for i, r := range recs {
			if i%1000 == 0 {
				if err := ctx.Err(); err != nil {
					return n, err
				}
			}
			line, err := zst.Decompress(r.raw)
			if err != nil || len(line) == 0 {
				continue
			}
			pr, perr := jsonl.Parse(line)
			if perr != nil || pr.Msg == nil {
				continue
			}
			n++
			if err := stm.deriveExtras(ctx, info, r.id, pr); err != nil {
				return n, err
			}
		}
	}
	if _, err := ResolvePointers(ctx, conn); err != nil {
		return n, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key,value) VALUES(?, '1')`, extrasFlag); err != nil {
		return n, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return n, err
	}
	committed = true
	return n, nil
}

// rederiveAssetChunks recreates the session row and chunks of every memory
// asset and every plan asset.
func rederiveAssetChunks(ctx context.Context, conn *sql.Conn) error {
	type doc struct {
		id                 int64
		kind               string
		sid, path, profile sql.NullString
		mtime              int64
		raw                []byte
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, kind, session_id, path, profile, mtime, raw FROM assets WHERE kind IN (?,?) AND raw IS NOT NULL ORDER BY id`, paths.AssetMemory, paths.AssetPlans)
	if err != nil {
		return err
	}
	var ds []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.id, &d.kind, &d.sid, &d.path, &d.profile, &d.mtime, &d.raw); err != nil {
			rows.Close()
			return err
		}
		ds = append(ds, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range ds {
		data, err := zst.Decompress(d.raw)
		if err != nil {
			return fmt.Errorf("%s asset %d: %w", d.kind, d.id, err)
		}
		if d.kind == paths.AssetPlans {
			if IsTextAsset(d.path.String) {
				err = rederivePlan(ctx, conn, d.id, d.sid.String, d.path.String, d.profile.String, d.mtime, data)
			}
		} else {
			slug := d.sid.String
			if len(slug) > 7 {
				slug = slug[7:]
			}
			err = rederiveMemory(ctx, conn, d.id, d.sid.String, slug, baseName(d.path.String), d.profile.String, d.mtime, data)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ExtrasReport describes RebuildExtras.
type ExtrasReport struct {
	FileChangesBefore, FileChangesAfter int
	ToolOutputsBefore, ToolOutputsAfter int
	SummariesKept, SummariesDeleted     int // tool-output summaries kept; change summaries and orphaned tool summaries deleted
	ChunksDeleted                       int
}

// RebuildExtras re-derives file_changes, file_fts and tool_outputs from
// records.raw: it deletes those rows, the change summaries (summaries with a
// change_id) and the chunks (with chunk_vec and chunk_fts rows) that derive
// from them, clears the backfill flag and re-runs BackfillExtras. Tool-output
// summaries whose (record_id, idx) still exists are kept. Memory chunks are
// re-derived and anchor records lose their session. Callers should hold the
// embed and summarize locks.
func RebuildExtras(ctx context.Context, st *store.Store) (ExtrasReport, error) {
	var rep ExtrasReport
	conn, err := st.DB().Conn(ctx)
	if err != nil {
		return rep, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return rep, err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	one := func(q string, dst *int) error { return conn.QueryRowContext(ctx, q).Scan(dst) }
	if err := one(`SELECT count(*) FROM file_changes`, &rep.FileChangesBefore); err != nil {
		return rep, err
	}
	if err := one(`SELECT count(*) FROM tool_outputs`, &rep.ToolOutputsBefore); err != nil {
		return rep, err
	}
	n, err := deleteChunksWhere(ctx, conn, `kind='filechange'`)
	if err != nil {
		return rep, err
	}
	rep.ChunksDeleted += n
	r, err := conn.ExecContext(ctx, `DELETE FROM summaries WHERE change_id IS NOT NULL`)
	if err != nil {
		return rep, err
	}
	d, _ := r.RowsAffected()
	rep.SummariesDeleted += int(d)
	for _, q := range []string{
		`DELETE FROM file_fts`,
		`DELETE FROM file_changes`,
		`DELETE FROM tool_outputs`,
		`DELETE FROM meta WHERE key='` + extrasFlag + `'`,
		`UPDATE records SET session_id=NULL WHERE file_id IN (SELECT id FROM files WHERE path='` + assetAnchorPath + `')`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return rep, fmt.Errorf("%s: %w", q, err)
		}
	}
	if err := rederiveAssetChunks(ctx, conn); err != nil {
		return rep, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return rep, err
	}
	committed = true
	if _, err := BackfillExtras(ctx, st); err != nil {
		return rep, err
	}
	// Tool-output summaries must still point at an existing (record_id, idx).
	tx, err := st.DB().Conn(ctx)
	if err != nil {
		return rep, err
	}
	defer tx.Close()
	if _, err := tx.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return rep, err
	}
	committed2 := false
	defer func() {
		if !committed2 {
			tx.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	orphan := `record_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM tool_outputs t WHERE t.record_id=summaries.record_id AND t.idx=summaries.idx)`
	rows, err := tx.QueryContext(ctx, `SELECT record_id, idx FROM summaries WHERE `+orphan)
	if err != nil {
		return rep, err
	}
	type ri struct{ rec, idx int64 }
	var orphans []ri
	for rows.Next() {
		var o ri
		rows.Scan(&o.rec, &o.idx)
		orphans = append(orphans, o)
	}
	rows.Close()
	for _, o := range orphans {
		c, err := deleteChunksWhere(ctx, tx, fmt.Sprintf(`kind='summary' AND record_id=%d AND seq=%d`, o.rec, 1000+o.idx))
		if err != nil {
			return rep, err
		}
		rep.ChunksDeleted += c
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM summaries WHERE `+orphan); err != nil {
		return rep, err
	}
	rep.SummariesDeleted += len(orphans)
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM summaries WHERE record_id IS NOT NULL`).Scan(&rep.SummariesKept); err != nil {
		return rep, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM file_changes`).Scan(&rep.FileChangesAfter); err != nil {
		return rep, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tool_outputs`).Scan(&rep.ToolOutputsAfter); err != nil {
		return rep, err
	}
	if _, err := tx.ExecContext(ctx, `COMMIT`); err != nil {
		return rep, err
	}
	committed2 = true
	return rep, nil
}
