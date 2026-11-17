package search

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/provider"
	"github.com/wamra/session-index/internal/store"
)

const (
	rrfK         = 60
	embedTimeout = 3 * time.Second
	defaultK     = 10
	maxTerms     = 24
)

// Opts are the search options. Mode is auto|vector|fts|hybrid (empty = auto).
type Opts struct {
	NoToolFTS           bool // skip the tool_fts leg (eval/diagnostics)
	Mode                string
	K                   int
	Since               time.Time
	Project, Role, Kind string
	// Profile keeps only hits whose session belongs to this profile.
	Profile string
	// Peers are other archive databases opened read-only and searched in
	// addition to st. Peers that fail to open are skipped with a Warn.
	Peers []paths.Peer
	// OwnName labels hits of st whose sessions carry no profile.
	OwnName string
	// Warn receives non-fatal problems (skipped peers, fingerprint mismatch).
	Warn func(msg string)
}

func (o Opts) warn(format string, a ...any) {
	if o.Warn != nil {
		o.Warn(fmt.Sprintf(format, a...))
	}
}

// Hit is the best chunk of one session.
type Hit struct {
	SessionID, Project, Title string
	Profile                   string
	Score                     float64
	Chunk                     string
	ChunkID                   int64
	TS                        string
	Role, Kind                string
}

// filter holds the optional metadata predicates.
type filter struct {
	Since               time.Time
	Project, Role, Kind string
}

func (f filter) set() bool {
	return !f.Since.IsZero() || f.Project != "" || f.Role != "" || f.Kind != ""
}

// cand is a leg result: a chunk id with a leg-local score (higher is better).
type cand struct {
	src   int // index into the sources slice (0 = own archive)
	id    int64
	score float64
	snip  string // record-level hits (id < 0): tool text snippet
}

type ckey struct {
	src int
	id  int64
}

func ids(cs []cand) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.id
	}
	return out
}

// source is one searched archive database.
type source struct {
	name       string
	db         *sql.DB
	own        bool
	hasProfile bool
	vecSkip    string // non-empty: reason the vector leg is skipped
}

// profileExpr is the SQL expression for the session's profile (alias s).
func (s *source) profileExpr() string {
	if s.hasProfile {
		return `COALESCE(NULLIF(s.profile,''),'')`
	}
	return `''`
}

// openSources returns the own archive followed by every peer that opens.
func openSources(ctx context.Context, st *store.Store, o Opts) ([]*source, func()) {
	ownHas, _ := store.HasColumn(st.DB(), "sessions", "profile")
	srcs := []*source{{name: o.OwnName, db: st.DB(), own: true, hasProfile: ownHas}}
	var ownFP string
	st.DB().QueryRowContext(ctx, `SELECT value FROM meta WHERE key='embedder_fingerprint'`).Scan(&ownFP)
	ownReal := realPath(st.Path())
	seen := map[string]bool{ownReal: true}
	var opened []*store.Store
	for _, p := range o.Peers {
		real := realPath(p.Path)
		if seen[real] {
			continue
		}
		seen[real] = true
		ps, err := store.OpenReadOnly(p.Path)
		if err == nil {
			var n int
			err = ps.DB().QueryRowContext(ctx, `SELECT count(*) FROM chunks`).Scan(&n)
			if err != nil {
				ps.Close()
			}
		}
		if err != nil {
			o.warn("skipping peer %s (%s): %v", p.Name, p.Path, err)
			continue
		}
		opened = append(opened, ps)
		has, _ := store.HasColumn(ps.DB(), "sessions", "profile")
		src := &source{name: p.Name, db: ps.DB(), hasProfile: has}
		var fp string
		ps.DB().QueryRowContext(ctx, `SELECT value FROM meta WHERE key='embedder_fingerprint'`).Scan(&fp)
		if fp != ownFP {
			src.vecSkip = fmt.Sprintf("peer %s embedder fingerprint %q differs from own %q; vector leg skipped (fts still used)", p.Name, fp, ownFP)
		}
		srcs = append(srcs, src)
	}
	return srcs, func() {
		for _, ps := range opened {
			ps.Close()
		}
	}
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return p
}

// legErr handles a per-source leg failure: the own archive's error is fatal,
// a peer is warned about and skipped.
func legErr(o Opts, s *source, err error) error {
	if s.own {
		return err
	}
	o.warn("skipping peer %s: %v", s.name, err)
	return nil
}

// merge combines per-source legs best-first (stable across sources) and keeps
// n. Vector scores are 1-distance (distance ascending across databases); fts
// scores are 1/(rrfK+rank+1) of the in-database rank, so sorting them merges
// by reciprocal rank fusion across databases.
func merge(legs [][]cand, n int) []cand {
	var all []cand
	for _, l := range legs {
		all = append(all, l...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

func ftsAll(ctx context.Context, srcs []*source, o Opts, q string, n int, f filter, withTools bool) ([]cand, error) {
	var legs [][]cand
	for i, s := range srcs {
		r, err := ftsLeg(ctx, s.db, q, n, f, withTools)
		if err != nil {
			if e := legErr(o, s, err); e != nil {
				return nil, e
			}
			continue
		}
		for j := range r {
			r[j].src = i
		}
		legs = append(legs, r)
	}
	return merge(legs, n), nil
}

// vectorAll embeds q once (3 s deadline) and runs the filtered KNN on every
// source whose embedder fingerprint matches.
func vectorAll(ctx context.Context, srcs []*source, o Opts, qv *queryVec, n int, f filter) ([]cand, error) {
	v, err := qv.get(ctx)
	if err != nil {
		return nil, err
	}
	var legs [][]cand
	limit := n
	for i, s := range srcs {
		if s.vecSkip != "" {
			o.warn("%s", s.vecSkip)
			continue
		}
		var r []cand
		var err error
		exact := false
		if o.Profile != "" {
			r, exact, err = exactProfileLeg(ctx, s, o.Profile, v, f)
		}
		qv.exact = qv.exact || exact
		if !exact && err == nil {
			r, err = vecLeg(ctx, s.db, v, min(n, maxVecK), f)
		}
		if err != nil {
			if e := legErr(o, s, err); e != nil {
				return nil, e
			}
			continue
		}
		for j := range r {
			r[j].src = i
		}
		if exact && len(r) > limit {
			limit = len(r) // an exact scan returns the whole profile
		}
		legs = append(legs, r)
	}
	return merge(legs, limit), nil
}

// Run searches q over st and every readable peer in o.Peers and returns hits
// grouped (deduplicated) by session plus the route used (vector, fts, hybrid,
// fts-fallback).
func Run(ctx context.Context, st *store.Store, emb provider.Embedder, q string, o Opts) ([]Hit, string, error) {
	k := o.K
	if k <= 0 {
		k = defaultK
	}
	n := 5 * k
	if n < 100 {
		n = 100
	}
	f := filter{Since: o.Since, Project: o.Project, Role: o.Role, Kind: o.Kind}
	srcs, closeAll := openSources(ctx, st, o)
	defer closeAll()

	qv := &queryVec{emb: emb, q: q}
	hits, route, err := runPool(ctx, srcs, qv, q, o, f, n, k)
	if err != nil || o.Profile == "" {
		return hits, route, err
	}
	// The profile filter is applied after ranking, so a small profile can
	// yield fewer than k hits from a pool of n candidates: grow the pool until
	// k hits are found or it covers every chunk.
	total := 0
	for _, s := range srcs {
		var c int
		if s.db.QueryRowContext(ctx, `SELECT count(*) FROM chunks`).Scan(&c) == nil {
			total += c
		}
	}
	for len(hits) < k && n < total {
		// An exact scan already returned every profile chunk, and a KNN-only
		// vector route cannot gain candidates beyond maxVecK.
		if route == "vector" && (qv.exact || n >= maxVecK) {
			break
		}
		n *= 2
		if hits, route, err = runPool(ctx, srcs, qv, q, o, f, n, k); err != nil {
			return nil, "", err
		}
	}
	return hits, route, nil
}

// queryVec embeds the query at most once per Run (3 s deadline); the vector
// or the error is reused by every pass of the pool-growth loop.
type queryVec struct {
	emb   provider.Embedder
	q     string
	done  bool
	v     []float32
	err   error
	exact bool // an exact-scan leg ran (its pool still grows past maxVecK)
}

func (c *queryVec) get(ctx context.Context) ([]float32, error) {
	if c.done {
		return c.v, c.err
	}
	c.done = true
	if c.emb == nil {
		c.err = fmt.Errorf("%w: no embedder configured", provider.ErrDown)
		return nil, c.err
	}
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	c.v, c.err = provider.Query(ectx, c.emb, c.q)
	return c.v, c.err
}

// maxVecK is sqlite-vec's cap on the KNN k.
const maxVecK = 4096

// runPool runs the routed legs with candidate pool n and groups the result.
func runPool(ctx context.Context, srcs []*source, qv *queryVec, q string, o Opts, f filter, n, k int) ([]Hit, string, error) {
	mode := o.Mode
	switch mode {
	case "", "auto":
		mode = "auto"
	case "vector", "fts", "hybrid":
	default:
		return nil, "", fmt.Errorf("search: unknown mode %q", o.Mode)
	}

	var ranked []cand
	route := mode
	switch mode {
	case "fts":
		r, err := ftsAll(ctx, srcs, o, q, n, f, !o.NoToolFTS)
		if err != nil {
			return nil, "", err
		}
		ranked = r
	case "vector":
		r, err := vectorAll(ctx, srcs, o, qv, n, f)
		if err != nil {
			return nil, "", err
		}
		ranked = r
	case "hybrid":
		fr, err := ftsAll(ctx, srcs, o, q, n, f, !o.NoToolFTS)
		if err != nil {
			return nil, "", err
		}
		vr, err := vectorAll(ctx, srcs, o, qv, n, f)
		if err != nil {
			return nil, "", err
		}
		ranked = fuse(vr, fr, rrfK)
	case "auto":
		if LooksLikeIdentifier(q) {
			route = "fts"
			r, err := ftsAll(ctx, srcs, o, q, n, f, !o.NoToolFTS)
			if err != nil {
				return nil, "", err
			}
			ranked = r
			if len(ranked) == 0 {
				vr, err := vectorAll(ctx, srcs, o, qv, n, f)
				switch {
				case err == nil:
					ranked, route = vr, "vector"
				case ctx.Err() != nil:
					return nil, "", ctx.Err()
				case !isDown(err):
					return nil, "", err
				}
			}
		} else {
			route = "vector"
			vr, err := vectorAll(ctx, srcs, o, qv, n, f)
			switch {
			case err == nil:
				ranked = vr
			case ctx.Err() != nil:
				return nil, "", ctx.Err()
			case isDown(err):
				route = "fts-fallback"
				r, ferr := ftsAll(ctx, srcs, o, q, n, f, false)
				if ferr != nil {
					return nil, "", ferr
				}
				ranked = r
			default:
				return nil, "", err
			}
		}
	}
	hits, err := group(ctx, srcs, o, ranked, k)
	return hits, route, err
}

func isDown(err error) bool {
	return errors.Is(err, provider.ErrDown) || errors.Is(err, context.DeadlineExceeded)
}

// vecLeg is a standalone KNN over chunk_vec (never joined); filters run
// inside the KNN through the metadata columns. Score is 1 - distance.
func vecLeg(ctx context.Context, db *sql.DB, v []float32, n int, f filter) ([]cand, error) {
	sqlText := `SELECT rowid, distance FROM chunk_vec WHERE embedding MATCH ? AND k = ?`
	args := []any{embed.VecBytes(v), n}
	if f.Project != "" {
		sqlText += ` AND project = ?`
		args = append(args, f.Project)
	}
	if f.Kind != "" {
		sqlText += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.Role != "" {
		sqlText += ` AND role = ?`
		args = append(args, f.Role)
	}
	if !f.Since.IsZero() {
		sqlText += ` AND ts >= ?`
		args = append(args, f.Since.Unix())
	}
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()
	var out []cand
	for rows.Next() {
		var c cand
		var d float64
		if err := rows.Scan(&c.id, &d); err != nil {
			return nil, err
		}
		c.score = 1 - d
		out = append(out, c)
	}
	return out, rows.Err()
}

var (
	tokRe     = regexp.MustCompile(`[A-Za-z0-9_]{2,}`)
	stopwords = map[string]bool{
		"the": true, "and": true, "for": true, "are": true, "was": true, "were": true, "with": true,
		"that": true, "this": true, "have": true, "has": true, "had": true, "not": true, "but": true,
		"from": true, "how": true, "what": true, "when": true, "where": true, "why": true, "who": true,
		"did": true, "does": true, "can": true, "you": true, "your": true, "our": true, "its": true,
		"is": true, "in": true, "on": true, "of": true, "to": true, "it": true, "an": true, "at": true,
		"be": true, "as": true, "or": true, "by": true, "do": true, "we": true,
	}
)

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ftsQuery builds the FTS5 cascade [phrase of the raw query, AND, OR]; every
// term is quoted so FTS5 syntax in the input cannot raise an error. Trigram
// needs >= 3 chars per quoted term, so the raw phrase is used only when the
// trimmed query has >= 3 characters and tokens shorter than 3 are dropped
// from the AND/OR forms.
func ftsQuery(q string) []string {
	var toks []string
	for _, t := range tokRe.FindAllString(q, -1) {
		if stopwords[strings.ToLower(t)] {
			continue
		}
		toks = append(toks, t)
		if len(toks) == maxTerms {
			break
		}
	}
	var long []string
	for _, t := range toks {
		if len(t) >= 3 {
			long = append(long, quote(t))
		}
	}
	var out []string
	add := func(s string) {
		for _, o := range out {
			if o == s {
				return
			}
		}
		out = append(out, s)
	}
	if raw := strings.TrimSpace(q); len(raw) >= 3 {
		add(quote(raw))
	}
	if len(long) > 0 {
		add(strings.Join(long, " AND "))
		add(strings.Join(long, " OR "))
	}
	return out
}

// allowedCTE builds the MATERIALIZED scope CTE for the filters ("" if none).
func allowedCTE(f filter) (string, []any) {
	if !f.set() {
		return "", nil
	}
	sqlText := `WITH allowed(id) AS MATERIALIZED (SELECT c.id FROM chunks c`
	var where []string
	var args []any
	if f.Project != "" {
		sqlText += ` JOIN sessions s ON s.session_id = c.session_id`
		where = append(where, `s.project = ?`)
		args = append(args, f.Project)
	}
	if f.Kind != "" {
		where = append(where, `c.kind = ?`)
		args = append(args, f.Kind)
	}
	if f.Role != "" {
		where = append(where, `c.role = ?`)
		args = append(args, f.Role)
	}
	if !f.Since.IsZero() {
		where = append(where, `c.ts >= ?`)
		args = append(args, f.Since.UTC().Format(time.RFC3339))
	}
	sqlText += ` WHERE ` + strings.Join(where, ` AND `) + `) `
	return sqlText, args
}

// ftsLeg runs the cascade on chunk_fts (and tool_fts when withTools),
// unioning tool hits into the leg. Score is the RRF contribution of the
// rank within the leg.
func ftsLeg(ctx context.Context, db *sql.DB, q string, n int, f filter, withTools bool) ([]cand, error) {
	cascade := ftsQuery(q)
	if len(cascade) == 0 {
		return nil, nil
	}
	cte, cargs := allowedCTE(f)
	join := ""
	if cte != "" {
		join = ` JOIN allowed ON allowed.id = chunk_fts.rowid`
	}
	var chunkIDs []int64
	for _, m := range cascade {
		args := append(append([]any{}, cargs...), m, n)
		got, err := queryIDs(ctx, db, cte+`SELECT chunk_fts.rowid FROM chunk_fts`+join+` WHERE chunk_fts MATCH ? ORDER BY bm25(chunk_fts) LIMIT ?`, args)
		if err != nil {
			return nil, fmt.Errorf("fts search: %w", err)
		}
		if len(got) > 0 {
			chunkIDs = got
			break
		}
	}
	var toolIDs []int64
	snips := map[int64]string{}
	if withTools {
		for _, m := range cascade {
			recs, texts, err := queryToolHits(ctx, db, m, n)
			if err != nil {
				return nil, fmt.Errorf("tool fts search: %w", err)
			}
			if len(recs) == 0 {
				continue
			}
			var err2 error
			toolIDs, err2 = toolChunks(ctx, db, recs, f)
			if err2 != nil {
				return nil, err2
			}
			for i, r := range recs {
				if _, ok := snips[r]; !ok {
					snips[r] = texts[i]
				}
			}
			break
		}
	}
	order := chunkIDs
	if len(toolIDs) > 0 {
		order = rrf(chunkIDs, toolIDs, rrfK)
	}
	if len(order) > n {
		order = order[:n]
	}
	out := make([]cand, len(order))
	for i, id := range order {
		out[i] = cand{id: id, score: 1 / float64(rrfK+i+1)}
		if id < 0 {
			out[i].snip = snips[-id]
		}
	}
	return out, nil
}

const maxSnipRunes = 2000

func snippet(s string) string {
	if len(s) <= maxSnipRunes { // bytes >= runes, so this is safe
		return s
	}
	rs := []rune(s)
	if len(rs) > maxSnipRunes {
		rs = rs[:maxSnipRunes]
	}
	return string(rs)
}

// queryToolHits returns the best-first tool_fts record ids for match m with
// the (snippet-trimmed) tool text of each row.
func queryToolHits(ctx context.Context, db *sql.DB, m string, n int) ([]int64, []string, error) {
	rows, err := db.QueryContext(ctx, `SELECT record_id, text FROM tool_fts WHERE tool_fts MATCH ? ORDER BY bm25(tool_fts) LIMIT ?`, m, n)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var recs []int64
	var texts []string
	for rows.Next() {
		var id int64
		var t string
		if err := rows.Scan(&id, &t); err != nil {
			return nil, nil, err
		}
		recs = append(recs, id)
		texts = append(texts, snippet(t))
	}
	return recs, texts, rows.Err()
}

func queryIDs(ctx context.Context, db *sql.DB, q string, args []any) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// rrf fuses two ranked id lists with reciprocal rank fusion
// (score += 1/(k+rank+1), rank 0-based) and returns ids best-first.
func rrf(a, b []int64, k int) []int64 {
	return ids(fuse(asCands(a), asCands(b), k))
}

func asCands(a []int64) []cand {
	out := make([]cand, len(a))
	for i, id := range a {
		out[i] = cand{id: id}
	}
	return out
}

func fuse(a, b []cand, k int) []cand {
	score := map[ckey]float64{}
	first := map[ckey]int{}
	snip := map[ckey]string{}
	for _, leg := range [][]cand{a, b} {
		for r, c := range leg {
			key := ckey{c.src, c.id}
			if c.snip != "" {
				snip[key] = c.snip
			}
			score[key] += 1 / float64(k+r+1)
			if _, ok := first[key]; !ok {
				first[key] = len(first)
			}
		}
	}
	out := make([]cand, 0, len(score))
	for key, s := range score {
		out = append(out, cand{src: key.src, id: key.id, score: s, snip: snip[key]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return first[ckey{out[i].src, out[i].id}] < first[ckey{out[j].src, out[j].id}]
	})
	return out
}

// group fetches chunk/session details for ranked candidates (per source) in
// second statements, drops hits outside o.Profile, keeps the best chunk per
// session id across all sources and returns at most k hits.
func group(ctx context.Context, srcs []*source, o Opts, ranked []cand, k int) ([]Hit, error) {
	if len(ranked) == 0 {
		return nil, nil
	}
	bySrc := make([][]cand, len(srcs))
	for _, c := range ranked {
		bySrc[c.src] = append(bySrc[c.src], c)
	}
	byKey := map[ckey]Hit{}
	for i, s := range srcs {
		if len(bySrc[i]) == 0 {
			continue
		}
		m, err := details(ctx, s, bySrc[i])
		if err != nil {
			if e := legErr(o, s, err); e != nil {
				return nil, e
			}
			continue
		}
		for id, h := range m {
			if h.Profile == "" {
				h.Profile = s.name
			}
			byKey[ckey{i, id}] = h
		}
	}
	seen := map[string]bool{}
	var out []Hit
	for _, c := range ranked { // ranked is best-first
		h, ok := byKey[ckey{c.src, c.id}]
		if !ok || seen[h.SessionID] {
			continue
		}
		if o.Profile != "" && h.Profile != o.Profile {
			continue
		}
		seen[h.SessionID] = true
		h.Score = c.score
		if c.id < 0 {
			h.Chunk = c.snip
		}
		out = append(out, h)
		if len(out) == k {
			break
		}
	}
	return out, nil
}

// details loads the Hit rows for one source's candidates, keyed by cand id.
func details(ctx context.Context, src *source, ranked []cand) (map[int64]Hit, error) {
	db := src.db
	byID := make(map[int64]Hit, len(ranked))
	var chunkRanked []cand
	var recIDs []int64
	for _, c := range ranked {
		if c.id < 0 {
			recIDs = append(recIDs, -c.id)
		} else {
			chunkRanked = append(chunkRanked, c)
		}
	}
	if err := recordHits(ctx, src, recIDs, byID); err != nil {
		return nil, err
	}
	const batch = 500
	for i := 0; i < len(chunkRanked); i += batch {
		end := min(i+batch, len(chunkRanked))
		ph := strings.TrimSuffix(strings.Repeat("?,", end-i), ",")
		args := make([]any, 0, end-i)
		for _, c := range chunkRanked[i:end] {
			args = append(args, c.id)
		}
		rows, err := db.QueryContext(ctx, `SELECT c.id, c.session_id, COALESCE(s.project,''), `+src.profileExpr()+`, COALESCE(NULLIF(s.custom_title,''), s.title, ''),
			c.text, COALESCE(c.ts,''), COALESCE(c.role,''), COALESCE(c.kind,'')
			FROM chunks c LEFT JOIN sessions s ON s.session_id = c.session_id WHERE c.id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h Hit
			var sid sql.NullString
			if err := rows.Scan(&h.ChunkID, &sid, &h.Project, &h.Profile, &h.Title, &h.Chunk, &h.TS, &h.Role, &h.Kind); err != nil {
				rows.Close()
				return nil, err
			}
			h.SessionID = sid.String
			byID[h.ChunkID] = h
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return byID, nil
}

// recordHits fills byID (keyed by the negative record id) with record-level
// hits: session, kind, role and ts come from the record itself.
func recordHits(ctx context.Context, src *source, recIDs []int64, byID map[int64]Hit) error {
	db := src.db
	const batch = 500
	for i := 0; i < len(recIDs); i += batch {
		part := recIDs[i:min(i+batch, len(recIDs))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		args := make([]any, len(part))
		for j, r := range part {
			args[j] = r
		}
		rows, err := db.QueryContext(ctx, `SELECT r.id, COALESCE(r.session_id,''), COALESCE(s.project,''), `+src.profileExpr()+`, COALESCE(NULLIF(s.custom_title,''), s.title, ''),
			COALESCE(r.ts,''), COALESCE(m.role, r.type, ''), COALESCE(f.kind,'')
			FROM records r LEFT JOIN sessions s ON s.session_id = r.session_id
			LEFT JOIN messages m ON m.record_id = r.id LEFT JOIN files f ON f.id = r.file_id
			WHERE r.id IN (`+ph+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h Hit
			var rid int64
			if err := rows.Scan(&rid, &h.SessionID, &h.Project, &h.Profile, &h.Title, &h.TS, &h.Role, &h.Kind); err != nil {
				rows.Close()
				return err
			}
			h.ChunkID = -rid
			byID[-rid] = h
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// toolChunks maps ranked tool_fts record ids to candidate ids in tool-hit
// rank order: the record's chunk ids (scoped by the filters) or, when the
// record has no chunk rows at all, the negative record id (a record-level
// hit, scoped by the same filters through records/sessions/files/messages).
func toolChunks(ctx context.Context, db *sql.DB, recs []int64, f filter) ([]int64, error) {
	cte, cargs := allowedCTE(f)
	seen := map[int64]bool{}
	var uniq []int64
	for _, r := range recs {
		if !seen[r] {
			seen[r] = true
			uniq = append(uniq, r)
		}
	}
	extra := ""
	if cte != "" {
		extra = ` AND id IN (SELECT id FROM allowed)`
	}
	rjoin, rwhere := "", ""
	var rargs []any
	if f.Project != "" {
		rjoin += ` JOIN sessions s ON s.session_id = r.session_id`
		rwhere += ` AND s.project = ?`
		rargs = append(rargs, f.Project)
	}
	if f.Kind != "" {
		rjoin += ` JOIN files f ON f.id = r.file_id`
		rwhere += ` AND f.kind = ?`
		rargs = append(rargs, f.Kind)
	}
	if f.Role != "" {
		rjoin += ` LEFT JOIN messages m ON m.record_id = r.id`
		rwhere += ` AND COALESCE(m.role, r.type) = ?`
		rargs = append(rargs, f.Role)
	}
	if !f.Since.IsZero() {
		rwhere += ` AND r.ts >= ?`
		rargs = append(rargs, f.Since.UTC().Format(time.RFC3339))
	}
	const batch = 500
	byRec := map[int64][]int64{}
	recLevel := map[int64]bool{}
	for i := 0; i < len(uniq); i += batch {
		part := uniq[i:min(i+batch, len(uniq))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		idArgs := make([]any, len(part))
		for j, r := range part {
			idArgs[j] = r
		}
		args := append(append([]any{}, cargs...), idArgs...)
		rows, err := db.QueryContext(ctx, cte+`SELECT id, record_id FROM chunks WHERE record_id IN (`+ph+`)`+extra+` ORDER BY id`, args...)
		if err != nil {
			return nil, fmt.Errorf("tool fts chunks: %w", err)
		}
		for rows.Next() {
			var id, rec int64
			if err := rows.Scan(&id, &rec); err != nil {
				rows.Close()
				return nil, err
			}
			byRec[rec] = append(byRec[rec], id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		rargsAll := append(append([]any{}, rargs...), idArgs...)
		rrows, err := db.QueryContext(ctx, `SELECT r.id FROM records r`+rjoin+` WHERE r.superseded = 0`+rwhere+
			` AND r.id IN (`+ph+`) AND NOT EXISTS (SELECT 1 FROM chunks c WHERE c.record_id = r.id)`, rargsAll...)
		if err != nil {
			return nil, fmt.Errorf("tool fts records: %w", err)
		}
		for rrows.Next() {
			var id int64
			if err := rrows.Scan(&id); err != nil {
				rrows.Close()
				return nil, err
			}
			recLevel[id] = true
		}
		err = rrows.Err()
		rrows.Close()
		if err != nil {
			return nil, err
		}
	}
	var out []int64
	for _, r := range uniq {
		out = append(out, byRec[r]...)
		if recLevel[r] {
			out = append(out, -r)
		}
	}
	return out, nil
}

// exactScanMax is the largest profile (in chunks) searched by exact scan; it
// is a var so tests can force the KNN path.
//
// Measured on the live archive (30,089 chunks, vector search end to end):
// unfiltered 190 ms; --profile default (250 chunks, 0.8%) 162 ms by exact
// scan; a mid-sized profile (8,288 chunks, 27%) 2,021 ms p50 by exact scan because
// every profile vector is fetched from vec0 by rowid in 500-id batches. The
// 4096 KNN cap only starves tiny profiles (0.8% of 4096 is about 34
// candidates), whereas a 27% profile gets about 1,100 candidates in the first
// KNN pass, so larger profiles use the KNN with the pool-doubling loop.
var exactScanMax = 2000

// exactProfileLeg replaces the KNN for a profile filter: KNN cannot filter by
// profile (chunk_vec has no profile column) and caps k at maxVecK, so a small
// profile can be missing entirely from the top candidates. When the source's
// chunks of profile number at most exactScanMax, their embeddings are read and
// ranked in Go by L2 distance (score 1-distance, as vecLeg). exact is false
// when the KNN should be used instead. A source whose sessions carry no
// profile column holds only its own profile (named s.name).
func exactProfileLeg(ctx context.Context, s *source, profile string, v []float32, f filter) (out []cand, exact bool, err error) {
	if !s.hasProfile {
		return nil, false, nil // all of the source is s.name; KNN + label filter
	}
	q := `SELECT c.id FROM chunks c JOIN sessions s ON s.session_id = c.session_id WHERE s.profile = ?`
	args := []any{profile}
	if f.Project != "" {
		q += ` AND s.project = ?`
		args = append(args, f.Project)
	}
	if f.Kind != "" {
		q += ` AND c.kind = ?`
		args = append(args, f.Kind)
	}
	if f.Role != "" {
		q += ` AND c.role = ?`
		args = append(args, f.Role)
	}
	if !f.Since.IsZero() {
		q += ` AND c.ts >= ?`
		args = append(args, f.Since.UTC().Format(time.RFC3339))
	}
	q += ` LIMIT ?`
	args = append(args, exactScanMax+1)
	ids, err := queryIDs(ctx, s.db, q, args)
	if err != nil {
		return nil, false, err
	}
	if len(ids) > exactScanMax {
		return nil, false, nil
	}
	const batch = 500
	for i := 0; i < len(ids); i += batch {
		part := ids[i:min(i+batch, len(ids))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		a := make([]any, len(part))
		for j, id := range part {
			a[j] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT rowid, embedding FROM chunk_vec WHERE rowid IN (`+ph+`)`, a...)
		if err != nil {
			return nil, false, fmt.Errorf("exact scan: %w", err)
		}
		for rows.Next() {
			var id int64
			var b []byte
			if err := rows.Scan(&id, &b); err != nil {
				rows.Close()
				return nil, false, err
			}
			if len(b) != len(v)*4 {
				continue
			}
			var sum float64
			for k := range v {
				d := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[k*4:])) - v[k])
				sum += d * d
			}
			out = append(out, cand{id: id, score: 1 - math.Sqrt(sum)})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, false, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out, true, nil // every profile chunk: n does not truncate an exact scan
}
