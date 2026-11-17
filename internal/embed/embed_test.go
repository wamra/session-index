package embed

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/store"
)

type req struct {
	Model     string   `json:"model"`
	Input     []string `json:"input"`
	Truncate  bool     `json:"truncate"`
	KeepAlive string   `json:"keep_alive"`
}

func vec(x float32) []float32 {
	v := make([]float32, store.VecDim)
	v[0] = x
	return v
}

// fake returns a server; handler may override via bad (inputs containing "BAD" yield status).
func fake(t *testing.T, onReq func(r req, w http.ResponseWriter) bool) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, hr *http.Request) {
		if hr.URL.Path != "/api/embed" || hr.Method != "POST" {
			t.Errorf("bad request %s %s", hr.Method, hr.URL.Path)
		}
		var r req
		if err := json.NewDecoder(hr.Body).Decode(&r); err != nil {
			t.Error(err)
		}
		if onReq != nil && onReq(r, w) {
			return
		}
		out := make([][]float32, len(r.Input))
		for i := range out {
			out[i] = vec(float32(i + 1))
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	}))
	t.Cleanup(s.Close)
	return s
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	db := st.DB()
	if _, err := db.Exec(`INSERT INTO files(id, path) VALUES (1, 'f')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO records(id, file_id, line_no, byte_off, byte_len) VALUES (1,1,1,0,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions(session_id, project) VALUES ('s1','projA')`); err != nil {
		t.Fatal(err)
	}
	return st
}

func seed(t *testing.T, st *store.Store, texts ...string) {
	t.Helper()
	for i, tx := range texts {
		if _, err := st.DB().Exec(`INSERT INTO chunks(record_id, session_id, kind, role, ts, seq, text) VALUES (1,'s1','main','user','2026-01-02T03:04:05Z',?,?)`, i, tx); err != nil {
			t.Fatal(err)
		}
	}
}

func count(t *testing.T, st *store.Store, q string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEmbedSendsPrefixAndBatch(t *testing.T) {
	var got req
	s := fake(t, func(r req, w http.ResponseWriter) bool { got = r; return false })
	c := NewClient(s.URL, "embeddinggemma")
	long := strings.Repeat("x", 3000)
	v, err := c.Embed(context.Background(), []string{"a", long}, DocPrefix)
	if err != nil || len(v) != 2 {
		t.Fatalf("v=%d err=%v", len(v), err)
	}
	if got.Model != "embeddinggemma" || !got.Truncate || got.KeepAlive != "15m" {
		t.Fatalf("%+v", got)
	}
	if got.Input[0] != "title: none | text: a" || got.Input[1] != DocPrefix+strings.Repeat("x", 2000) {
		t.Fatalf("inputs %q", got.Input[0])
	}
	if _, err := c.Query(context.Background(), "q"); err != nil || got.Input[0] != "task: search result | query: q" || len(got.Input) != 1 {
		t.Fatalf("query %v %q", err, got.Input)
	}
	if NewClient("http://x", "m").HTTP.Timeout.Seconds() != 120 {
		t.Fatal("timeout")
	}
}

func TestVecBytes(t *testing.T) {
	b := VecBytes([]float32{1.5, -2})
	if len(b) != 8 || math.Float32frombits(binary.LittleEndian.Uint32(b)) != 1.5 || math.Float32frombits(binary.LittleEndian.Uint32(b[4:])) != -2 {
		t.Fatal(b)
	}
}

func TestDrainMarksEmbedded(t *testing.T) {
	st := newStore(t)
	seed(t, st, "one", "two", "three")
	s := fake(t, nil)
	c := NewClient(s.URL, "embeddinggemma")
	n, err := Drain(context.Background(), st, c, 128)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunk_vec`) != 3 || count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`) != 0 {
		t.Fatal("vectors/embedded_at")
	}
	var proj string
	var ts int64
	var l int
	if err := st.DB().QueryRow(`SELECT project, ts, length(embedding) FROM chunk_vec WHERE rowid=1`).Scan(&proj, &ts, &l); err != nil || proj != "projA" || ts != 1767323045 || l != 3072 {
		t.Fatalf("%v %q %d %d", err, proj, ts, l)
	}
	if p, _ := ChunksPending(context.Background(), st); p != 0 {
		t.Fatal(p)
	}
	if n, err := Drain(context.Background(), st, c, 128); err != nil || n != 0 {
		t.Fatalf("second n=%d err=%v", n, err)
	}
}

func TestDrainWhenDownReturnsErrOllamaDown(t *testing.T) {
	st := newStore(t)
	seed(t, st, "one", "two")
	s := fake(t, nil)
	s.Close()
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if n != 0 || !errors.Is(err, ErrOllamaDown) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if p, _ := ChunksPending(context.Background(), st); p != 2 {
		t.Fatalf("pending=%d", p)
	}
}

func TestDrain5xxParksNothing(t *testing.T) {
	st := newStore(t)
	seed(t, st, "one", "two")
	s := fake(t, func(r req, w http.ResponseWriter) bool { w.WriteHeader(503); return true })
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if n != 0 || !errors.Is(err, ErrOllamaDown) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`) != 2 {
		t.Fatal("parked")
	}
}

func TestDrainPoisonBisect(t *testing.T) {
	st := newStore(t)
	seed(t, st, "good1", "BAD", "good2")
	s := fake(t, func(r req, w http.ResponseWriter) bool {
		for _, in := range r.Input {
			if strings.Contains(in, "BAD") {
				w.WriteHeader(400)
				return true
			}
		}
		return false
	})
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at='failed' AND text='BAD'`) != 1 || count(t, st, `SELECT count(*) FROM chunk_vec`) != 2 {
		t.Fatal("poison not isolated")
	}
	if p, _ := ChunksPending(context.Background(), st); p != 0 {
		t.Fatal(p)
	}
}

func TestDrainWrongCountBisects(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a", "b")
	s := fake(t, func(r req, w http.ResponseWriter) bool {
		if len(r.Input) > 1 {
			json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vec(1)}})
			return true
		}
		return false
	})
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at='failed'`) != 0 || count(t, st, `SELECT count(*) FROM chunk_vec`) != 2 {
		t.Fatal("wrong-count batch mis-marked")
	}
}

func TestDrainFingerprintMismatch(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a")
	if _, err := st.DB().Exec(`UPDATE meta SET value='other' WHERE key='embedder_fingerprint'`); err != nil {
		t.Fatal(err)
	}
	var calls int32
	s := fake(t, func(r req, w http.ResponseWriter) bool { atomic.AddInt32(&calls, 1); return false })
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if n != 0 || !errors.Is(err, ErrFingerprintMismatch) || calls != 0 {
		t.Fatalf("n=%d err=%v calls=%d", n, err, calls)
	}
}

func TestDrainLocked(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s := fake(t, func(r req, w http.ResponseWriter) bool {
		close(entered)
		<-release
		return false
	})
	done := make(chan error, 1)
	go func() {
		_, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
		done <- err
	}()
	<-entered
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if n != 0 || !errors.Is(err, ErrLocked) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDrainByteCapBatching(t *testing.T) {
	st := newStore(t)
	var texts []string
	for i := 0; i < 600; i++ { // 600 * 2000 = 1.2 MB > 1 MB
		texts = append(texts, fmt.Sprintf("%04d", i)+strings.Repeat("y", 1996))
	}
	seed(t, st, texts...)
	var sizes []int
	s := fake(t, func(r req, w http.ResponseWriter) bool { sizes = append(sizes, len(r.Input)); return false })
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 1000)
	if err != nil || n != 600 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, sz := range sizes {
		if sz > 128 || sz*2000 > 1<<20 {
			t.Fatalf("batch of %d too large (%v)", sz, sizes)
		}
	}
	if sizes[0] != 128 {
		t.Fatalf("sizes %v", sizes)
	}
}

func TestByteCapBindsBeforeCount(t *testing.T) {
	old := maxBatchBytes
	maxBatchBytes = 5000
	defer func() { maxBatchBytes = old }()
	st := newStore(t)
	seed(t, st, strings.Repeat("z", 2000), strings.Repeat("z", 2000), strings.Repeat("z", 2000), "tail")
	rows, err := loadPending(context.Background(), st.DB(), 128)
	if err != nil || len(rows) != 2 {
		t.Fatal(len(rows), err)
	}
}

func TestDrainReembedDeletesExistingVec(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a")
	if _, err := st.DB().Exec(`INSERT INTO chunk_vec(rowid, embedding, session_id, project, kind, role, ts) VALUES (1, ?, 's1','p','main','user',0)`, VecBytes(vec(9))); err != nil {
		t.Fatal(err)
	}
	s := fake(t, nil)
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunk_vec`) != 1 {
		t.Fatal("dup")
	}
	var proj string
	if err := st.DB().QueryRow(`SELECT project FROM chunk_vec WHERE rowid=1`).Scan(&proj); err != nil || proj != "projA" {
		t.Fatal(err, proj)
	}
}

func badHandler(r req, w http.ResponseWriter) bool {
	for _, in := range r.Input {
		if strings.Contains(in, "BAD") {
			w.WriteHeader(400)
			return true
		}
	}
	return false
}

func TestDrainPoisonAtEnds(t *testing.T) {
	for _, texts := range [][]string{{"BAD", "g1", "g2"}, {"g1", "g2", "BAD"}} {
		st := newStore(t)
		seed(t, st, texts...)
		s := fake(t, badHandler)
		n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
		if err != nil || n != 2 {
			t.Fatalf("%v: n=%d err=%v", texts, n, err)
		}
		if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at='failed' AND text='BAD'`) != 1 || count(t, st, `SELECT count(*) FROM chunk_vec`) != 2 {
			t.Fatalf("%v: not isolated", texts)
		}
	}
}

func TestDrainSystemicStatusesParkNothing(t *testing.T) {
	for _, code := range []int{404, 408, 429} {
		st := newStore(t)
		seed(t, st, "a", "b")
		s := fake(t, func(r req, w http.ResponseWriter) bool { w.WriteHeader(code); return true })
		n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
		if n != 0 || !errors.Is(err, ErrOllamaDown) {
			t.Fatalf("%d: n=%d err=%v", code, n, err)
		}
		if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`) != 2 {
			t.Fatalf("%d: parked", code)
		}
	}
}

func TestDrainWrongDimensionIsBroken(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a", "b")
	s := fake(t, func(r req, w http.ResponseWriter) bool {
		out := make([][]float32, len(r.Input))
		for i := range out {
			out[i] = []float32{1, 2, 3}
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
		return true
	})
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if n != 0 || !errors.Is(err, ErrEmbedderBroken) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`) != 2 || count(t, st, `SELECT count(*) FROM chunk_vec`) != 0 {
		t.Fatal("rows marked")
	}
}

func TestDrainAllRowsFailIsBroken(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a", "b", "c")
	s := fake(t, func(r req, w http.ResponseWriter) bool { w.WriteHeader(400); return true })
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if n != 0 || !errors.Is(err, ErrEmbedderBroken) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`) != 3 {
		t.Fatal("rows marked")
	}
}

func TestDrainCancelAfterFirstBatch(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a", "b", "c", "d")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	var calls int32
	s := fake(t, func(r req, w http.ResponseWriter) bool {
		if atomic.AddInt32(&calls, 1) == 2 {
			cancel()
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			return true
		}
		return false
	})
	n, err := Drain(ctx, st, NewClient(s.URL, "embeddinggemma"), 2)
	if n != 2 || !errors.Is(err, context.Canceled) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunk_vec`) != 2 || count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`) != 2 {
		t.Fatal("state")
	}
	// already-cancelled context: loop never runs, still reports ctx error
	n, err = Drain(ctx, st, NewClient(s.URL, "embeddinggemma"), 2)
	if n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestDrainReleasesLockAfterError(t *testing.T) {
	st := newStore(t)
	seed(t, st, "a")
	s := fake(t, nil)
	s.Close()
	if _, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128); !errors.Is(err, ErrOllamaDown) {
		t.Fatal(err)
	}
	if _, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128); errors.Is(err, ErrLocked) || !errors.Is(err, ErrOllamaDown) {
		t.Fatalf("second drain: %v", err)
	}
}

func TestDrainLonePoisonRowIsParked(t *testing.T) {
	st := newStore(t)
	seed(t, st, "only")
	s := fake(t, func(r req, w http.ResponseWriter) bool { w.WriteHeader(400); return true })
	n, err := Drain(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128)
	if n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM chunks WHERE embedded_at='failed'`) != 1 || count(t, st, `SELECT count(*) FROM chunk_vec`) != 0 {
		t.Fatal("lone row not parked")
	}
}

func TestDrainCappedStopsAndPersistsAcrossRuns(t *testing.T) {
	st := newStore(t)
	seed(t, st, "one", "two", "three", "four", "five")
	s := fake(t, nil)
	c := NewClient(s.URL, "embeddinggemma")
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	n, err := DrainCapped(context.Background(), st, c, 128, 3, clock)
	var capErr *ErrDailyCap
	if n != 3 || !errors.As(err, &capErr) || capErr.Used != 3 || capErr.Cap != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if p, _ := ChunksPending(context.Background(), st); p != 2 {
		t.Fatalf("pending=%d", p)
	}
	if n, err := DrainCapped(context.Background(), st, c, 128, 3, clock); n != 0 || !errors.As(err, &capErr) {
		t.Fatalf("same day: n=%d err=%v", n, err)
	}
	now = now.Add(24 * time.Hour)
	if n, err := DrainCapped(context.Background(), st, c, 128, 3, clock); n != 2 || err != nil {
		t.Fatalf("next day: n=%d err=%v", n, err)
	}
	// uncapped Drain is unchanged and counts too
	if used, _ := store.UsageToday(context.Background(), st.DB(), store.UsageEmbed, now); used != 2 {
		t.Fatalf("usage %d", used)
	}
}

func TestDrainCountsDispatchedChunksWhenProviderIsDown(t *testing.T) {
	st := newStore(t)
	seed(t, st, "one", "two", "three")
	s := fake(t, func(r req, w http.ResponseWriter) bool { w.WriteHeader(503); return true })
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	_, err := DrainCapped(context.Background(), st, NewClient(s.URL, "embeddinggemma"), 128, 100, func() time.Time { return now })
	if !errors.Is(err, ErrOllamaDown) {
		t.Fatal(err)
	}
	if used, _ := store.UsageToday(context.Background(), st.DB(), store.UsageEmbed, now); used != 3 {
		t.Fatalf("usage %d", used)
	}
	if p, _ := ChunksPending(context.Background(), st); p != 3 {
		t.Fatalf("pending %d", p)
	}
}
