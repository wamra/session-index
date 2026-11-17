package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wamra/session-index/internal/jsonl"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/zst"
)

// Rederive empties the derived tables (messages, chunk_vec, chunks, chunk_fts,
// tool_fts, sessions), stores the embedder fingerprint (when non-empty) and
// rebuilds the derived rows from records.raw for every non-superseded record,
// all in ONE BEGIN IMMEDIATE transaction (a failure leaves the old index
// intact). It returns the number of records processed. records and files are
// not touched. Callers should hold the embed lock.
func Rederive(ctx context.Context, st *store.Store, fingerprint string) (int, error) {
	return RederiveDims(ctx, st, fingerprint, 0)
}

// RederiveDims is Rederive that, when dims > 0, also recreates chunk_vec with
// that vector size (DROP + CREATE inside the same transaction) and stores it
// as meta.vec_dims, so a changed embedder can be switched in.
func RederiveDims(ctx context.Context, st *store.Store, fingerprint string, dims int) (int, error) {
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
	// Summaries keep their file_changes link across the rebuild by
	// (record_id, tool_use_id, file_path).
	for _, q := range []string{
		`CREATE TEMP TABLE IF NOT EXISTS _sc(id INTEGER PRIMARY KEY, record_id INTEGER, tool_use_id TEXT, file_path TEXT)`,
		`DELETE FROM _sc`,
		`INSERT INTO _sc SELECT s.id, fc.record_id, fc.tool_use_id, fc.file_path FROM summaries s JOIN file_changes fc ON fc.id=s.change_id`,
		`UPDATE summaries SET change_id=NULL WHERE change_id IS NOT NULL`,
		`DELETE FROM tool_outputs`,
		`DELETE FROM file_changes`,
		`DELETE FROM file_fts`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return 0, fmt.Errorf("%s: %w", q, err)
		}
	}
	for _, q := range []string{
		`DELETE FROM messages`,
		`INSERT INTO chunk_fts(chunk_fts) VALUES('delete-all')`,
		`DELETE FROM chunks`,
		`DELETE FROM tool_fts`,
		`DELETE FROM sessions`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return 0, fmt.Errorf("%s: %w", q, err)
		}
	}
	if dims > 0 {
		for _, q := range []string{`DROP TABLE IF EXISTS chunk_vec`, store.CreateChunkVecSQL(dims)} {
			if _, err := conn.ExecContext(ctx, q); err != nil {
				return 0, fmt.Errorf("%s: %w", q, err)
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key,value) VALUES('vec_dims', ?)`, strconv.Itoa(dims)); err != nil {
			return 0, err
		}
	} else if _, err := conn.ExecContext(ctx, `DELETE FROM chunk_vec`); err != nil {
		return 0, err
	}
	if fingerprint != "" {
		if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key,value) VALUES('embedder_fingerprint', ?)`, fingerprint); err != nil {
			return 0, err
		}
	}
	stm, err := prepare(ctx, conn)
	if err != nil {
		return 0, err
	}
	defer stm.close()

	type fileRow struct {
		id              int64
		path, sid, kind string
		agent, profile  sql.NullString
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, path, COALESCE(session_id,''), COALESCE(kind,''), agent_id, profile FROM files ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var files []fileRow
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.id, &f.path, &f.sid, &f.kind, &f.agent, &f.profile); err != nil {
			rows.Close()
			return 0, err
		}
		files = append(files, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, f := range files {
		info := paths.Info{SessionID: f.sid, Kind: f.kind, AgentID: f.agent.String, ProjectSlug: slugOf(f.path, f.sid), Profile: f.profile.String}
		var sess session
		n, err := rederiveFile(ctx, conn, stm, info, f.id, &sess)
		if err != nil {
			return total, err
		}
		total += n
		if n > 0 && f.sid != "" {
			if err := upsertSession(ctx, conn, info, sess); err != nil {
				return total, err
			}
		}
	}
	if err := rebuildExtras(ctx, conn); err != nil {
		return total, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key,value) VALUES(?, '1')`, extrasFlag); err != nil {
		return total, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return total, err
	}
	committed = true
	return total, nil
}

func rederiveFile(ctx context.Context, conn *sql.Conn, stm *stmts, info paths.Info, fileID int64, sess *session) (int, error) {
	type rec struct {
		id  int64
		raw []byte
	}
	rows, err := conn.QueryContext(ctx, `SELECT id, raw FROM records WHERE file_id=? AND superseded=0 ORDER BY line_no`, fileID)
	if err != nil {
		return 0, err
	}
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			rows.Close()
			return 0, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for i, r := range recs {
		if i%1000 == 0 {
			if err := ctx.Err(); err != nil {
				return i, err
			}
		}
		line, err := zst.Decompress(r.raw)
		if err != nil {
			return i, err
		}
		if len(line) == 0 {
			continue
		}
		pr, perr := jsonl.Parse(line)
		var cwd, branch string
		hasUsage := false
		if perr == nil {
			cwd, branch, hasUsage = probe(line, pr.Msg != nil)
		}
		if cwd != "" {
			sess.cwd = cwd
		}
		if branch != "" {
			sess.branch = branch
		}
		if pr.AITitle != "" {
			sess.title = pr.AITitle
		}
		if pr.CustomTitle != "" {
			sess.customTitle = pr.CustomTitle
		}
		if pr.Msg == nil {
			continue
		}
		if _, err := stm.derive(ctx, info, r.id, pr, hasUsage); err != nil {
			return i, err
		}
	}
	return len(recs), nil
}

// slugOf returns the project directory name of a transcript path: the path
// element immediately before the session id (as directory or file stem).
func slugOf(path, sid string) string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := len(parts) - 1; i >= 1; i-- {
		if parts[i] == sid || parts[i] == sid+".jsonl" {
			return parts[i-1]
		}
	}
	return ""
}
