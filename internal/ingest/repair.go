package ingest

import (
	"context"
	"fmt"

	"github.com/wamra/session-index/internal/store"
)

// RepairReport counts the actions of Repair.
type RepairReport struct {
	Messages, Chunks, ChunkVec, ToolFTS int64 // orphan derived rows deleted
	FileChanges, Summaries, ToolOutputs int64
	Sessions                            int // sessions recomputed
}

// Repair deletes derived rows (messages, chunks with their chunk_vec rows,
// tool_fts) whose record is missing or superseded, recomputes the affected
// sessions, then rebuilds every FTS5 table (chunk_fts, tool_fts, asset_fts, file_fts and any added later) from its content, all in
// one BEGIN IMMEDIATE transaction. records and files are never touched and
// nothing is embedded. Callers should hold the embed lock.
func Repair(ctx context.Context, st *store.Store) (RepairReport, error) {
	var rep RepairReport
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
	const live = `(SELECT id FROM records WHERE superseded=0)`

	// Sessions whose superseded records still own derived rows.
	rows, err := conn.QueryContext(ctx, `SELECT DISTINCT session_id FROM records WHERE superseded=1 AND session_id IS NOT NULL AND (
		id IN (SELECT record_id FROM messages) OR id IN (SELECT record_id FROM chunks) OR id IN (SELECT record_id FROM file_changes) OR id IN (SELECT record_id FROM tool_fts))`)
	if err != nil {
		return rep, err
	}
	var sids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return rep, err
		}
		sids = append(sids, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	del := func(dst *int64, q string) error {
		r, err := conn.ExecContext(ctx, q)
		if err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
		n, _ := r.RowsAffected()
		if dst != nil {
			*dst += n
		}
		return nil
	}
	for _, step := range []struct {
		dst *int64
		q   string
	}{
		{&rep.Messages, `DELETE FROM messages WHERE record_id NOT IN ` + live},
		{&rep.ToolOutputs, `DELETE FROM tool_outputs WHERE record_id NOT IN ` + live},
		{nil, `DELETE FROM file_fts WHERE change_id IN (SELECT id FROM file_changes WHERE record_id NOT IN ` + live + `)`},
		{&rep.Summaries, `DELETE FROM summaries WHERE change_id IN (SELECT id FROM file_changes WHERE record_id NOT IN ` + live + `)`},
		{&rep.FileChanges, `DELETE FROM file_changes WHERE record_id NOT IN ` + live},
		{&rep.Summaries, `DELETE FROM summaries WHERE record_id IS NOT NULL AND record_id NOT IN ` + live},
		{&rep.Summaries, `DELETE FROM summaries WHERE change_id IS NOT NULL AND change_id NOT IN (SELECT id FROM file_changes)`},
		{&rep.Summaries, `DELETE FROM summaries WHERE asset_id IS NOT NULL AND asset_id NOT IN (SELECT id FROM assets)`},
		{&rep.ToolFTS, `DELETE FROM tool_fts WHERE record_id NOT IN ` + live},
		{&rep.ChunkVec, `DELETE FROM chunk_vec WHERE rowid IN (SELECT id FROM chunks WHERE record_id > 0 AND record_id NOT IN ` + live + `)`},
		{&rep.Chunks, `DELETE FROM chunks WHERE record_id > 0 AND record_id NOT IN ` + live},
		{&rep.ChunkVec, `DELETE FROM chunk_vec WHERE rowid NOT IN (SELECT id FROM chunks)`},
	} {
		if err := del(step.dst, step.q); err != nil {
			return rep, err
		}
	}
	for _, sid := range sids {
		if err := refreshSession(ctx, conn, sid); err != nil {
			return rep, err
		}
	}
	rep.Sessions = len(sids)
	fts, err := store.FTSTables(ctx, conn)
	if err != nil {
		return rep, err
	}
	for _, tbl := range fts {
		if err := store.FTSCommand(ctx, conn, tbl, "rebuild"); err != nil {
			return rep, err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return rep, err
	}
	committed = true
	return rep, nil
}
