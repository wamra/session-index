package store

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/session-index/internal/provider"
)

func vec(vals ...float32) []byte {
	b := make([]byte, VecDim*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

func TestOpenCreatesSchemaAndVec(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()

	var n int
	if err := db.QueryRow(`select count(*) from sqlite_master where name in
		('files','records','messages','chunks','chunk_vec','chunk_fts','tool_fts','sessions','meta')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Fatalf("tables = %d, want 9", n)
	}

	var idx int
	if err := db.QueryRow(`select count(*) from sqlite_master where type='index' and name='chunks_pending'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("chunks_pending index missing: %d %v", idx, err)
	}

	var ver string
	if err := db.QueryRow(`select vec_version()`).Scan(&ver); err != nil || ver == "" {
		t.Fatalf("vec_version %q %v", ver, err)
	}

	ins := `insert into chunk_vec(rowid, embedding, session_id, project, kind, role, ts) values (?,?,?,?,?,?,?)`
	vs := [][]byte{vec(1, 0), vec(0, 1), vec(0.9, 0.1)}
	kinds := []string{"main", "main", "sub"}
	for i, v := range vs {
		if _, err := db.Exec(ins, i+1, v, "s", "p", kinds[i], "user", 1000+i); err != nil {
			t.Fatal(err)
		}
	}
	knn := func(extra string) []int {
		rows, err := db.Query(`select rowid from chunk_vec where embedding match ? and k = 2 `+extra+` order by distance`, vec(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int
		for rows.Next() {
			var id int
			rows.Scan(&id)
			ids = append(ids, id)
		}
		return ids
	}
	if got := knn(""); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("knn = %v, want [1 3]", got)
	}
	if got := knn("and kind = 'main'"); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("filtered knn = %v, want [1 2]", got)
	}

	if _, err := db.Exec(`insert into chunk_fts(rowid,text) values (1,'xxabcxx')`); err != nil {
		t.Fatal(err)
	}
	var rid int
	if err := db.QueryRow(`select rowid from chunk_fts where chunk_fts match 'abc'`).Scan(&rid); err != nil || rid != 1 {
		t.Fatalf("fts rid=%d err=%v", rid, err)
	}

	for k, want := range map[string]string{"schema_version": "1", "embedder_fingerprint": "ollama|embeddinggemma|768|title: none | text: |v1", "vec_dims": "768"} {
		var v string
		if err := db.QueryRow(`select value from meta where key=?`, k).Scan(&v); err != nil || v != want {
			t.Fatalf("meta %s = %q %v", k, v, err)
		}
	}
	var jm string
	db.QueryRow(`pragma journal_mode`).Scan(&jm)
	if jm != strings.ToLower(JournalMode) {
		t.Fatalf("journal_mode %s, want %s", jm, JournalMode)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate not idempotent: %v", err)
	}
}

func TestMigrateAddsProfileColumnsToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db := s.DB()
	// Recreate the pre-profile schema for the two tables and add old rows.
	for _, q := range []string{
		`ALTER TABLE files DROP COLUMN profile`,
		`ALTER TABLE sessions DROP COLUMN profile`,
		`INSERT INTO files(path, session_id) VALUES ('/x/p/s1.jsonl','s1')`,
		`INSERT INTO sessions(session_id, project) VALUES ('s1','p')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, tb := range []string{"files", "sessions"} {
		if ok, _ := HasColumn(db, tb, "profile"); ok {
			t.Fatalf("%s still has profile", tb)
		}
	}
	s.Close()

	s, err = OpenProfile(path, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tb := range []string{"files", "sessions"} {
		if ok, err := HasColumn(s.DB(), tb, "profile"); err != nil || !ok {
			t.Fatalf("%s missing profile after migrate: %v", tb, err)
		}
		var p string
		if err := s.DB().QueryRow(`SELECT profile FROM ` + tb).Scan(&p); err != nil || p != "desktop" {
			t.Fatalf("%s backfill = %q, %v", tb, p, err)
		}
	}
	// Idempotent.
	if err := Migrate(s.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointLeavesEmptyWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`INSERT INTO meta(key,value) VALUES('k','v')`); err != nil {
		t.Fatal(err)
	}
	if JournalMode != "WAL" {
		// No WAL (not the case on any platform today): Checkpoint must still succeed.
		if err := s.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("expected non-empty -wal before checkpoint: %v %v", fi, err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("-wal size = %d, want 0", fi.Size())
	}
}

func TestOpenReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.DB().Exec(`INSERT INTO meta(key,value) VALUES('k','v')`)
	s.Checkpoint()
	s.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var v string
	if err := ro.DB().QueryRow(`SELECT value FROM meta WHERE key='k'`).Scan(&v); err != nil || v != "v" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := ro.DB().Exec(`INSERT INTO meta(key,value) VALUES('z','z')`); err == nil {
		t.Fatal("write on read-only store succeeded")
	}
	bad := filepath.Join(t.TempDir(), "bad.sqlite")
	os.WriteFile(bad, []byte("not a database at all, just text padding padding padding padding padding"), 0o644)
	if _, err := OpenReadOnly(bad); err == nil {
		t.Fatal("malformed file opened")
	}
}

func metaVal(t *testing.T, db *sql.DB, k string) string {
	t.Helper()
	var v string
	if err := db.QueryRow(`select value from meta where key=?`, k).Scan(&v); err != nil {
		t.Fatalf("meta %s: %v", k, err)
	}
	return v
}

func TestNewArchiveUsesConfiguredDims(t *testing.T) {
	st, err := OpenWith(filepath.Join(t.TempDir(), "a.db"), Opts{Dims: 4, Fingerprint: "openai|m|4||v1"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if metaVal(t, st.DB(), "vec_dims") != "4" || metaVal(t, st.DB(), "embedder_fingerprint") != "openai|m|4||v1" {
		t.Fatal("meta not set from options")
	}
	if _, err := st.DB().Exec(`insert into chunk_vec(rowid, embedding, session_id, project, kind, role, ts) values (1, ?, 's', 'p', 'main', 'user', 1)`, make([]byte, 4*4)); err != nil {
		t.Fatalf("4-dim vector rejected: %v", err)
	}
	if _, err := st.DB().Exec(`insert into chunk_vec(rowid, embedding, session_id, project, kind, role, ts) values (2, ?, 's', 'p', 'main', 'user', 1)`, make([]byte, 4*768)); err == nil {
		t.Fatal("768-dim vector accepted by a 4-dim table")
	}
}

func TestDimsMismatchIsAnErrorNamingBothValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	st, err := OpenWith(path, Opts{Dims: 4})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	_, err = OpenWith(path, Opts{Dims: 8})
	var me *DimsMismatchError
	if !errors.As(err, &me) || me.Stored != 4 || me.Config != 8 {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"4", "8", "rebuild-derived"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q lacks %q", err, want)
		}
	}
	for name, o := range map[string]Opts{"same": {Dims: 4}, "unknown": {}, "allow": {Dims: 8, AllowDimsChange: true}} {
		s, err := OpenWith(path, o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if metaVal(t, s.DB(), "vec_dims") != "4" {
			t.Fatalf("%s: stored dims changed", name)
		}
		s.Close()
	}
}

func TestLegacyArchiveGetsDimsAndFingerprintRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// make it look like an archive from before providers existed
	if _, err := st.DB().Exec(`delete from meta where key='vec_dims'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`update meta set value=? where key='embedder_fingerprint'`, provider.LegacyFingerprint); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := metaVal(t, st.DB(), "embedder_fingerprint"); got != "ollama|embeddinggemma|768|title: none | text: |v1" {
		t.Fatalf("fingerprint %q", got)
	}
	if got := metaVal(t, st.DB(), "vec_dims"); got != "768" {
		t.Fatalf("vec_dims %q (read from the existing chunk_vec)", got)
	}
}

func TestEffectiveVecDimsFallsBackToDDLOnReadOnlyLegacyArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.db")
	st, err := OpenWith(path, Opts{Dims: 12})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`delete from meta where key='vec_dims'`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if n, _ := VecDims(ro.DB()); n != 0 {
		t.Fatalf("VecDims = %d", n)
	}
	if n, err := EffectiveVecDims(ro.DB()); err != nil || n != 12 {
		t.Fatalf("EffectiveVecDims = %d %v", n, err)
	}
}
