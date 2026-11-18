package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wamra/session-index/internal/config"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/provider"
	"github.com/wamra/session-index/internal/search"
	"github.com/wamra/session-index/internal/store"
)

type jsonHit struct {
	SessionID string  `json:"session_id"`
	Profile   string  `json:"profile"`
	Project   string  `json:"project"`
	Title     string  `json:"title"`
	Score     float64 `json:"score"`
	Chunk     string  `json:"chunk"`
	ChunkID   int64   `json:"chunk_id"`
	TS        string  `json:"ts"`
	Role      string  `json:"role"`
	Kind      string  `json:"kind"`
}

// kindMarker prefixes hits that are not plain transcript text.
func kindMarker(kind string) string {
	switch kind {
	case "summary":
		return "[summary] "
	case "memory":
		return "[memory] "
	case "plan":
		return "[plan] "
	case "filechange":
		return "[file] "
	}
	return ""
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// repeatFlag is a repeatable string flag.
type repeatFlag []string

func (r *repeatFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatFlag) Set(v string) error { *r = append(*r, v); return nil }

// peerName labels an explicit archive path: <profile> for
// <profile>/cache/session-archive/archive.sqlite, else the file stem.
func peerName(p string) string {
	suffix := string(filepath.Separator) + paths.ArchiveRel
	if strings.HasSuffix(p, suffix) {
		return filepath.Base(strings.TrimSuffix(p, suffix))
	}
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// searchPeers lists the peer archives to search: SESSION_INDEX_PEERS
// (colon-separated paths) replaces automatic discovery of
// <profiles>/*/cache/session-archive/archive.sqlite; --db paths are added.
func searchPeers(extra []string, errw io.Writer) []paths.Peer {
	var peers []paths.Peer
	if env := config.WithLegacy(os.Getenv)("SESSION_INDEX_PEERS"); env != "" {
		for _, p := range filepath.SplitList(env) {
			if p != "" {
				peers = append(peers, paths.Peer{Name: peerName(p), Path: p})
			}
		}
	} else if own, pdir := ownProfile(); pdir != "" {
		peers = paths.DiscoverPeers(pdir, own)
	} else if len(extra) == 0 {
		fmt.Fprintln(errw, "search: no profiles tree found; searching own archive only")
	}
	for _, p := range extra {
		peers = append(peers, paths.Peer{Name: peerName(p), Path: p})
	}
	return peers
}

func cmdSearch(args []string, out, errw io.Writer) int {
	fs := newFlags("search", errw)
	mode := fs.String("mode", "auto", "auto|vector|fts|hybrid")
	k := fs.Int("k", 10, "max sessions")
	since := fs.String("since", "", "7d, 24h or YYYY-MM-DD")
	project := fs.String("project", "", "project substring")
	role := fs.String("role", "", "user|assistant")
	kind := fs.String("kind", "", "main|subagent|workflow|plan")
	asJSON := fs.Bool("json", false, "print JSON")
	profile := fs.String("profile", "", "only sessions of this profile")
	var dbs repeatFlag
	fs.Var(&dbs, "db", "extra archive.sqlite to search (repeatable)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) == 0 {
		fmt.Fprintln(errw, "search: QUERY is required")
		return 2
	}
	st0, err := parseSince(*since, time.Now())
	if err != nil {
		fmt.Fprintln(errw, err)
		return 2
	}
	st, err := openSearchStore(errw)
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer st.Close()
	ownName, _ := ownProfile()
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(errw, "search: %v\n", err)
		return 1
	}
	emb, err := newEmbedder(cfg, st)
	if err != nil {
		fmt.Fprintf(errw, "search: %v\n", err)
		return 1
	}
	// EffectiveVecDims reads the chunk_vec DDL when meta.vec_dims is not set
	// (a read-only open cannot migrate an older archive).
	fallback := false
	if sd, derr := store.EffectiveVecDims(st.DB()); derr == nil && sd > 0 && emb.Dims() > 0 && sd != emb.Dims() && *mode != "fts" {
		me := &store.DimsMismatchError{Stored: sd, Config: emb.Dims()}
		if *mode == "vector" {
			fmt.Fprintf(errw, "search: %v (try --mode fts)\n", me)
			return 1
		}
		fmt.Fprintf(errw, "search: %v; skipping the vector leg, using FTS\n", me)
		fallback = true
		m := "fts"
		mode = &m
	}
	hits, route, err := search.Run(context.Background(), st, emb, strings.Join(pos, " "), search.Opts{
		Mode: *mode, K: *k, Since: st0, Project: *project, Role: *role, Kind: *kind,
		Profile: *profile, Peers: searchPeers(dbs, errw), OwnName: ownName,
		Warn: func(m string) { fmt.Fprintf(errw, "search: warning: %s\n", m) },
	})
	if err != nil {
		if errors.Is(err, provider.ErrAuth) {
			fmt.Fprintf(errw, "search: %s (try --mode fts)\n", authLine(err, cfg.Embed.Provider, cfg.Embed.BaseURL, cfg.Embed.APIKeyEnv))
		} else if errors.Is(err, provider.ErrDown) {
			fmt.Fprintf(errw, "search: %s unreachable at %s (try --mode fts)\n", cfg.Embed.Provider, embedEndpoint(cfg.Embed))
		} else {
			fmt.Fprintf(errw, "search: %v\n", err)
		}
		return 1
	}
	if fallback {
		route = "fts-fallback"
	}
	if *asJSON {
		jh := make([]jsonHit, 0, len(hits))
		for _, h := range hits {
			jh = append(jh, jsonHit{h.SessionID, h.Profile, h.Project, h.Title, h.Score, h.Chunk, h.ChunkID, h.TS, h.Role, h.Kind})
		}
		b, err := json.MarshalIndent(map[string]any{"hits": jh, "route": route}, "", "  ")
		if err != nil {
			fmt.Fprintf(errw, "search: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, string(b))
		return 0
	}
	for _, h := range hits {
		date := h.TS
		if len(date) > 10 {
			date = date[:10]
		}
		sid := h.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		fmt.Fprintf(out, "[%.3f] %s %s %s %s — %s — %s%s\n", h.Score, date, h.Profile, sid, h.Project, h.Title, kindMarker(h.Kind), snippet(h.Chunk, 160))
	}
	fmt.Fprintf(out, "route: %s\n", route)
	return 0
}

// openSearchStore opens the own archive read-only. Only an archive that
// predates the profile columns is opened for writing, once, to migrate it.
func openSearchStore(errw io.Writer) (*store.Store, error) {
	st, err := store.OpenReadOnly(dbPath())
	if err != nil {
		if _, serr := os.Stat(dbPath()); serr != nil {
			return openStore() // no archive yet: create an empty one as before
		}
		return nil, err
	}
	f, _ := store.HasColumn(st.DB(), "files", "profile")
	s, _ := store.HasColumn(st.DB(), "sessions", "profile")
	if f && s {
		return st, nil
	}
	st.Close()
	fmt.Fprintln(errw, "search: migrating the archive to add profile columns (one time)")
	return openStore()
}

type shownMsg struct {
	recID                int64
	uuid, ts, role, text string
	summaries            []string
}

func cmdShow(args []string, out, errw io.Writer) int {
	fs := newFlags("show", errw)
	around := fs.String("around", "", "record UUID to center on")
	n := fs.Int("n", 20, "number of messages (0 = all)")
	changes := fs.Bool("changes", false, "list the session's file changes grouped by file")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(errw, "show: exactly one SESSION_ID is required")
		return 2
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	sid, err := resolveSession(ctx, st.DB(), pos[0])
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	if *changes {
		return showChanges(ctx, st.DB(), sid, out, errw)
	}
	rows, err := st.DB().QueryContext(ctx, `SELECT r.id, COALESCE(r.uuid,''), COALESCE(r.ts,''), COALESCE(m.role,''), COALESCE(m.text,'')
		FROM messages m JOIN records r ON r.id=m.record_id
		WHERE r.session_id=? AND r.superseded=0 AND m.role IN ('user','assistant')
			AND (m.text<>'' OR EXISTS (SELECT 1 FROM summaries s WHERE s.record_id=r.id AND s.text<>''))
		ORDER BY r.ts, r.id`, sid)
	if err != nil {
		fmt.Fprintf(errw, "show: %v\n", err)
		return 1
	}
	defer rows.Close()
	var msgs []shownMsg
	for rows.Next() {
		var m shownMsg
		if err := rows.Scan(&m.recID, &m.uuid, &m.ts, &m.role, &m.text); err != nil {
			fmt.Fprintf(errw, "show: %v\n", err)
			return 1
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(errw, "show: %v\n", err)
		return 1
	}
	srows, err := st.DB().QueryContext(ctx, `SELECT s.record_id, s.text FROM summaries s JOIN records r ON r.id=s.record_id
		WHERE r.session_id=? AND r.superseded=0 AND s.text<>'' ORDER BY s.record_id, s.idx`, sid)
	if err != nil {
		fmt.Fprintf(errw, "show: %v\n", err)
		return 1
	}
	defer srows.Close()
	byRec := map[int64][]string{}
	for srows.Next() {
		var id int64
		var txt string
		if err := srows.Scan(&id, &txt); err != nil {
			fmt.Fprintf(errw, "show: %v\n", err)
			return 1
		}
		byRec[id] = append(byRec[id], txt)
	}
	for i := range msgs {
		msgs[i].summaries = byRec[msgs[i].recID]
	}
	if len(msgs) == 0 {
		fmt.Fprintf(errw, "show: no messages for session %s\n", sid)
		return 1
	}
	lo, hi := 0, len(msgs)
	if *around != "" {
		idx := -1
		for i, m := range msgs {
			if m.uuid == *around {
				idx = i
				break
			}
		}
		if idx < 0 {
			fmt.Fprintf(errw, "show: uuid %s has no user/assistant text in session %s\n", *around, sid)
			return 1
		}
		if *n > 0 {
			lo = idx - *n/2
			if lo < 0 {
				lo = 0
			}
			hi = lo + *n
			if hi > len(msgs) {
				hi = len(msgs)
				if lo = hi - *n; lo < 0 {
					lo = 0
				}
			}
		}
	} else if *n > 0 && *n < len(msgs) {
		hi = *n
	}
	for _, m := range msgs[lo:hi] {
		fmt.Fprintf(out, "%s %s [%s]\n", m.ts, m.uuid, m.role)
		if m.text != "" {
			fmt.Fprintf(out, "%s\n", m.text)
		}
		for _, sm := range m.summaries {
			fmt.Fprintf(out, "  [tool output summary] %s\n", snippet(sm, 400))
		}
		fmt.Fprintln(out)
	}
	return 0
}
