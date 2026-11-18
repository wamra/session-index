package ingest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/zst"
)

// MaxAssetBytes is the largest file archived as an asset; bigger files are
// skipped whole (never partially archived).
var MaxAssetBytes int64 = 64 << 20

// asset text indexing cap.
const maxAssetFTS = 256 << 10

// Stderr receives skip notes (a var so tests can capture).
var Stderr io.Writer = os.Stderr

// AssetResult describes one asset call.
type AssetResult struct {
	Archived bool // new or changed content stored
	Skipped  bool // too large for the asset limit
	Ignored  bool // not an asset (.DS_Store, restore temp files, unclassified)
	Failed   bool // unreadable now; recorded with backoff (or still in backoff)
}

var textExts = map[string]bool{".txt": true, ".md": true, ".json": true, ".js": true}

// IsTextAsset reports whether a path is indexed as text.
func IsTextAsset(path string) bool { return textExts[strings.ToLower(filepath.Ext(path))] }

func skipName(path string) bool {
	b := filepath.Base(path)
	return b == ".DS_Store" || strings.HasPrefix(b, ".restore-")
}

// Asset archives a non-transcript file under root (latest version only).
func Asset(ctx context.Context, st *store.Store, root, path, profile string) (AssetResult, error) {
	info, ok := paths.ClassifyAsset(root, path)
	if !ok || skipName(path) {
		return AssetResult{Ignored: true}, nil
	}
	return archiveAsset(ctx, st, path, info, profile)
}

// FileHistory archives a snapshot under a profile's file-history directory.
func FileHistory(ctx context.Context, st *store.Store, dir, path, profile string) (AssetResult, error) {
	info, ok := paths.ClassifyFileHistory(dir, path)
	if !ok || skipName(path) {
		return AssetResult{Ignored: true}, nil
	}
	return archiveAsset(ctx, st, path, info, profile)
}

// ProfileDirAsset archives a file under one of the profile folders in
// paths.SweptDirs; dir is <profile>/<kind>. Besides the usual skips, the lock
// files Claude Code keeps in task lists are ignored.
func ProfileDirAsset(ctx context.Context, st *store.Store, dir, kind, path, profile string) (AssetResult, error) {
	info, ok := paths.ClassifyProfileDir(dir, kind, profile, path)
	if !ok || skipName(path) || (kind == paths.AssetTasks && filepath.Base(path) == ".lock") {
		return AssetResult{Ignored: true}, nil
	}
	return archiveAsset(ctx, st, path, info, profile)
}

func archiveAsset(ctx context.Context, st *store.Store, path string, info paths.AssetInfo, profile string) (res AssetResult, err error) {
	db := st.DB()
	var retry sql.NullString
	if db.QueryRowContext(ctx, `SELECT next_retry_at FROM assets WHERE path=?`, path).Scan(&retry) == nil && retry.Valid {
		if t, perr := time.Parse(time.RFC3339, retry.String); perr == nil && t.After(time.Now()) {
			return AssetResult{Failed: true}, nil // still in backoff after a recorded failure
		}
	}
	res, err = archiveAssetNow(ctx, st, path, info, profile)
	if err != nil && ctx.Err() == nil {
		recordAssetFailure(ctx, db, path, info, profile, err)
		return AssetResult{Failed: true}, err
	}
	return res, err
}

func assetFailKey(path string) string { return "asset_fail:" + path }

// recordAssetFailure stores the failure on the asset row (created with raw
// NULL when the file was never archived) with the same 30 s doubling to 1 h
// backoff as transcript files. Best effort.
func recordAssetFailure(ctx context.Context, db *sql.DB, path string, info paths.AssetInfo, profile string, cause error) {
	bg := context.WithoutCancel(ctx)
	n := 0
	var v string
	if db.QueryRowContext(bg, `SELECT value FROM meta WHERE key=?`, assetFailKey(path)).Scan(&v) == nil {
		n, _ = strconv.Atoi(v)
	}
	n++
	d := backoffBase
	for i := 1; i < n && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	now := time.Now().UTC()
	reason := strings.ReplaceAll(cause.Error(), path, "<path>")
	if len(reason) > 200 {
		reason = reason[:200]
	}
	db.ExecContext(bg, `INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)`, assetFailKey(path), strconv.Itoa(n))
	db.ExecContext(bg, `INSERT INTO assets(profile, session_id, path, kind, first_seen, last_seen, failed_at, fail_reason, next_retry_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET failed_at=excluded.failed_at, fail_reason=excluded.fail_reason, next_retry_at=excluded.next_retry_at`,
		nullStr(profile), nullStr(info.SessionID), path, info.Kind, now.Format(time.RFC3339), now.Format(time.RFC3339),
		now.Format(time.RFC3339), reason, now.Add(d).Format(time.RFC3339))
}

func archiveAssetNow(ctx context.Context, st *store.Store, path string, info paths.AssetInfo, profile string) (res AssetResult, err error) {
	db := st.DB()
	fi, err := os.Stat(path)
	if err != nil {
		return res, err
	}
	if !fi.Mode().IsRegular() {
		return AssetResult{Ignored: true}, nil
	}
	size, mtime, mode := fi.Size(), fi.ModTime().UnixNano(), int64(fi.Mode().Perm())
	if size > MaxAssetBytes {
		fmt.Fprintf(Stderr, "session-index: skipping %s: %d bytes exceeds the %d byte asset limit\n", path, size, MaxAssetBytes)
		return AssetResult{Skipped: true}, nil
	}
	var oSize, oMtime, oMode sql.NullInt64
	var oSha sql.NullString
	known := db.QueryRowContext(ctx, `SELECT size, mtime, mode, sha256 FROM assets WHERE path=? AND raw IS NOT NULL`, path).Scan(&oSize, &oMtime, &oMode, &oSha) == nil
	if known && oSize.Int64 == size && oMtime.Int64 == mtime && oMode.Valid && oMode.Int64 == mode {
		return res, nil // unchanged
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return res, err
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	now := time.Now().UTC().Format(time.RFC3339)
	if known && oSha.String == sha && oSize.Int64 == int64(len(data)) {
		// Same content, new mtime/mode: metadata only.
		_, err = db.ExecContext(ctx, `UPDATE assets SET mtime=?, mode=?, last_seen=?, deleted_at=NULL, failed_at=NULL, fail_reason=NULL, next_retry_at=NULL WHERE path=?`, mtime, mode, now, path)
		return res, err
	}
	raw := zst.Compress(data) // never a zero-length blob

	conn, err := db.Conn(ctx)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err = conn.ExecContext(ctx, `INSERT INTO assets(profile, session_id, path, kind, size, mtime, mode, sha256, raw, first_seen, last_seen)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET profile=COALESCE(excluded.profile, profile), session_id=excluded.session_id,
			kind=excluded.kind, size=excluded.size, mtime=excluded.mtime, mode=excluded.mode, sha256=excluded.sha256,
			raw=excluded.raw, last_seen=excluded.last_seen, deleted_at=NULL,
			failed_at=NULL, fail_reason=NULL, next_retry_at=NULL`,
		nullStr(profile), nullStr(info.SessionID), path, info.Kind, int64(len(data)), mtime, mode, sha, raw, now, now); err != nil {
		return res, err
	}
	var id int64
	if err = conn.QueryRowContext(ctx, `SELECT id FROM assets WHERE path=?`, path).Scan(&id); err != nil {
		return res, err
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM asset_fts WHERE asset_id=?`, id); err != nil {
		return res, err
	}
	if IsTextAsset(path) {
		if _, err = conn.ExecContext(ctx, `INSERT INTO asset_fts(text, asset_id) VALUES(?,?)`, truncBytes(strings.ToValidUTF8(string(data), ""), maxAssetFTS), id); err != nil {
			return res, err
		}
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, assetFailKey(path)); err != nil {
		return res, err
	}
	if info.Kind == paths.AssetToolResult && !IsTextAsset(path) {
		if err = markBinary(ctx, conn, id); err != nil {
			return res, err
		}
	}
	if info.Kind == paths.AssetMemory {
		if err = rederiveMemory(ctx, conn, id, info.SessionID, info.ProjectSlug, filepath.Base(path), profile, mtime, data); err != nil {
			return res, err
		}
	}
	if info.Kind == paths.AssetPlans && IsTextAsset(path) {
		if err = rederivePlan(ctx, conn, id, info.SessionID, path, profile, mtime, data); err != nil {
			return res, err
		}
	}
	if _, err = conn.ExecContext(ctx, `COMMIT`); err != nil {
		return res, err
	}
	committed = true
	res.Archived = true
	return res, nil
}

// SweepAssets archives every non-transcript file under root, plus the
// profile's file-history snapshots and the folders in paths.SweptDirs when
// profDir (the profile directory that holds projects/) is non-empty.
func SweepAssets(ctx context.Context, st *store.Store, root, profDir, profile string, sum *Summary) error {
	var all []string
	if err := paths.WalkAssets(root, func(p string) { all = append(all, p) }); err != nil {
		return err
	}
	each := func(p string, f func() (AssetResult, error)) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sum.Assets++
		r, err := f()
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(Stderr, "session-index: asset %s: %v\n", p, err)
			sum.AssetsFailed++
		case r.Archived:
			sum.AssetsArchived++
		case r.Skipped:
			sum.AssetsSkipped++
		case r.Ignored:
			sum.AssetsIgnored++
		case r.Failed:
			sum.AssetsFailed++
		}
		return nil
	}
	for _, p := range all {
		p := p
		if err := each(p, func() (AssetResult, error) { return Asset(ctx, st, root, p, profile) }); err != nil {
			return err
		}
	}
	if profDir != "" {
		walk := func(dir string, f func(p string) (AssetResult, error)) error {
			var list []string
			if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
				paths.WalkAssets(dir, func(p string) { list = append(list, p) })
			}
			for _, p := range list {
				p := p
				if err := each(p, func() (AssetResult, error) { return f(p) }); err != nil {
					return err
				}
			}
			return nil
		}
		fhDir := filepath.Join(profDir, paths.AssetFileHistory)
		if err := walk(fhDir, func(p string) (AssetResult, error) { return FileHistory(ctx, st, fhDir, p, profile) }); err != nil {
			return err
		}
		for _, kind := range paths.SweptDirs {
			kind, dir := kind, filepath.Join(profDir, kind)
			if err := walk(dir, func(p string) (AssetResult, error) { return ProfileDirAsset(ctx, st, dir, kind, p, profile) }); err != nil {
				return err
			}
		}
	}
	if err := backfillPlanSessions(ctx, st); err != nil {
		return err
	}
	if _, err := MarkBinaryAssets(ctx, st.DB()); err != nil {
		return err
	}
	_, err := ResolvePointers(ctx, st.DB())
	return err
}

// rederiveMemory (re)creates the synthetic session row and the chunks of one
// memory markdown asset. Old chunks of the asset are removed first.
func rederiveMemory(ctx context.Context, tx execQuerier, assetID int64, sessionID, slug, name, profile string, mtime int64, data []byte) error {
	var texts []string
	for _, c := range chunkTexts([]string{strings.ToValidUTF8(string(data), "")}) {
		texts = append(texts, name+"\n"+c)
	}
	return rederiveDoc(ctx, tx, assetID, sessionID, slug, "memory", "memory", profile, mtime, texts)
}

// rederivePlan (re)creates the synthetic session row and the chunks of one
// plan file. Every plan has its own session id (paths.DocSession of
// sessionID, the pseudo-session "plans:<profile>" its asset is stored under),
// so a search lists each matching plan; the row's title is the plan's heading,
// or the file name. Each chunk starts with the file name and, when the chunk
// does not contain it, the plan's heading, so a hit in the middle of a plan
// still says which plan it is.
func rederivePlan(ctx context.Context, tx execQuerier, assetID int64, sessionID, path, profile string, mtime int64, data []byte) error {
	name := filepath.Base(path)
	text := strings.ToValidUTF8(string(data), "")
	heading := planHeading(text)
	title := strings.TrimSpace(strings.TrimLeft(heading, "#"))
	if title == "" {
		title = name
	}
	var texts []string
	for _, c := range chunkTexts([]string{text}) {
		if heading != "" && !strings.Contains(c, heading) {
			c = heading + "\n\n" + c
		}
		texts = append(texts, name+"\n"+c)
	}
	return rederiveDoc(ctx, tx, assetID, paths.DocSession(sessionID, paths.AssetPlans, path), "", title, "plan", profile, mtime, texts)
}

// backfillPlanSessions derives the chunks of every archived plan file that
// has no session row of its own, from the stored bytes, and then drops the
// row "plans:<profile>" that all plans of a profile shared before each plan
// got its own session id.
func backfillPlanSessions(ctx context.Context, st *store.Store) error {
	db := st.DB()
	have := map[string]bool{}
	var shared []string
	rows, err := db.QueryContext(ctx, `SELECT session_id FROM sessions WHERE kind='plan'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return err
		}
		have[s] = true
		if _, rel := paths.SplitDocSession(s); rel == "" {
			shared = append(shared, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type plan struct {
		id                 int64
		sid, path, profile sql.NullString
		mtime              int64
	}
	var todo []plan
	rows, err = db.QueryContext(ctx, `SELECT id, session_id, path, profile, mtime FROM assets WHERE kind=? AND raw IS NOT NULL ORDER BY id`, paths.AssetPlans)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p plan
		if err := rows.Scan(&p.id, &p.sid, &p.path, &p.profile, &p.mtime); err != nil {
			rows.Close()
			return err
		}
		if IsTextAsset(p.path.String) && !have[paths.DocSession(p.sid.String, paths.AssetPlans, p.path.String)] {
			todo = append(todo, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(todo) == 0 && len(shared) == 0 {
		return nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	for _, p := range todo {
		var raw []byte
		if err := conn.QueryRowContext(ctx, `SELECT raw FROM assets WHERE id=?`, p.id).Scan(&raw); err != nil {
			return err
		}
		data, err := zst.Decompress(raw)
		if err != nil {
			return fmt.Errorf("plans asset %d: %w", p.id, err)
		}
		if err := rederivePlan(ctx, conn, p.id, p.sid.String, p.path.String, p.profile.String, p.mtime, data); err != nil {
			return err
		}
	}
	for _, s := range shared {
		if _, err := conn.ExecContext(ctx, `DELETE FROM sessions WHERE session_id=? AND kind='plan'
			AND NOT EXISTS (SELECT 1 FROM chunks WHERE session_id=?)`, s, s); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// maxPlanHeading bounds the heading repeated in every chunk of a plan.
const maxPlanHeading = 200

// planHeading returns the first non-blank line of a plan when it is a
// markdown heading of at most maxPlanHeading runes, else "".
func planHeading(text string) string {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "#") && utf8.RuneCountInString(l) <= maxPlanHeading {
			return l
		}
		return ""
	}
	return ""
}

// rederiveDoc replaces the chunks of a chunked asset (memory file, plan) with
// texts and upserts its synthetic session row; kind is both the session kind
// and the chunk kind.
func rederiveDoc(ctx context.Context, tx execQuerier, assetID int64, sessionID, project, title, kind, profile string, mtime int64, texts []string) error {
	recID, err := AssetRecord(ctx, tx, assetID, sessionID)
	if err != nil {
		return err
	}
	if err := deleteChunksOf(ctx, tx, recID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(session_id, project, title, kind, message_count, profile)
		VALUES(?,?,?,?,0,?)
		ON CONFLICT(session_id) DO UPDATE SET project=excluded.project, title=excluded.title, kind=excluded.kind,
			profile=COALESCE(excluded.profile, profile)`, sessionID, nullStr(project), title, kind, nullStr(profile)); err != nil {
		return err
	}
	ts := time.Unix(0, mtime).UTC().Format(time.RFC3339)
	for i, c := range texts {
		if _, err := AddChunk(ctx, tx, recID, sessionID, kind, "", ts, i, c); err != nil {
			return err
		}
	}
	return nil
}

type execQuerier interface {
	execer
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// markBinary parks a non-text tool-result asset with a failed:binary summary
// row so pending counts are honest.
func markBinary(ctx context.Context, tx execer, assetID int64) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO summaries(asset_id, text, model, created_at) VALUES(?, '', 'failed:binary', ?)`,
		assetID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// MarkBinaryAssets adds the failed:binary summary row to tool-result assets
// whose extension is not text (pdf, images) and returns how many were added.
func MarkBinaryAssets(ctx context.Context, db *sql.DB) (int, error) {
	r, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO summaries(asset_id, text, model, created_at)
		SELECT a.id, '', 'failed:binary', ? FROM assets a WHERE a.kind='tool-result' AND a.raw IS NOT NULL
			AND lower(a.path) NOT LIKE '%.txt' AND lower(a.path) NOT LIKE '%.md' AND lower(a.path) NOT LIKE '%.json' AND lower(a.path) NOT LIKE '%.js'`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// AssetRecord returns the id of the anchor record of an asset, creating it
// (and the shared anchor file row) when missing. chunks.record_id is a
// foreign key to records, so chunks that belong to an asset (memory chunks,
// summaries of tool-result files) hang off this record. The anchor file has
// no session and its records carry no raw lines.
func AssetRecord(ctx context.Context, tx execQuerier, assetID int64, sessionID string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO files(path, kind, first_seen, last_seen) VALUES(?, 'asset', ?, ?)`, assetAnchorPath, now, now); err != nil {
		return 0, err
	}
	var fid int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM files WHERE path=?`, assetAnchorPath).Scan(&fid); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO records(file_id, line_no, byte_off, byte_len, type, session_id, raw) VALUES(?,?,0,0,'asset',?,?)`,
		fid, assetID, nil, zst.Compress(nil)); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM records WHERE file_id=? AND line_no=?`, fid, assetID).Scan(&id)
	return id, err
}

const assetAnchorPath = "asset:anchor"

// deleteChunksOf removes the chunks (with vectors and FTS rows) of a record.
func deleteChunksOf(ctx context.Context, tx execer, recID int64) error {
	_, err := deleteChunksWhere(ctx, tx, fmt.Sprintf(`record_id=%d`, recID))
	return err
}

// deleteChunksWhere deletes the chunks matching a trusted SQL condition,
// together with their chunk_vec and chunk_fts rows, and returns how many.
func deleteChunksWhere(ctx context.Context, tx execer, cond string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, text FROM chunks WHERE `+cond)
	if err != nil {
		return 0, err
	}
	type ch struct {
		id   int64
		text string
	}
	var cs []ch
	for rows.Next() {
		var c ch
		if err := rows.Scan(&c.id, &c.text); err != nil {
			rows.Close()
			return 0, err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, c := range cs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vec WHERE rowid=?`, c.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_fts(chunk_fts, rowid, text) VALUES('delete', ?, ?)`, c.id, c.text); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE id=?`, c.id); err != nil {
			return 0, err
		}
	}
	return len(cs), nil
}

// AddChunk inserts a chunk row and its chunk_fts row and returns the chunk id.
// recordID is a record id (the AssetRecord for chunks that belong to an asset).
func AddChunk(ctx context.Context, tx execer, recordID int64, sessionID, kind, role, ts string, seq int, text string) (int64, error) {
	r, err := tx.ExecContext(ctx, `INSERT INTO chunks(record_id,session_id,kind,role,ts,seq,text) VALUES(?,?,?,?,?,?,?)`,
		recordID, sessionID, kind, nullStr(role), nullStr(ts), seq, text)
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO chunk_fts(rowid,text) VALUES(?,?)`, id, text)
	return id, err
}
