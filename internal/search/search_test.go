package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/store"
)

func TestRouterOnLabeledCategories(t *testing.T) {
	qpath := os.Getenv("SESSION_INDEX_LABELED_QUERIES")
	if qpath == "" {
		t.Skip("SESSION_INDEX_LABELED_QUERIES not set")
	}
	b, err := os.ReadFile(qpath)
	if err != nil {
		t.Skip("labeled queries not present")
	}
	var qs []struct{ Q, Cat string }
	if err := json.Unmarshal(b, &qs); err != nil {
		t.Fatal(err)
	}
	idTotal, idHit, otherTotal, otherHit := 0, 0, 0, 0
	for _, q := range qs {
		got := LooksLikeIdentifier(q.Q)
		if q.Cat == "identifier" {
			idTotal++
			if got {
				idHit++
			}
		} else {
			otherTotal++
			if got {
				otherHit++
				t.Logf("false positive (%s): %q", q.Cat, q.Q)
			}
		}
	}
	t.Logf("identifier %d/%d, others-as-identifier %d/%d", idHit, idTotal, otherHit, otherTotal)
	if idTotal != 80 || otherTotal != 478 {
		t.Fatalf("unexpected category sizes %d/%d", idTotal, otherTotal)
	}
	if idHit < 78 {
		t.Errorf("identifier recall %d/80 < 78", idHit)
	}
	if otherHit != 0 {
		t.Errorf("%d/478 non-identifiers routed to fts", otherHit)
	}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFtsQueryEscapesQuotesAndSyntax(t *testing.T) {
	st := newStore(t)
	db := st.DB()
	if _, err := db.Exec(`INSERT INTO chunk_fts(rowid,text) VALUES (1,'say hi x y there')`); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{`say "hi" -x:y*`, `"`, `a" OR "b`, `NEAR(a b) AND NOT c`, `foo-bar:baz* ^qux`, `""`, ``} {
		for _, q := range ftsQuery(in) {
			rows, err := db.Query(`SELECT rowid FROM chunk_fts WHERE chunk_fts MATCH ?`, q)
			if err != nil {
				t.Fatalf("input %q query %q: %v", in, q, err)
			}
			rows.Close()
		}
	}
	got := ftsQuery(`say "hi" -x:y*`)
	if len(got) == 0 || got[0] != `"say ""hi"" -x:y*"` {
		t.Fatalf("cascade = %v", got)
	}
}

func TestRRFOrder(t *testing.T) {
	got := rrf([]int64{1, 2, 3}, []int64{3, 2, 4}, 60)
	// 3: 1/63+1/61 (.03227) > 2: 2/62 (.03226) > 1: 1/61 > 4: 1/63
	want := []int64{3, 2, 1, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := rrf([]int64{5, 6}, nil, 60); !reflect.DeepEqual(got, []int64{5, 6}) {
		t.Fatalf("single leg %v", got)
	}
}

func vec(i int) []float32 {
	v := make([]float32, store.VecDim)
	v[i] = 1
	return v
}

func fakeOllama(t *testing.T, v []float32) *embed.Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{v}})
	}))
	t.Cleanup(s.Close)
	return embed.NewClient(s.URL, "m")
}

func deadOllama(t *testing.T) *embed.Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := s.URL
	s.Close()
	return embed.NewClient(url, "m")
}

func seedSession(t *testing.T, st *store.Store, sid, project, title, custom string) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO sessions(session_id, project, title, custom_title) VALUES (?,?,?,?)`, sid, project, title, custom); err != nil {
		t.Fatal(err)
	}
}

// seedChunk inserts record+chunk+fts and, when v != nil, the vec row.
func seedChunk(t *testing.T, st *store.Store, id int64, sid, project, kind, role string, ts time.Time, text string, v []float32) {
	t.Helper()
	db := st.DB()
	rec := 1000 + id
	if _, err := db.Exec(`INSERT OR IGNORE INTO files(id, path) VALUES (?,?)`, rec, "f"+sid+string(rune('a'+id%26))+text[:1]+time.Now().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO records(id,file_id,line_no,byte_off,byte_len,session_id,ts) VALUES (?,?,?,?,?,?,?)`, rec, rec, 1, 0, 0, sid, ts.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chunks(id,record_id,session_id,kind,role,ts,seq,text) VALUES (?,?,?,?,?,?,0,?)`, id, rec, sid, kind, role, ts.UTC().Format(time.RFC3339), text); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO chunk_fts(rowid,text) VALUES (?,?)`, id, text); err != nil {
		t.Fatal(err)
	}
	if v != nil {
		if _, err := db.Exec(`INSERT INTO chunk_vec(rowid,embedding,session_id,project,kind,role,ts) VALUES (?,?,?,?,?,?,?)`,
			id, embed.VecBytes(v), sid, project, kind, role, ts.Unix()); err != nil {
			t.Fatal(err)
		}
	}
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestRunGroupsBySession(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "Auto title", "Custom title")
	seedSession(t, st, "s2", "p", "Other title", "")
	seedChunk(t, st, 1, "s1", "p", "text", "user", t0, "alpha one", vec(0))
	seedChunk(t, st, 2, "s1", "p", "text", "user", t0, "alpha two", vec(1))
	seedChunk(t, st, 3, "s2", "p", "text", "user", t0, "beta", vec(2))
	emb := fakeOllama(t, vec(0))
	hits, route, err := Run(context.Background(), st, emb, "how do things work", Opts{Mode: "vector", K: 10})
	if err != nil || route != "vector" {
		t.Fatalf("route=%s err=%v", route, err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 sessions, got %d: %+v", len(hits), hits)
	}
	if hits[0].SessionID != "s1" || hits[0].ChunkID != 1 || hits[0].Title != "Custom title" || hits[0].Project != "p" {
		t.Fatalf("hit0 = %+v", hits[0])
	}
	if hits[1].SessionID != "s2" || hits[1].Title != "Other title" || hits[0].Score < hits[1].Score {
		t.Fatalf("hit1 = %+v", hits[1])
	}
	hits, _, _ = Run(context.Background(), st, emb, "how do things work", Opts{Mode: "vector", K: 1})
	if len(hits) != 1 {
		t.Fatalf("K=1 got %d", len(hits))
	}
}

func TestAutoFallsBackToVectorOnEmptyFts(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	seedChunk(t, st, 1, "s1", "p", "text", "user", t0, "nothing relevant", vec(0))
	hits, route, err := Run(context.Background(), st, fakeOllama(t, vec(0)), "my_missing_ident", Opts{Mode: "auto", K: 5})
	if err != nil || route != "vector" || len(hits) != 1 {
		t.Fatalf("route=%s err=%v hits=%v", route, err, hits)
	}
}

func TestAutoRoutesIdentifiersToFts(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	seedChunk(t, st, 1, "s1", "p", "text", "user", t0, "look at internal/store/store.go now", vec(0))
	hits, route, err := Run(context.Background(), st, deadOllama(t), "internal/store/store.go", Opts{Mode: "auto", K: 5})
	if err != nil || route != "fts" || len(hits) != 1 {
		t.Fatalf("route=%s err=%v hits=%v", route, err, hits)
	}
}

func TestAutoFallsBackToFtsWhenOllamaDown(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	seedChunk(t, st, 1, "s1", "p", "text", "user", t0, "database migration plan", vec(0))
	hits, route, err := Run(context.Background(), st, deadOllama(t), "database migration", Opts{Mode: "auto", K: 5})
	if err != nil || route != "fts-fallback" || len(hits) != 1 {
		t.Fatalf("route=%s err=%v hits=%v", route, err, hits)
	}
	if _, _, err := Run(context.Background(), st, deadOllama(t), "database migration", Opts{Mode: "vector", K: 5}); err == nil {
		t.Fatal("vector mode must return the error")
	}
}

func TestFiltersApplyInsideKNN(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	// 8 near chunks of kind a, 2 far chunks of kind b: unfiltered top-2 are all kind a.
	for i := 1; i <= 8; i++ {
		v := vec(0)
		v[1] = float32(i) * 0.01
		seedChunk(t, st, int64(i), "s1", "p", "a", "user", t0, "near", v)
	}
	seedChunk(t, st, 9, "s1", "p", "b", "user", t0, "far", vec(5))
	seedChunk(t, st, 10, "s1", "p", "b", "assistant", t0.Add(time.Hour), "far", vec(6))
	ids, err := vecLeg(context.Background(), st.DB(), vec(0), 2, filter{Kind: "b"})
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	ids, err = vecLeg(context.Background(), st.DB(), vec(0), 2, filter{Role: "assistant", Since: t0.Add(time.Minute), Project: "p"})
	if err != nil || len(ids) != 1 || ids[0].id != 10 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

func TestFtsFiltersScope(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p1", "t", "")
	seedSession(t, st, "s2", "p2", "t", "")
	seedChunk(t, st, 1, "s1", "p1", "text", "user", t0, "shared keyword here", nil)
	seedChunk(t, st, 2, "s2", "p2", "text", "user", t0, "shared keyword here", nil)
	hits, _, err := Run(context.Background(), st, nil, "shared keyword", Opts{Mode: "fts", K: 5, Project: "p2"})
	if err != nil || len(hits) != 1 || hits[0].SessionID != "s2" {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
	hits, _, _ = Run(context.Background(), st, nil, "shared keyword", Opts{Mode: "fts", K: 5, Since: t0.Add(time.Hour)})
	if len(hits) != 0 {
		t.Fatalf("since filter: %v", hits)
	}
}

func TestToolFtsHitsMapToChunks(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	seedChunk(t, st, 1, "s1", "p", "text", "assistant", t0, "I ran the build step", nil)
	if _, err := st.DB().Exec(`INSERT INTO tool_fts(text, record_id) VALUES ('ERROR zxcvb_failure in output', 1001)`); err != nil {
		t.Fatal(err)
	}
	hits, route, err := Run(context.Background(), st, nil, "zxcvb_failure", Opts{Mode: "fts", K: 5})
	if err != nil || route != "fts" || len(hits) != 1 || hits[0].ChunkID != 1 {
		t.Fatalf("route=%s err=%v hits=%v", route, err, hits)
	}
}

func TestRunHybridFusesLegs(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	seedSession(t, st, "s2", "p", "t", "")
	seedSession(t, st, "s3", "p", "t", "")
	seedSession(t, st, "s4", "p", "t", "")
	// 1: vector only (closest), 2: fts only, 3: both legs, 4: filler.
	seedChunk(t, st, 1, "s1", "p", "text", "user", t0, "unrelated words", vec(0))
	seedChunk(t, st, 2, "s2", "p", "text", "user", t0, "quokka marsupial", nil)
	v3 := vec(0)
	v3[1] = 0.5
	seedChunk(t, st, 3, "s3", "p", "text", "user", t0, "quokka habitat", v3)
	seedChunk(t, st, 4, "s4", "p", "text", "user", t0, "filler", vec(7))
	hits, route, err := Run(context.Background(), st, fakeOllama(t, vec(0)), "quokka", Opts{Mode: "hybrid", K: 10})
	if err != nil || route != "hybrid" {
		t.Fatalf("route=%s err=%v", route, err)
	}
	pos := map[int64]int{}
	for i, h := range hits {
		pos[h.ChunkID] = i
	}
	if _, ok := pos[1]; !ok {
		t.Fatalf("vector-only chunk missing: %+v", hits)
	}
	if _, ok := pos[2]; !ok {
		t.Fatalf("fts-only chunk missing: %+v", hits)
	}
	if p, ok := pos[3]; !ok || p != 0 {
		t.Fatalf("chunk in both legs should rank first: %+v", hits)
	}
}

func TestToolFtsRespectsFilters(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	seedChunk(t, st, 1, "s1", "p", "tool", "assistant", t0, "ran build one", nil)
	seedChunk(t, st, 2, "s1", "p", "text", "assistant", t0, "ran build two", nil)
	for _, r := range []int{1001, 1002} {
		if _, err := st.DB().Exec(`INSERT INTO tool_fts(text, record_id) VALUES ('zxcvb_failure output', ?)`, r); err != nil {
			t.Fatal(err)
		}
	}
	hits, _, err := Run(context.Background(), st, nil, "zxcvb_failure", Opts{Mode: "fts", K: 5, Kind: "text"})
	if err != nil || len(hits) != 1 || hits[0].ChunkID != 2 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	hits, _, _ = Run(context.Background(), st, nil, "zxcvb_failure", Opts{Mode: "fts", K: 5})
	if len(hits) != 1 {
		t.Fatalf("one session expected: %+v", hits)
	}
}

func TestFtsRawPhraseMatchesPath(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p", "t", "")
	seedSession(t, st, "s2", "p", "t", "")
	seedChunk(t, st, 1, "s1", "p", "text", "user", t0, "claude skills history claude lives under dot claude folders somewhere", nil)
	seedChunk(t, st, 2, "s2", "p", "text", "user", t0, "install it at /.claude/skills/claude-history for use", nil)
	hits, _, err := Run(context.Background(), st, nil, "/.claude/skills/claude-history", Opts{Mode: "fts", K: 5})
	if err != nil || len(hits) == 0 || hits[0].ChunkID != 2 {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if got := ftsQuery("/.claude/skills/claude-history"); got[0] != `"/.claude/skills/claude-history"` {
		t.Fatalf("cascade = %v", got)
	}
	if _, _, err := Run(context.Background(), st, nil, `say "hi"`, Opts{Mode: "fts", K: 5}); err != nil {
		t.Fatal(err)
	}
}

// seedToolOnlyRecord inserts a record with no chunk rows plus its tool_fts row.
func seedToolOnlyRecord(t *testing.T, st *store.Store, rec int64, sid, fileKind, typ, role string, ts time.Time, tool string) {
	t.Helper()
	db := st.DB()
	if _, err := db.Exec(`INSERT INTO files(id, path, kind) VALUES (?,?,?)`, rec, "tool-file-"+sid+tool[:3], fileKind); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO records(id,file_id,line_no,byte_off,byte_len,session_id,type,ts) VALUES (?,?,?,?,?,?,?,?)`, rec, rec, 1, 0, 0, sid, typ, ts.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(record_id, role, text) VALUES (?,?,'')`, rec, role); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tool_fts(text, record_id) VALUES (?,?)`, tool, rec); err != nil {
		t.Fatal(err)
	}
}

func TestToolOnlyRecordSurfaces(t *testing.T) {
	st := newStore(t)
	seedSession(t, st, "s1", "p1", "title1", "")
	seedSession(t, st, "s2", "p2", "title2", "")
	seedToolOnlyRecord(t, st, 7, "s1", "main", "assistant", "assistant", t0, `Bash {"command":"zxqv_unique_marker --flag"}`)
	seedToolOnlyRecord(t, st, 8, "s2", "subagent", "user", "user", t0, `zxqv_unique_marker output`)
	ctx := context.Background()
	hits, route, err := Run(ctx, st, nil, "zxqv_unique_marker", Opts{Mode: "fts", K: 5})
	if err != nil || route != "fts" || len(hits) != 2 {
		t.Fatalf("route=%s err=%v hits=%+v", route, err, hits)
	}
	for _, h := range hits {
		if h.ChunkID >= 0 || h.Chunk == "" || h.SessionID == "" || h.TS == "" {
			t.Fatalf("not a record-level hit: %+v", h)
		}
	}
	for _, c := range []struct {
		o   Opts
		sid string
	}{
		{Opts{Project: "p1"}, "s1"}, {Opts{Role: "user"}, "s2"}, {Opts{Kind: "subagent"}, "s2"},
	} {
		c.o.Mode, c.o.K = "fts", 5
		hits, _, err := Run(ctx, st, nil, "zxqv_unique_marker", c.o)
		if err != nil || len(hits) != 1 || hits[0].SessionID != c.sid {
			t.Fatalf("%+v: hits=%+v err=%v", c.o, hits, err)
		}
	}
	hits, _, _ = Run(ctx, st, nil, "zxqv_unique_marker", Opts{Mode: "fts", K: 5, Since: t0.Add(time.Hour)})
	if len(hits) != 0 {
		t.Fatalf("since filter: %+v", hits)
	}
	hits, _, _ = Run(ctx, st, nil, "zxqv_unique_marker", Opts{Mode: "fts", K: 5, NoToolFTS: true})
	if len(hits) != 0 {
		t.Fatalf("no-tool-fts: %+v", hits)
	}
}
