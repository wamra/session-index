package ingest

import (
	"context"
	"github.com/wamra/session-index/internal/store"
	"strconv"
	"testing"
)

func TestRepairRemovesOrphansAndRebuildsFTS(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText)+asstLine(2, longText), false)
	if _, err := File(context.Background(), st, root, p, ""); err != nil {
		t.Fatal(err)
	}
	liveMsgs := count(t, st, `SELECT count(*) FROM messages`)
	liveChunks := count(t, st, `SELECT count(*) FROM chunks`)
	if liveMsgs != 2 || liveChunks != 2 {
		t.Fatalf("setup msgs=%d chunks=%d", liveMsgs, liveChunks)
	}
	db := st.DB()
	// Orphans: supersede the second record without cleaning its derived rows,
	// plus a tool_fts row pointing at a record that does not exist.
	vec := make([]byte, 768*4)
	var cid, rid int64
	if err := db.QueryRow(`SELECT c.id, c.record_id FROM chunks c JOIN records r ON r.id=c.record_id WHERE r.type='assistant'`).Scan(&cid, &rid); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE records SET superseded=1 WHERE id=` + itoa(rid),
		`INSERT INTO tool_fts(text, record_id) VALUES ('ghost tool text', 999999)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO chunk_vec(rowid, embedding, session_id, project, kind, role, ts) VALUES (?,?,?,?,?,?,?)`,
		cid, vec, sid, "proj", "main", "assistant", 1000); err != nil {
		t.Fatal(err)
	}
	rep, err := Repair(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Messages != 1 || rep.Chunks != 1 || rep.ChunkVec != 1 || rep.ToolFTS < 2 || rep.Sessions != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if n := count(t, st, `SELECT count(*) FROM messages m LEFT JOIN records r ON r.id=m.record_id WHERE r.id IS NULL OR r.superseded=1`); n != 0 {
		t.Fatalf("orphan messages left: %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks`); n != 1 {
		t.Fatalf("chunks = %d", n)
	}
	if n := count(t, st, `SELECT message_count FROM sessions WHERE session_id=?`, sid); n != 1 {
		t.Fatalf("message_count = %d, want 1", n)
	}
	fts, err := store.FTSTables(context.Background(), db)
	if err != nil || len(fts) < 4 {
		t.Fatalf("fts tables %v %v", fts, err)
	}
	for _, tbl := range fts {
		if err := store.FTSCommand(context.Background(), db, tbl, "integrity-check"); err != nil {
			t.Fatalf("after repair: %v", err)
		}
	}
	// Idempotent.
	rep, err = Repair(context.Background(), st)
	if err != nil || rep != (RepairReport{}) {
		t.Fatalf("second repair %+v %v", rep, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
