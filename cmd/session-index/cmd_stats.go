package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wamra/session-index/internal/config"
	"github.com/wamra/session-index/internal/hooks"
	"github.com/wamra/session-index/internal/jsonl"
	"github.com/wamra/session-index/internal/launchd"
	"github.com/wamra/session-index/internal/provider"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/summarize"
	"github.com/wamra/session-index/internal/zst"
)

func dbSize(path string) int64 {
	var n int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(path + suffix); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func scalar(ctx context.Context, db *sql.DB, q string) (string, error) {
	var s sql.NullString
	err := db.QueryRowContext(ctx, q).Scan(&s)
	return s.String, err
}

// cloudStorage reports whether the resolved path lies in a folder that a
// cloud sync client usually owns: macOS File Provider roots
// (~/Library/CloudStorage, ~/Library/Mobile Documents) or a path component
// named like a sync folder (OneDrive*, Dropbox, Google Drive, iCloud Drive).
// It is a name-based heuristic for a warning, not a guarantee either way.
func cloudStorage(path string) bool {
	p := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		p = r
	} else if r, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		p = filepath.Join(r, filepath.Base(path))
	}
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	for i, c := range parts[:max(len(parts)-1, 0)] {
		if c == "Library" && (parts[i+1] == "CloudStorage" || parts[i+1] == "Mobile Documents") {
			return true
		}
		switch l := strings.ToLower(c); {
		case strings.HasPrefix(l, "onedrive"), l == "dropbox", l == "google drive", l == "googledrive", l == "icloud drive", l == "iclouddrive":
			return true
		}
	}
	return false
}

// quickCheck runs PRAGMA quick_check and returns "ok" or the joined problems.
func quickCheck(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return "", err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return "ok", nil
	}
	return strings.Join(msgs, "; "), nil
}

func cmdStats(args []string, out, errw io.Writer) int {
	fs := newFlags("stats", errw)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	db := st.DB()
	fmt.Fprintf(out, "db: %s (%s)\n", st.Path(), humanBytes(dbSize(st.Path())))
	for _, t := range []string{"files", "records", "messages", "chunks", "chunk_vec", "chunk_fts", "tool_fts", "sessions", "assets", "file_changes", "tool_outputs", "summaries"} {
		v, err := scalar(ctx, db, `SELECT count(*) FROM `+t)
		if err != nil {
			fmt.Fprintf(errw, "stats: %s: %v\n", t, err)
			return 1
		}
		fmt.Fprintf(out, "%-10s %s\n", t, v)
	}
	if c, err := summarize.Count(ctx, db); err == nil {
		fmt.Fprintf(out, "summaries: %d done, %d failed, pending %d tool outputs + %d file changes + %d tool-result files\n",
			c.Done, c.Failed, c.PendingTool, c.PendingChange, c.PendingAsset)
	}
	oldest, err1 := scalar(ctx, db, `SELECT MIN(ts) FROM records WHERE ts IS NOT NULL AND ts<>''`)
	newest, err2 := scalar(ctx, db, `SELECT MAX(ts) FROM records WHERE ts IS NOT NULL AND ts<>''`)
	if err := errors.Join(err1, err2); err != nil {
		fmt.Fprintf(errw, "stats: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "oldest record ts: %s\nnewest record ts: %s\n", oldest, newest)
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(kind,'?'), count(*) FROM sessions GROUP BY kind ORDER BY kind`)
	if err != nil {
		fmt.Fprintf(errw, "stats: %v\n", err)
		return 1
	}
	defer rows.Close()
	fmt.Fprintln(out, "sessions by kind:")
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			fmt.Fprintf(errw, "stats: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "  %-9s %d\n", k, n)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(errw, "stats: %v\n", err)
		return 1
	}
	return 0
}

func settingsPathDefault() string { return filepath.Join(claudeDir(), "settings.json") }
func plistPathDefault() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", launchd.Label+".plist")
}

// doctorProviders reports the embed and summarize providers: configuration,
// dimensions, fingerprint and health. A bad config file is a FAILED line, not
// an abort. It returns false when something is wrong (a summarizer that is
// merely not ready is informational; a rejected key is not).
func doctorProviders(ctx context.Context, st *store.Store, fp string, out io.Writer) bool {
	ok := true
	fmt.Fprintf(out, "config: %s\n", configPath())
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(out, "config: FAILED: %v\n", err)
		return false
	}
	emb, err := newEmbedder(cfg, st)
	if err != nil {
		fmt.Fprintf(out, "config: FAILED: %v\n", err)
		return false
	}
	sum, err := newSummarizer(cfg, "")
	if err != nil {
		fmt.Fprintf(out, "config: FAILED: %v\n", err)
		return false
	}
	storedDims, _ := store.EffectiveVecDims(st.DB())
	ec, sc := cfg.Embed, cfg.Summarize
	fmt.Fprintf(out, "embed: provider=%s %s model=%s dims=%d (archive %d)\n", ec.Provider, endpointField(ec.Provider, ec.BaseURL, ec.Region, ec.Profile), ec.Model, emb.Dims(), storedDims)
	fmt.Fprintf(out, "embed api key env: %s\n", provider.KeyState(ec.APIKeyEnv))
	if ec.Dims > 0 && storedDims > 0 && ec.Dims != storedDims {
		ok = false
		fmt.Fprintf(out, "embed dims: MISMATCH %v\n", &store.DimsMismatchError{Stored: storedDims, Config: ec.Dims})
	}
	if provider.NormalizeFingerprint(fp) == emb.Fingerprint() {
		fmt.Fprintf(out, "embedder fingerprint: match (%s)\n", fp)
	} else {
		ok = false
		fmt.Fprintf(out, "embedder fingerprint: MISMATCH stored=%q expected=%q (run rebuild-derived)\n", fp, emb.Fingerprint())
	}
	switch err := emb.Health(ctx); {
	case errors.Is(err, provider.ErrAuth):
		ok = false
		fmt.Fprintf(out, "embed health: FAILED: %s\n", authLine(err, ec.Provider, ec.BaseURL, ec.APIKeyEnv))
	case err != nil:
		ok = false
		fmt.Fprintf(out, "embed health: FAILED: %s unreachable or unusable at %s (%v)\n", ec.Provider, embedEndpoint(ec), err)
	default:
		fmt.Fprintf(out, "embed health: ok (%s at %s, model %s)\n", ec.Provider, embedEndpoint(ec), ec.Model)
	}
	fmt.Fprintf(out, "summarize: provider=%s %s model=%s enabled=%v\n", sc.Provider, endpointField(sc.Provider, sc.BaseURL, sc.Region, sc.Profile), sc.Model, *sc.Enabled)
	fmt.Fprintf(out, "summarize api key env: %s\n", provider.KeyState(sc.APIKeyEnv))
	doctorUsage(ctx, st, cfg, out)
	switch err := sum.Health(ctx); {
	case errors.Is(err, provider.ErrAuth):
		if *sc.Enabled {
			ok = false
		}
		fmt.Fprintf(out, "summarize health: FAILED: %s\n", authLine(err, sc.Provider, sc.BaseURL, sc.APIKeyEnv))
	case err != nil:
		// summaries are optional: report, but do not fail the check
		fmt.Fprintf(out, "summarize health: not ready (optional): %v\n", err)
	default:
		fmt.Fprintf(out, "summarize health: ok (%s at %s, model %s)\n", sc.Provider, sumEndpoint(sc), sc.Model)
	}
	return ok
}

// doctorUsage prints today's provider usage against the daily caps.
func doctorUsage(ctx context.Context, st *store.Store, cfg config.Config, out io.Writer) {
	now := time.Now().UTC()
	for _, u := range []struct {
		name, kind string
		cap        func() (int, string)
	}{
		{"summarize", store.UsageSummarize, cfg.Summarize.DailyCap},
		{"embed", store.UsageEmbed, cfg.Embed.DailyCap},
	} {
		used, err := store.UsageToday(ctx, st.DB(), u.kind, now)
		if err != nil {
			fmt.Fprintf(out, "%s usage: query failed: %v\n", u.name, err)
			continue
		}
		limit, src := u.cap()
		lim := "unlimited"
		if limit > 0 {
			lim = fmt.Sprint(limit)
		}
		fmt.Fprintf(out, "%s usage today (UTC %s): %d / %s (%s)\n", u.name, now.Format("2006-01-02"), used, lim, src)
	}
	if k := cfg.Summarize.Kinds; len(k) > 0 {
		fmt.Fprintf(out, "summarize kinds: %s; max_input_chars: %d\n", strings.Join(k, ", "), cfg.Summarize.MaxInputChars)
	}
}

func cmdDoctor(args []string, out, errw io.Writer) int {
	fs := newFlags("doctor", errw)
	full := fs.Bool("full", false, "also run PRAGMA integrity_check (slow on large archives)")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	ok := true
	st, err := openStoreWith(dbPath(), true, io.Discard) // a size mismatch is reported below, not fatal
	if err != nil {
		doctorEncryption(dbPath(), out)
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer st.Close()
	ctx := context.Background()
	db := st.DB()
	fmt.Fprintf(out, "db: %s (%s)\n", st.Path(), humanBytes(dbSize(st.Path())))
	doctorEncryption(st.Path(), out)
	if cloudStorage(st.Path()) {
		fmt.Fprintf(out, "WARNING: database appears to be under a cloud-synced folder: sync churn and conflict risk on a live WAL database; relocate with SESSION_INDEX_DB and keep snapshots (see docs/REFERENCE.md, Database location)\n")
	}
	qc, err := quickCheck(ctx, db)
	if err != nil {
		fmt.Fprintf(errw, "doctor: quick_check: %v\n", err)
		return 1
	}
	if qc == "ok" {
		fmt.Fprintln(out, "quick_check: ok")
	} else {
		ok = false
		fmt.Fprintf(out, "quick_check: FAILED: %s\n", qc)
	}
	reopen := func() (*sql.DB, func(), error) {
		fst, err := openStoreWith(st.Path(), true, io.Discard)
		if err != nil {
			return nil, nil, err
		}
		return fst.DB(), func() { fst.Close() }, nil
	}
	if !doctorFTS(ctx, db, reopen, *full, out) {
		ok = false
	}
	if !doctorIntegrity(ctx, db, *full, out, errw) {
		ok = false
	}
	var counts [4]string
	for i, q := range []string{
		`SELECT count(*) FROM records`,
		`SELECT count(*) FROM chunks`,
		`SELECT count(*) FROM chunks WHERE embedded_at IS NULL`,
		`SELECT count(*) FROM chunks WHERE embedded_at='failed'`,
	} {
		v, err := scalar(ctx, db, q)
		if err != nil {
			fmt.Fprintf(errw, "doctor: %v\n", err)
			return 1
		}
		counts[i] = v
	}
	records, chunks, pending, failed := counts[0], counts[1], counts[2], counts[3]
	fmt.Fprintf(out, "records: %s  chunks: %s  pending: %s  failed: %s\n", records, chunks, pending, failed)

	fp, err := scalar(ctx, db, `SELECT value FROM meta WHERE key='embedder_fingerprint'`)
	if err != nil {
		fmt.Fprintf(errw, "doctor: %v\n", err)
		return 1
	}
	if !doctorProviders(ctx, st, fp, out) {
		ok = false
	}

	sp := settingsPathDefault()
	if hooks.HasHook(sp) {
		fmt.Fprintf(out, "hooks: present in %s\n", sp)
	} else {
		ok = false
		fmt.Fprintf(out, "hooks: NOT installed in %s\n", sp)
	}
	pp := plistPathDefault()
	if _, err := os.Stat(pp); err == nil {
		fmt.Fprintf(out, "launchd: plist present at %s (optional)\n", pp)
	} else {
		fmt.Fprintf(out, "launchd: not installed (optional; catch-up is manual: session-index sweep --embed)\n")
	}
	if !ok {
		return 1
	}
	return 0
}

// doctorFTS integrity-checks every FTS5 table, and with full also runs PRAGMA
// integrity_check, on one connection of db. A failure is re-judged once on a
// brand-new connection pool from reopen: a long-lived connection can hold a
// stale FTS5 view while another process writes (ncruces WASM build: PRAGMA
// integrity_check reports "malformed inverted index" on such a connection
// until an FTS command refreshes it, while a fresh connection reports ok, see
// internal/store/fts_integrity_test.go), so nothing is reported as FAILED
// before a fresh pool agrees. The FTS 'integrity-check' command is an INSERT,
// so the fresh pool is a normal writable one (store.OpenWith), not read-only.
func doctorFTS(ctx context.Context, db *sql.DB, reopen func() (*sql.DB, func(), error), full bool, out io.Writer) bool {
	check := func(db *sql.DB) (lines []string, failed bool, err error) {
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, false, err
		}
		defer conn.Close()
		// PRAGMA integrity_check runs FIRST on the connection: it must not have
		// cached any FTS5 state yet (the FTS commands below would cache it, and a
		// writer committing in between makes the PRAGMA see a stale view).
		var pragmaLine string
		if full {
			res, e := integrityCheckConn(ctx, conn)
			switch {
			case e != nil:
				failed = true
				pragmaLine = fmt.Sprintf("integrity_check: FAILED: %v (run `session-index repair`)", e)
			case res == "ok":
				pragmaLine = "integrity_check: ok"
			default:
				failed = true
				pragmaLine = fmt.Sprintf("integrity_check: FAILED: %s (run `session-index repair`)", res)
			}
		}
		tables, err := store.FTSTables(ctx, conn)
		if err != nil {
			return nil, false, err
		}
		for _, tbl := range tables {
			if e := store.FTSCommand(ctx, conn, tbl, "integrity-check"); e != nil {
				failed = true
				lines = append(lines, fmt.Sprintf("%s integrity-check: FAILED: %v (run `session-index repair`)", tbl, e))
			} else {
				lines = append(lines, fmt.Sprintf("%s integrity-check: ok", tbl))
			}
		}
		if full {
			lines = append(lines, pragmaLine)
		}
		return lines, failed, nil
	}
	lines, failed, err := check(db)
	if err == nil && failed {
		fdb, closeFn, rerr := reopen()
		if rerr == nil {
			l2, f2, err2 := check(fdb)
			closeFn()
			if err2 == nil {
				lines, failed = l2, f2
			}
		}
	}
	if err != nil {
		fmt.Fprintf(out, "fts integrity: FAILED: %v\n", err)
		return false
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	return !failed
}

// integrityCheckConn is integrityCheck on one connection.
func integrityCheckConn(ctx context.Context, conn *sql.Conn) (string, error) {
	rows, err := conn.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return "", err
		}
		if len(lines) < 5 {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "; "), rows.Err()
}

// doctorIntegrity reports assets and summaries, runs PRAGMA foreign_key_check
// and the pointer / orphan checks; with full it also sample-verifies stored
// records and assets. It returns false when a problem was found (unresolved
// pointers are listed but are not a failure: they are expected for files that
// were deleted before they could be archived).
func doctorIntegrity(ctx context.Context, db *sql.DB, full bool, out, errw io.Writer) bool {
	good := true
	one := func(q string) int {
		var n int
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			fmt.Fprintf(out, "integrity query failed: %v\n", err)
			good = false
		}
		return n
	}
	var bytes int64
	db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM assets WHERE raw IS NOT NULL`).Scan(&bytes)
	fmt.Fprintf(out, "assets: %d (%s)  file_changes: %d  tool_outputs: %d\n", one(`SELECT count(*) FROM assets WHERE raw IS NOT NULL`), humanBytes(bytes),
		one(`SELECT count(*) FROM file_changes`), one(`SELECT count(*) FROM tool_outputs`))
	if c, err := summarize.Count(ctx, db); err == nil {
		fmt.Fprintf(out, "summaries: %d done, %d failed, pending: %d tool outputs, %d file changes, %d tool-result files\n",
			c.Done, c.Failed, c.PendingTool, c.PendingChange, c.PendingAsset)
	}
	if n := one(`SELECT count(*) FROM pragma_foreign_key_check`); n == 0 {
		fmt.Fprintln(out, "foreign_key_check: 0 violations")
	} else {
		good = false
		fmt.Fprintf(out, "foreign_key_check: %d VIOLATIONS\n", n)
		if rows, err := db.QueryContext(ctx, `SELECT "table", COALESCE(rowid,0), "parent" FROM pragma_foreign_key_check LIMIT 5`); err == nil {
			for rows.Next() {
				var t, p string
				var r int64
				rows.Scan(&t, &r, &p)
				fmt.Fprintf(out, "  %s rowid %d -> missing %s\n", t, r, p)
			}
			rows.Close()
		}
	}
	if nf := one(`SELECT count(*) FROM assets WHERE failed_at IS NOT NULL`); nf > 0 {
		fmt.Fprintf(out, "failed assets: %d (retried with backoff on the next sweep)\n", nf)
		if rows, err := db.QueryContext(ctx, `SELECT path, COALESCE(fail_reason,''), COALESCE(failed_at,'') FROM assets WHERE failed_at IS NOT NULL ORDER BY path LIMIT 20`); err == nil {
			for rows.Next() {
				var p, why, at string
				rows.Scan(&p, &why, &at)
				fmt.Fprintf(out, "  failed asset %s: %s (%s)\n", p, why, at)
			}
			rows.Close()
		}
	}
	ptrs := one(`SELECT count(*) FROM tool_outputs WHERE saved_path IS NOT NULL`)
	unres := one(`SELECT count(*) FROM tool_outputs WHERE saved_path IS NOT NULL AND asset_id IS NULL`)
	fmt.Fprintf(out, "tool-result pointers: %d, resolved to an archived asset: %d, unresolved: %d\n", ptrs, ptrs-unres, unres)
	if unres > 0 {
		if rows, err := db.QueryContext(ctx, `SELECT saved_path, count(*) FROM tool_outputs WHERE saved_path IS NOT NULL AND asset_id IS NULL GROUP BY saved_path ORDER BY saved_path LIMIT 20`); err == nil {
			for rows.Next() {
				var p string
				var n int
				rows.Scan(&p, &n)
				fmt.Fprintf(out, "  unresolved (file not archived; expected only if deleted before archiving): %s (x%d)\n", p, n)
			}
			rows.Close()
		}
	}
	if n := one(`SELECT count(*) FROM summaries WHERE record_id IS NULL AND asset_id IS NULL AND change_id IS NULL`); n > 0 {
		good = false
		fmt.Fprintf(out, "summaries without a target: %d (run repair)\n", n)
	}
	if n := one(`SELECT count(*) FROM chunks c WHERE c.kind='summary'
		AND NOT EXISTS (SELECT 1 FROM summaries s WHERE s.record_id=c.record_id)
		AND NOT EXISTS (SELECT 1 FROM summaries s JOIN records r ON r.line_no=s.asset_id JOIN files f ON f.id=r.file_id AND f.path='asset:anchor' WHERE r.id=c.record_id)`); n > 0 {
		good = false
		fmt.Fprintf(out, "summary chunks without a source summary: %d (run repair)\n", n)
	}
	if n := one(`SELECT count(*) FROM chunks c WHERE c.kind='filechange'
		AND NOT EXISTS (SELECT 1 FROM summaries s JOIN file_changes fc ON fc.id=s.change_id WHERE fc.record_id=c.record_id)`); n > 0 {
		good = false
		fmt.Fprintf(out, "filechange chunks without a source change summary: %d (run repair)\n", n)
	}
	if full {
		if !sampleVerify(ctx, db, out) {
			good = false
		}
	}
	return good
}

// sampleVerify decompresses 100 random live records (byte_len) and 20 random
// assets (size and sha256) and compares them with the stored values. It also
// compares the tool_outputs count with the tool_result blocks parsed from every
// live record.
func sampleVerify(ctx context.Context, db *sql.DB, out io.Writer) bool {
	good := true
	bad := 0
	rows, err := db.QueryContext(ctx, `SELECT raw, byte_len FROM records WHERE superseded=0 AND type<>'asset' ORDER BY random() LIMIT 100`)
	if err != nil {
		fmt.Fprintf(out, "sample verification: %v\n", err)
		return false
	}
	n := 0
	for rows.Next() {
		var raw []byte
		var bl int64
		if err := rows.Scan(&raw, &bl); err != nil {
			bad++
			continue
		}
		n++
		if line, err := zst.Decompress(raw); err != nil || int64(len(line))+1 != bl {
			bad++
		}
	}
	rows.Close()
	fmt.Fprintf(out, "sample verification: %d records decompressed, %d byte_len mismatches\n", n, bad)
	if bad > 0 {
		good = false
	}
	abad, an := 0, 0
	arows, err := db.QueryContext(ctx, `SELECT raw, size, sha256 FROM assets WHERE raw IS NOT NULL ORDER BY random() LIMIT 20`)
	if err != nil {
		fmt.Fprintf(out, "sample verification: %v\n", err)
		return false
	}
	for arows.Next() {
		var raw []byte
		var size int64
		var sha string
		if err := arows.Scan(&raw, &size, &sha); err != nil {
			abad++
			continue
		}
		an++
		data, err := zst.Decompress(raw)
		sum := sha256.Sum256(data)
		if err != nil || int64(len(data)) != size || hex.EncodeToString(sum[:]) != sha {
			abad++
		}
	}
	arows.Close()
	fmt.Fprintf(out, "sample verification: %d assets decompressed, %d sha256/size mismatches\n", an, abad)
	if abad > 0 {
		good = false
	}
	// tool_outputs must account for every tool_result block of every live record.
	rrows, err := db.QueryContext(ctx, `SELECT raw FROM records WHERE superseded=0 AND type<>'asset' AND byte_len>0`)
	if err != nil {
		fmt.Fprintf(out, "tool_outputs coverage: %v\n", err)
		return false
	}
	blocks := 0
	for rrows.Next() {
		var raw []byte
		if rrows.Scan(&raw) != nil {
			continue
		}
		line, err := zst.Decompress(raw)
		if err != nil {
			continue
		}
		if rec, err := jsonl.Parse(line); err == nil && rec.Msg != nil {
			blocks += len(rec.Msg.ToolResults)
		}
	}
	rrows.Close()
	var outputs int
	db.QueryRowContext(ctx, `SELECT count(*) FROM tool_outputs`).Scan(&outputs)
	if blocks == outputs {
		fmt.Fprintf(out, "tool_outputs coverage: %d rows = %d tool_result blocks in live records\n", outputs, blocks)
	} else {
		good = false
		fmt.Fprintf(out, "tool_outputs coverage: MISMATCH %d rows vs %d tool_result blocks (run rebuild-derived)\n", outputs, blocks)
	}
	return good
}
