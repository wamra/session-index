package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

// ftsFixture creates a plain trigram FTS5 table like tool_fts/asset_fts/file_fts
// (text indexed, owner UNINDEXED) with n rows spread over n/per owners.
func ftsFixture(t *testing.T, n, per int) *sql.DB {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	db := s.DB()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE exp_fts USING fts5(text, owner UNINDEXED, tokenize='trigram')`); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	words := strings.Fields("alpha beta gamma delta error warning panic stack trace file path config session archive summary output tool result")
	tx, _ := db.Begin()
	for i := 0; i < n; i++ {
		var sb strings.Builder
		for j := 0; j < 200; j++ {
			sb.WriteString(words[rng.Intn(len(words))])
			sb.WriteString(fmt.Sprint(rng.Intn(1000)))
			sb.WriteByte(' ')
		}
		if _, err := tx.Exec(`INSERT INTO exp_fts(text, owner) VALUES(?,?)`, sb.String(), i/per); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

func ftsCheck(db *sql.DB) error {
	_, err := db.Exec(`INSERT INTO exp_fts(exp_fts) VALUES('integrity-check')`)
	return err
}

// TestFTSDeleteVariants: bulk deletes from a plain trigram FTS5 table (by an
// UNINDEXED column, by rowid, everything, followed by optimize/automerge)
// leave integrity-check and PRAGMA integrity_check clean on a single
// connection: no single-connection corruption could be reproduced.
func TestFTSDeleteVariants(t *testing.T) {
	cases := []struct {
		name string
		del  func(db *sql.DB) error
	}{
		{"delete-by-unindexed-column", func(db *sql.DB) error { _, err := db.Exec(`DELETE FROM exp_fts WHERE owner % 3 = 0`); return err }},
		{"delete-by-unindexed-in-subquery", func(db *sql.DB) error {
			_, err := db.Exec(`DELETE FROM exp_fts WHERE owner IN (SELECT 0 UNION SELECT 3 UNION SELECT 6 UNION SELECT 9 UNION SELECT 12)`)
			return err
		}},
		{"delete-by-rowid-subquery", func(db *sql.DB) error {
			_, err := db.Exec(`DELETE FROM exp_fts WHERE rowid IN (SELECT rowid FROM exp_fts WHERE owner % 3 = 0)`)
			return err
		}},
		{"delete-all-rows-unconditional", func(db *sql.DB) error { _, err := db.Exec(`DELETE FROM exp_fts`); return err }},
		{"delete-by-unindexed-then-optimize", func(db *sql.DB) error {
			if _, err := db.Exec(`DELETE FROM exp_fts WHERE owner % 3 = 0`); err != nil {
				return err
			}
			_, err := db.Exec(`INSERT INTO exp_fts(exp_fts, rank) VALUES('optimize', 0)`)
			return err
		}},
		{"delete-by-unindexed-then-automerge", func(db *sql.DB) error {
			if _, err := db.Exec(`DELETE FROM exp_fts WHERE owner % 3 = 0`); err != nil {
				return err
			}
			_, err := db.Exec(`INSERT INTO exp_fts(exp_fts, rank) VALUES('automerge', 0)`)
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := ftsFixture(t, 1500, 10)
			if err := ftsCheck(db); err != nil {
				t.Fatalf("before delete: %v", err)
			}
			if err := c.del(db); err != nil {
				t.Fatal(err)
			}
			if err := ftsCheck(db); err != nil {
				t.Fatalf("integrity-check after %s: %v", c.name, err)
			}
			var res string
			if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
				t.Fatalf("PRAGMA integrity_check after %s: %q %v", c.name, res, err)
			}
		})
	}
}

// TestFTSUpsertPatternIntegrity mimics asset upserts (DELETE WHERE owner=?,
// then INSERT of a large text) in many small transactions while another
// connection runs PRAGMA integrity_check. Finding (ncruces WASM build): the
// data is never corrupt, but a long-lived second connection reports
// "malformed inverted index" (a stale FTS5 view) until an FTS command
// refreshes it, whereas a fresh connection never does. The test asserts the
// facts doctor relies on: fresh connections are never flagged, and every
// connection is ok once the writer is done and the FTS command ran.
func TestFTSUpsertPatternIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.sqlite")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.DB().Exec(`CREATE VIRTUAL TABLE exp_fts USING fts5(text, owner UNINDEXED, tokenize='trigram')`); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pragma := func(db *sql.DB) string {
		var res string
		if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&res); err != nil {
			return err.Error()
		}
		return res
	}
	stop := make(chan struct{})
	type tally struct{ longRuns, longBad, freshRuns, freshBad int }
	done := make(chan tally)
	go func() {
		var c tally
		defer func() { done <- c }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.longRuns++
			if pragma(r.DB()) != "ok" {
				c.longBad++
			}
			fs, err := Open(path)
			if err != nil {
				continue
			}
			c.freshRuns++
			if res := pragma(fs.DB()); res != "ok" {
				c.freshBad++
			}
			fs.Close()
		}
	}()
	rng := rand.New(rand.NewSource(3))
	big := strings.Repeat("The quick brown fox jumps over the lazy dog 0123456789. ", 400)
	for i := 0; i < 600; i++ {
		id := rng.Intn(80)
		tx, _ := w.DB().Begin()
		tx.Exec(`DELETE FROM exp_fts WHERE owner=?`, id)
		if _, err := tx.Exec(`INSERT INTO exp_fts(text, owner) VALUES(?,?)`, fmt.Sprintf("%s %d %d", big, i, rng.Int()), id); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	}
	close(stop)
	c := <-done
	t.Logf("long-lived connection: %d/%d PRAGMA integrity_check runs flagged; fresh connections: %d/%d flagged", c.longBad, c.longRuns, c.freshBad, c.freshRuns)
	if c.longBad == 0 {
		t.Skip("this environment never flagged a stale FTS5 view on the long-lived connection; the false positive could not be observed, so the assertions below would pass vacuously")
	}
	if c.freshBad != 0 {
		t.Fatalf("fresh connections flagged %d times", c.freshBad)
	}
	if _, err := r.DB().Exec(`INSERT INTO exp_fts(exp_fts) VALUES('integrity-check')`); err != nil {
		t.Fatal(err)
	}
	for name, db := range map[string]*sql.DB{"writer": w.DB(), "reader": r.DB()} {
		if res := pragma(db); res != "ok" {
			t.Fatalf("%s connection after the writer finished: %q", name, res)
		}
	}
}

func TestFTSTablesListsEveryFTS5Table(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "a.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`CREATE VIRTUAL TABLE future_fts USING fts5(x)`); err != nil {
		t.Fatal(err)
	}
	got, err := FTSTables(context.Background(), s.DB())
	if err != nil {
		t.Fatal(err)
	}
	want := "asset_fts chunk_fts file_fts future_fts tool_fts"
	if strings.Join(got, " ") != want {
		t.Fatalf("FTSTables = %v, want %s", got, want)
	}
}
