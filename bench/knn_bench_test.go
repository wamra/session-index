// Package bench holds the SQLite driver micro-benchmark used for the
// driver decision recorded in docs/REFERENCE.md.
package bench

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/store"
)

// dataEnv names the file of benchmark embeddings (raw little-endian float32,
// store.VecDim values per row). The test is skipped when it is unset.
const dataEnv = "SESSION_INDEX_BENCH_VECS"

// TestKNNLatency inserts the benchmark embeddings and times 100 KNN k=50 queries.
func TestKNNLatency(t *testing.T) {
	dataPath := os.Getenv(dataEnv)
	if dataPath == "" {
		t.Skipf("%s not set", dataEnv)
	}
	raw, err := os.ReadFile(dataPath)
	if err != nil {
		t.Skipf("benchmark data not present: %v", err)
	}
	const dim = store.VecDim
	rowBytes := dim * 4
	n := len(raw) / rowBytes
	if n*rowBytes != len(raw) {
		t.Fatalf("data size %d not a multiple of %d", len(raw), rowBytes)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "bench.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()

	t0 := time.Now()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`insert into chunk_vec(rowid, embedding, session_id, project, kind, role, ts) values (?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		kind := "main"
		if i%5 == 0 {
			kind = "sub"
		}
		if _, err := stmt.Exec(i+1, raw[i*rowBytes:(i+1)*rowBytes], "s", "p", kind, "user", i); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Logf("inserted %d vectors in %v", n, time.Since(t0))

	var durs []time.Duration
	for q := 0; q < 100; q++ {
		i := (q * 151) % n
		start := time.Now()
		rows, err := db.Query(`select rowid, distance from chunk_vec where embedding match ? and k = 50 order by distance`,
			raw[i*rowBytes:(i+1)*rowBytes])
		if err != nil {
			t.Fatal(err)
		}
		cnt := 0
		for rows.Next() {
			var id int64
			var d float64
			rows.Scan(&id, &d)
			cnt++
		}
		rows.Close()
		durs = append(durs, time.Since(start))
		if cnt != 50 {
			t.Fatalf("got %d rows", cnt)
		}
	}
	sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
	t.Logf("KNN k=50 over %d vectors: p50=%v p95=%v max=%v", n, durs[49], durs[94], durs[99])
}
