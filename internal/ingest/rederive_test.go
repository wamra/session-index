package ingest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wamra/session-index/internal/store"
)

func TestRederiveRebuildsDerivedRows(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText)+asstLine(2, longText+"\n\nshort\n\nAnother paragraph that is long enough to keep.")+"\n", false)
	sub := filepath.Join(root, "proj", sid, "subagents", "agent-a1.jsonl")
	write(t, sub, userLine(3, longText), false)
	ctx := context.Background()
	if _, err := Sweep(ctx, st, root, ""); err != nil {
		t.Fatal(err)
	}
	q := map[string]string{
		"chunks":   `SELECT count(*) FROM chunks`,
		"messages": `SELECT count(*) FROM messages`,
		"sessions": `SELECT count(*) FROM sessions`,
		"tool":     `SELECT count(*) FROM tool_fts WHERE tool_fts MATCH '"zebrafish"'`,
		"fts":      `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH '"sufficiently"'`,
		"records":  `SELECT count(*) FROM records`,
	}
	want := map[string]int{}
	for k, s := range q {
		want[k] = count(t, st, s)
	}
	if want["chunks"] == 0 || want["messages"] == 0 || want["sessions"] == 0 {
		t.Fatalf("fixture empty: %v", want)
	}
	var proj, cwd, kind string
	st.DB().QueryRow(`SELECT project, cwd, kind FROM sessions`).Scan(&proj, &cwd, &kind)

	for _, s := range []string{`DELETE FROM messages`, `DELETE FROM chunks`, `DELETE FROM chunk_vec`,
		`INSERT INTO chunk_fts(chunk_fts) VALUES('delete-all')`, `DELETE FROM tool_fts`, `DELETE FROM sessions`} {
		if _, err := st.DB().Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Rederive(ctx, st, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != want["records"] {
		t.Fatalf("records=%d want %d", n, want["records"])
	}
	for k, s := range q {
		if got := count(t, st, s); got != want[k] {
			t.Errorf("%s: got %d want %d", k, got, want[k])
		}
	}
	var proj2, cwd2, kind2 string
	st.DB().QueryRow(`SELECT project, cwd, kind FROM sessions`).Scan(&proj2, &cwd2, &kind2)
	if proj2 != proj || cwd2 != cwd || kind2 != kind || proj2 != "proj" {
		t.Fatalf("session %q %q %q vs %q %q %q", proj2, cwd2, kind2, proj, cwd, kind)
	}
}

func TestRederiveDimsRecreatesChunkVec(t *testing.T) {
	st, err := store.OpenWith(filepath.Join(t.TempDir(), "a.db"), store.Opts{Dims: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.DB()
	if _, err := db.Exec(`INSERT INTO chunk_vec(rowid, embedding, session_id, project, kind, role, ts) VALUES (1, ?, 's', 'p', 'main', 'user', 1)`, make([]byte, 4*4)); err != nil {
		t.Fatal(err)
	}
	if _, err := RederiveDims(context.Background(), st, "openai|m|6||v1", 6); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM chunk_vec`); n != 0 {
		t.Fatalf("chunk_vec kept %d rows", n)
	}
	if d, _ := store.VecDims(db); d != 6 {
		t.Fatalf("vec_dims = %d", d)
	}
	var fp string
	db.QueryRow(`SELECT value FROM meta WHERE key='embedder_fingerprint'`).Scan(&fp)
	if fp != "openai|m|6||v1" {
		t.Fatalf("fingerprint %q", fp)
	}
	if _, err := db.Exec(`INSERT INTO chunk_vec(rowid, embedding, session_id, project, kind, role, ts) VALUES (1, ?, 's', 'p', 'main', 'user', 1)`, make([]byte, 4*6)); err != nil {
		t.Fatalf("6-dim insert after rebuild: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO chunk_vec(rowid, embedding, session_id, project, kind, role, ts) VALUES (2, ?, 's', 'p', 'main', 'user', 1)`, make([]byte, 4*4)); err == nil {
		t.Fatal("old 4-dim insert still accepted")
	}
	// the archive reopens with the new size and rejects the old one
	path := st.Path()
	if s2, err := store.OpenWith(path, store.Opts{Dims: 6}); err != nil {
		t.Fatal(err)
	} else {
		s2.Close()
	}
	if _, err := store.OpenWith(path, store.Opts{Dims: 4}); err == nil {
		t.Fatal("old dims accepted after rebuild")
	}
}
