package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
)

// peerDB builds a peer archive file with the given sessions seeded through fn.
func peerDB(t *testing.T, name string, fn func(st *store.Store)) paths.Peer {
	t.Helper()
	p := filepath.Join(t.TempDir(), name, "archive.sqlite")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	fn(st)
	if err := st.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	st.Close()
	return paths.Peer{Name: name, Path: p}
}

func setProfile(t *testing.T, st *store.Store, sid, profile string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE sessions SET profile=? WHERE session_id=?`, profile, sid); err != nil {
		t.Fatal(err)
	}
}

type warns struct{ msgs []string }

func (w *warns) add(m string)   { w.msgs = append(w.msgs, m) }
func (w *warns) joined() string { return strings.Join(w.msgs, "\n") }

func TestRunMergesPeersDedupesAndLabelsProfiles(t *testing.T) {
	own := newStore(t)
	seedSession(t, own, "s-own", "p", "Own title", "")
	setProfile(t, own, "s-own", "desktop")
	seedChunk(t, own, 1, "s-own", "p", "text", "user", t0, "zebrafish alpha on desktop", vec(0))
	// A session present in both archives: the peer's copy is the better hit.
	seedSession(t, own, "s-dup", "p", "Dup title", "")
	setProfile(t, own, "s-dup", "desktop")
	seedChunk(t, own, 2, "s-dup", "p", "text", "user", t0, "unrelated filler words here", vec(7))

	peer := peerDB(t, "laptop", func(st *store.Store) {
		seedSession(t, st, "s-peer", "p", "Peer title", "")
		setProfile(t, st, "s-peer", "laptop")
		// Same chunk id (1) as in own: ids must not collide across databases.
		seedChunk(t, st, 1, "s-peer", "p", "text", "user", t0, "zebrafish beta zebrafish on laptop", vec(1))
		seedSession(t, st, "s-dup", "p", "Dup title", "")
		setProfile(t, st, "s-dup", "laptop")
		seedChunk(t, st, 2, "s-dup", "p", "text", "user", t0, "zebrafish zebrafish zebrafish dup", vec(2))
	})

	w := &warns{}
	hits, route, err := Run(context.Background(), own, nil, "zebrafish", Opts{
		Mode: "fts", K: 10, Peers: []paths.Peer{peer}, OwnName: "desktop", Warn: w.add, NoToolFTS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if route != "fts" || len(w.msgs) != 0 {
		t.Fatalf("route=%s warns=%v", route, w.msgs)
	}
	got := map[string]string{}
	for _, h := range hits {
		if _, dup := got[h.SessionID]; dup {
			t.Fatalf("session %s returned twice: %+v", h.SessionID, hits)
		}
		got[h.SessionID] = h.Profile
	}
	if len(hits) != 3 || got["s-own"] != "desktop" || got["s-peer"] != "laptop" {
		t.Fatalf("hits = %+v", hits)
	}
	if got["s-dup"] != "laptop" { // best hit kept; its label comes with it
		t.Fatalf("s-dup label = %q, want laptop (peer copy is the best hit)", got["s-dup"])
	}

	// --profile filters on the label.
	hits, _, err = Run(context.Background(), own, nil, "zebrafish", Opts{
		Mode: "fts", K: 10, Peers: []paths.Peer{peer}, OwnName: "desktop", Profile: "laptop", NoToolFTS: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Profile != "laptop" {
			t.Fatalf("profile filter leaked %+v", h)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("laptop hits = %+v", hits)
	}
}

func TestRunVectorMergesAcrossDBsByDistance(t *testing.T) {
	own := newStore(t)
	seedSession(t, own, "s-own", "p", "Own", "")
	seedChunk(t, own, 1, "s-own", "p", "text", "user", t0, "own far text", vec(5))
	peer := peerDB(t, "laptop", func(st *store.Store) {
		seedSession(t, st, "s-peer", "p", "Peer", "")
		setProfile(t, st, "s-peer", "laptop")
		seedChunk(t, st, 1, "s-peer", "p", "text", "user", t0, "peer near text", vec(3))
	})
	// Query vector equals the peer's vector exactly, so the peer hit is first.
	hits, route, err := Run(context.Background(), own, fakeOllama(t, vec(3)), "some words about things", Opts{
		Mode: "vector", K: 5, Peers: []paths.Peer{peer}, OwnName: "desktop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if route != "vector" || len(hits) != 2 || hits[0].SessionID != "s-peer" || hits[0].Profile != "laptop" || hits[1].Profile != "desktop" {
		t.Fatalf("route=%s hits=%+v", route, hits)
	}
}

func TestRunSkipsUnreadablePeer(t *testing.T) {
	own := newStore(t)
	seedSession(t, own, "s-own", "p", "Own", "")
	seedChunk(t, own, 1, "s-own", "p", "text", "user", t0, "zebrafish survives", vec(0))
	bad := filepath.Join(t.TempDir(), "bad", "archive.sqlite")
	os.MkdirAll(filepath.Dir(bad), 0o755)
	os.WriteFile(bad, []byte(strings.Repeat("this is not sqlite ", 200)), 0o644)
	missing := paths.Peer{Name: "gone", Path: filepath.Join(t.TempDir(), "nope.sqlite")}

	w := &warns{}
	hits, _, err := Run(context.Background(), own, nil, "zebrafish", Opts{
		Mode: "fts", K: 5, Peers: []paths.Peer{{Name: "bad", Path: bad}, missing}, OwnName: "desktop", Warn: w.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SessionID != "s-own" {
		t.Fatalf("hits = %+v", hits)
	}
	if j := w.joined(); !strings.Contains(j, "skipping peer bad") || !strings.Contains(j, "skipping peer gone") {
		t.Fatalf("warnings = %q", j)
	}
}

func TestRunFingerprintMismatchSkipsPeerVectorLegOnly(t *testing.T) {
	own := newStore(t)
	seedSession(t, own, "s-own", "p", "Own", "")
	seedChunk(t, own, 1, "s-own", "p", "text", "user", t0, "own words about zebrafish", vec(5))
	peer := peerDB(t, "laptop", func(st *store.Store) {
		seedSession(t, st, "s-peer", "p", "Peer", "")
		setProfile(t, st, "s-peer", "laptop")
		seedChunk(t, st, 1, "s-peer", "p", "text", "user", t0, "peer words about zebrafish", vec(3))
		if _, err := st.DB().Exec(`UPDATE meta SET value='other-model|v9' WHERE key='embedder_fingerprint'`); err != nil {
			t.Fatal(err)
		}
	})
	w := &warns{}
	o := Opts{K: 5, Peers: []paths.Peer{peer}, OwnName: "desktop", Warn: w.add, NoToolFTS: true}

	o.Mode = "vector"
	hits, _, err := Run(context.Background(), own, fakeOllama(t, vec(3)), "zebrafish things", o)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SessionID != "s-own" || !strings.Contains(w.joined(), "fingerprint") {
		t.Fatalf("vector: hits=%+v warns=%q", hits, w.joined())
	}

	o.Mode = "fts"
	hits, _, err = Run(context.Background(), own, nil, "zebrafish", o)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("fts still uses the peer: %+v", hits)
	}
}

func TestProfileFilterGrowsPoolForSmallProfile(t *testing.T) {
	own := newStore(t)
	// 200 chunks of the big profile, all matching and nearer than the small ones.
	for i := 1; i <= 200; i++ {
		sid := "big" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		seedSession(t, own, sid, "p", "Big", "")
		setProfile(t, own, sid, "desktop")
		seedChunk(t, own, int64(i), sid, "p", "text", "user", t0, "zebrafish big profile row "+sid, vec(0))
	}
	for i := 0; i < 3; i++ {
		sid := "tiny" + string(rune('a'+i))
		seedSession(t, own, sid, "p", "Tiny", "")
		setProfile(t, own, sid, "default")
		seedChunk(t, own, int64(1000+i), sid, "p", "text", "user", t0, "zebrafish tiny profile row "+sid, vec(1))
	}
	for _, mode := range []string{"fts", "vector"} {
		hits, _, err := Run(context.Background(), own, fakeOllama(t, vec(0)), "zebrafish", Opts{
			Mode: mode, K: 10, Profile: "default", OwnName: "desktop", NoToolFTS: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 3 {
			t.Fatalf("%s: got %d hits, want 3: %+v", mode, len(hits), hits)
		}
	}
}

// Real-world ratio: ~250 profile chunks among 30,000, all far from the query,
// i.e. outside the KNN's top maxVecK candidates.
func TestProfileFilterExactScanAtRealRatio(t *testing.T) {
	own := newStore(t)
	db := own.DB()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	must := func(q string, a ...any) {
		if _, err := tx.Exec(q, a...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO files(id, path) VALUES (1,'f')`)
	near, far := embed.VecBytes(vec(0)), embed.VecBytes(vec(1))
	nsess := 0
	for i := 1; i <= 30000; i++ {
		small := i%120 == 0 // 250 chunks
		sid := fmt.Sprintf("big%03d", i%200)
		prof, vb := "desktop", near
		if small {
			sid = fmt.Sprintf("small%02d", (i/120)%25) // 25 sessions
			prof, vb = "default", far
		}
		if i <= 200 || small && nsess < 25 {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO sessions(session_id, project, profile) VALUES (?,?,?)`, sid, "p", prof); err != nil {
				t.Fatal(err)
			}
			if small {
				nsess++
			}
		}
		must(`INSERT INTO records(id,file_id,line_no,byte_off,byte_len,session_id) VALUES (?,1,?,0,0,?)`, i, i, sid)
		must(`INSERT INTO chunks(id,record_id,session_id,kind,role,ts,seq,text) VALUES (?,?,?,'text','user','2026-01-01T00:00:00Z',0,'x')`, i, i, sid)
		must(`INSERT INTO chunk_vec(rowid,embedding,session_id,project,kind,role,ts) VALUES (?,?,?,?,?,?,?)`, i, vb, sid, "p", "text", "user", 1767225600)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var np int
	db.QueryRow(`SELECT count(*) FROM chunks c JOIN sessions s ON s.session_id=c.session_id WHERE s.profile='default'`).Scan(&np)
	if np != 250 {
		t.Fatalf("profile chunks = %d", np)
	}
	hits, route, err := Run(context.Background(), own, fakeOllama(t, vec(0)), "configuration of things", Opts{
		Mode: "vector", K: 10, Profile: "default", OwnName: "desktop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if route != "vector" || len(hits) != 10 {
		t.Fatalf("route=%s got %d hits, want 10", route, len(hits))
	}
	for _, h := range hits {
		if h.Profile != "default" {
			t.Fatalf("leak %+v", h)
		}
	}
}

func TestProfilePassesEmbedQueryOnce(t *testing.T) {
	old := exactScanMax
	exactScanMax = 0 // force the KNN + pool-growth path
	defer func() { exactScanMax = old }()
	own := newStore(t)
	for i := 1; i <= 200; i++ {
		sid := fmt.Sprintf("big%03d", i)
		seedSession(t, own, sid, "p", "Big", "")
		setProfile(t, own, sid, "desktop")
		seedChunk(t, own, int64(i), sid, "p", "text", "user", t0, "row "+sid, vec(0))
	}
	for i := 0; i < 3; i++ {
		sid := fmt.Sprintf("tiny%d", i)
		seedSession(t, own, sid, "p", "Tiny", "")
		setProfile(t, own, sid, "default")
		seedChunk(t, own, int64(1000+i), sid, "p", "text", "user", t0, "row "+sid, vec(1))
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{vec(0)}})
	}))
	defer srv.Close()
	hits, _, err := Run(context.Background(), own, embed.NewClient(srv.URL, "m"), "some prose words here", Opts{
		Mode: "auto", K: 10, Profile: "default", OwnName: "desktop",
	})
	if err != nil || len(hits) != 3 {
		t.Fatalf("hits=%d err=%v", len(hits), err)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("embed calls = %d, want 1 across several passes", c)
	}
}

// One session owns 150 nearer chunks; 12 others own one farther chunk each.
// The exact leg must return the whole profile so K=10 sessions are found in
// the first pass (no truncation to the n=100 pool).
func TestExactProfileScanReturnsWholeProfile(t *testing.T) {
	own := newStore(t)
	for i := 1; i <= 300; i++ { // filler of another profile, nearest to the query
		sid := fmt.Sprintf("big%03d", i)
		seedSession(t, own, sid, "p", "Big", "")
		setProfile(t, own, sid, "desktop")
		seedChunk(t, own, int64(i), sid, "p", "text", "user", t0, "row "+sid, vec(0))
	}
	seedSession(t, own, "dense", "p", "Dense", "")
	setProfile(t, own, "dense", "default")
	for i := 0; i < 150; i++ {
		seedChunk(t, own, int64(1000+i), "dense", "p", "text", "user", t0, fmt.Sprintf("dense %d", i), vec(2))
	}
	for i := 0; i < 12; i++ {
		sid := fmt.Sprintf("sparse%02d", i)
		seedSession(t, own, sid, "p", "Sparse", "")
		setProfile(t, own, sid, "default")
		seedChunk(t, own, int64(2000+i), sid, "p", "text", "user", t0, "sparse "+sid, vec(1))
	}
	hits, _, err := Run(context.Background(), own, fakeOllama(t, vec(2)), "some prose words here", Opts{
		Mode: "vector", K: 10, Profile: "default", OwnName: "desktop",
	})
	if err != nil || len(hits) != 10 {
		t.Fatalf("hits=%d err=%v", len(hits), err)
	}
}
