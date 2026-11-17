package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/session-index/internal/store"
)

func newFTSArchive(t *testing.T) (string, *store.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.sqlite")
	w, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if _, err := w.DB().Exec(`CREATE VIRTUAL TABLE exp_fts USING fts5(text, owner UNINDEXED, tokenize='trigram')`); err != nil {
		t.Fatal(err)
	}
	return path, w
}

// The first pass fails (a query_only connection cannot run the FTS5
// integrity-check INSERT); the fresh pool agrees the archive is fine, so
// doctor must not report FAILED.
func TestDoctorFTSRecheckOnFreshPoolOverridesFirstPass(t *testing.T) {
	path, _ := newFTSArchive(t)
	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	reopened := 0
	reopen := func() (*sql.DB, func(), error) {
		reopened++
		st, err := store.Open(path)
		if err != nil {
			return nil, nil, err
		}
		return st.DB(), func() { st.Close() }, nil
	}
	var out bytes.Buffer
	if !doctorFTS(context.Background(), ro.DB(), reopen, true, &out) || reopened != 1 || strings.Contains(out.String(), "FAILED") {
		t.Fatalf("reopened %d\n%s", reopened, out.String())
	}
	if !strings.Contains(out.String(), "exp_fts integrity-check: ok") || !strings.Contains(out.String(), "chunk_fts integrity-check: ok") || !strings.Contains(out.String(), "integrity_check: ok") {
		t.Fatalf("%s", out.String())
	}
	// healthy first pass: no reopen
	reopened = 0
	rw, _ := store.Open(path)
	defer rw.Close()
	out.Reset()
	if !doctorFTS(context.Background(), rw.DB(), reopen, true, &out) || reopened != 0 {
		t.Fatalf("reopened %d\n%s", reopened, out.String())
	}
	// still failing on the fresh pool: reported as FAILED
	out.Reset()
	bad := func() (*sql.DB, func(), error) { return ro.DB(), func() {}, nil }
	if doctorFTS(context.Background(), ro.DB(), bad, true, &out) || !strings.Contains(out.String(), "FAILED") {
		t.Fatalf("%s", out.String())
	}
}

// A long-lived connection flagged stale by a concurrent writer (the FTS
// reproduction setup): doctorFTS never reports FAILED.
func TestDoctorFTSWithLongLivedConnectionAndConcurrentWriter(t *testing.T) {
	path, w := newFTSArchive(t)
	long, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer long.Close()
	reopen := func() (*sql.DB, func(), error) {
		st, err := store.Open(path)
		if err != nil {
			return nil, nil, err
		}
		return st.DB(), func() { st.Close() }, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		big := strings.Repeat("The quick brown fox jumps over the lazy dog 0123456789. ", 400)
		for i := 0; i < 300; i++ {
			tx, _ := w.DB().Begin()
			tx.Exec(`DELETE FROM exp_fts WHERE owner=?`, i%40)
			tx.Exec(`INSERT INTO exp_fts(text, owner) VALUES(?,?)`, fmt.Sprintf("%s %d", big, i), i%40)
			tx.Commit()
		}
	}()
	runs, flagged := 0, 0
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		var res string
		if long.DB().QueryRow(`PRAGMA integrity_check`).Scan(&res) == nil && res != "ok" {
			flagged++
		}
		var out bytes.Buffer
		if !doctorFTS(context.Background(), long.DB(), reopen, true, &out) {
			t.Fatalf("doctorFTS reported a problem during concurrent writes:\n%s", out.String())
		}
		runs++
	}
	t.Logf("%d doctorFTS runs, long-lived connection flagged stale %d times by raw PRAGMA", runs, flagged)
	if flagged == 0 {
		t.Skip("the long-lived connection was never flagged stale here, so the run proved nothing")
	}
}
