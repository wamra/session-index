// Package ingest copies Claude Code transcript JSONL files into the archive
// database losslessly (compressed raw line per physical line) and derives the
// parsed rows (messages, chunks, tool_fts, sessions) from them.
package ingest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wamra/session-index/internal/jsonl"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/zst"
)

// maxLine is the per-line cap (a var so tests can inject a small one). A longer
// line fails the file's ingest (rollback, backoff) rather than being truncated.
var maxLine = 256 << 20

const (
	prefixBytes   = 4096
	maxChunkRunes = 2000
	minChunkRunes = 20
	maxToolBytes  = 8192
	backoffBase   = 30 * time.Second
	backoffMax    = time.Hour
)

// Result describes one File call.
type Result struct {
	NewLines, NewChunks int
	Reset               bool
}

// Summary describes one Sweep call.
type Summary struct {
	Files, NewLines, NewChunks, Skipped, Failed int
	// Assets counts non-transcript files seen; AssetsArchived the new or
	// changed ones stored, AssetsSkipped those too large to archive,
	// AssetsIgnored .DS_Store and similar, AssetsFailed unreadable ones.
	Assets, AssetsArchived, AssetsSkipped, AssetsIgnored, AssetsFailed int
}

// File ingests the new complete lines of path (a transcript under root) for
// the given profile (empty = unknown, stored as NULL). Failures are recorded
// as per-file backoff (files.next_retry_at).
func File(ctx context.Context, st *store.Store, root, path, profile string) (Result, error) {
	info, ok := paths.Classify(root, path)
	if !ok {
		return Result{}, fmt.Errorf("ingest: %s is not a recognised transcript path under %s", path, root)
	}
	info.Profile = profile
	res, err := ingestFile(ctx, st.DB(), info, path)
	if err != nil {
		if ctx.Err() == nil {
			recordFailure(st.DB(), path)
		}
		return Result{}, fmt.Errorf("ingest %s: %w", path, err)
	}
	return res, nil
}

// Sweep walks root and ingests every recognised transcript for the given
// profile, skipping files in failure backoff.
func Sweep(ctx context.Context, st *store.Store, root, profile string) (Summary, error) {
	if _, err := BackfillExtras(ctx, st); err != nil {
		return Summary{}, fmt.Errorf("backfill file changes and tool outputs: %w", err)
	}
	var all []string
	if err := paths.Walk(root, func(p string) { all = append(all, p) }); err != nil {
		return Summary{}, err
	}
	var sum Summary
	now := time.Now()
	for _, p := range all {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if _, ok := paths.Classify(root, p); !ok {
			sum.Skipped++
			continue
		}
		if inBackoff(ctx, st.DB(), p, now) {
			sum.Skipped++
			continue
		}
		res, err := File(ctx, st, root, p, profile)
		if err != nil {
			if ctx.Err() != nil {
				return sum, ctx.Err()
			}
			sum.Failed++
			continue
		}
		sum.Files++
		sum.NewLines += res.NewLines
		sum.NewChunks += res.NewChunks
	}
	// <root> is <profile>/projects: file-history snapshots sit next to it.
	if err := SweepAssets(ctx, st, root, filepath.Join(filepath.Dir(filepath.Clean(root)), "file-history"), profile, &sum); err != nil {
		return sum, err
	}
	return sum, nil
}

func inBackoff(ctx context.Context, db *sql.DB, path string, now time.Time) bool {
	var s sql.NullString
	if db.QueryRowContext(ctx, `SELECT next_retry_at FROM files WHERE path=?`, path).Scan(&s) != nil || !s.Valid {
		return false
	}
	t, err := time.Parse(time.RFC3339, s.String)
	return err == nil && t.After(now)
}

func failKey(path string) string { return "ingest_fail:" + path }

// recordFailure bumps the consecutive-failure count and sets next_retry_at to
// 30 s doubling per failure, capped at 1 h. Best effort.
func recordFailure(db *sql.DB, path string) {
	ctx := context.Background()
	n := 0
	var v string
	if db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, failKey(path)).Scan(&v) == nil {
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
	retry := time.Now().Add(d).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)`, failKey(path), strconv.Itoa(n))
	db.ExecContext(ctx, `INSERT INTO files(path, first_seen, last_seen, next_retry_at) VALUES(?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET next_retry_at=excluded.next_retry_at`, path, now, now, retry)
}

// beforeTxn is a test hook called between the fast-path stat and BEGIN IMMEDIATE.
var beforeTxn func()

type session struct {
	cwd, branch, title, customTitle string
}

func ingestFile(ctx context.Context, db *sql.DB, info paths.Info, path string) (res Result, err error) {
	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return res, err
	}
	size, mtime := fi.Size(), fi.ModTime().UnixNano()

	// Fast path: nothing changed since the last complete ingest.
	var fOff, fSize, fMtime, fMode sql.NullInt64
	if db.QueryRowContext(ctx, `SELECT "offset", size, mtime, mode FROM files WHERE path=?`, path).Scan(&fOff, &fSize, &fMtime, &fMode) == nil &&
		fOff.Int64 == size && fSize.Int64 == size && fMtime.Int64 == mtime && size > 0 {
		var nr sql.NullString
		db.QueryRowContext(ctx, `SELECT next_retry_at FROM files WHERE path=?`, path).Scan(&nr)
		if !nr.Valid {
			if !fMode.Valid { // rows archived before the mode column existed
				db.ExecContext(ctx, `UPDATE files SET mode=? WHERE path=?`, int64(fi.Mode().Perm()), path)
			}
			return res, nil
		}
	}

	if beforeTxn != nil {
		beforeTxn()
	}
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
	tx := conn // *sql.Conn supports ExecContext/QueryRowContext/PrepareContext

	// The stat above only feeds the fast path: a concurrent ingest may have
	// advanced the offset since. Decide Reset/append from a stat taken while
	// holding the write lock.
	if fi, err = f.Stat(); err != nil {
		return res, err
	}
	size, mtime = fi.Size(), fi.ModTime().UnixNano()

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err = tx.ExecContext(ctx, `INSERT INTO files(path, session_id, kind, agent_id, first_seen, last_seen, profile)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET session_id=excluded.session_id, kind=excluded.kind,
			agent_id=excluded.agent_id, last_seen=excluded.last_seen,
			profile=COALESCE(excluded.profile, profile)`,
		path, info.SessionID, info.Kind, nullStr(info.AgentID), now, now, nullStr(info.Profile)); err != nil {
		return res, err
	}
	var fileID, offset int64
	var sha sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT id, "offset", sha256_prefix FROM files WHERE path=?`, path).Scan(&fileID, &offset, &sha); err != nil {
		return res, err
	}

	// Truncation or rewrite: re-ingest from zero, superseding old rows.
	if offset > 0 {
		reset := size < offset
		if !reset {
			h, herr := prefixHash(f, offset)
			if herr != nil {
				return res, herr
			}
			reset = h != sha.String
		}
		if reset {
			if err = supersede(ctx, tx, fileID); err != nil {
				return res, err
			}
			offset = 0
			res.Reset = true
		}
	}

	var lineNo int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(line_no),0) FROM records WHERE file_id=?`, fileID).Scan(&lineNo); err != nil {
		return res, err
	}

	stmts, err := prepare(ctx, tx)
	if err != nil {
		return res, err
	}
	defer stmts.close()

	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return res, err
	}
	rd := bufio.NewReaderSize(f, 1<<20)
	var sess session
	for {
		if res.NewLines%1000 == 0 {
			if err = ctx.Err(); err != nil {
				return res, err
			}
		}
		line, total, complete, rerr := readLine(rd)
		if rerr != nil && rerr != io.EOF {
			return res, rerr
		}
		if !complete {
			break // EOF; partial trailing line (if any) is picked up next time
		}
		lineNo++
		nc, perr := stmts.insertLine(ctx, info, fileID, lineNo, offset, int64(total), line, &sess)
		if perr != nil {
			return res, fmt.Errorf("line %d: %w", lineNo, perr)
		}
		res.NewLines++
		res.NewChunks += nc
		offset += int64(total)
	}

	h, err := prefixHash(f, offset)
	if err != nil {
		return res, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE files SET "offset"=?, size=?, mtime=?, sha256_prefix=?, last_seen=?,
		next_retry_at=NULL, deleted_at=NULL, mode=? WHERE id=?`, offset, size, mtime, h, now, int64(fi.Mode().Perm()), fileID); err != nil {
		return res, err
	}
	if res.NewLines > 0 || res.Reset {
		if err = upsertSession(ctx, tx, info, sess); err != nil {
			return res, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key=?`, failKey(path)); err != nil {
		return res, err
	}
	if _, err = tx.ExecContext(ctx, `COMMIT`); err != nil {
		return res, err
	}
	committed = true
	return res, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// prefixHash is the hex sha256 of the first min(4096, n) bytes of f.
func prefixHash(f *os.File, n int64) (string, error) {
	if n > prefixBytes {
		n = prefixBytes
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, n), buf); err != nil {
		return "", err
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// supersede marks the file's live records superseded (never deleted), frees
// their line_no (negated record id) so the file can be re-ingested, and drops
// their derived chunks and tool_fts rows.
func supersede(ctx context.Context, tx execer, fileID int64) error {
	live := `SELECT id FROM records WHERE file_id=? AND superseded=0`
	rows, err := tx.QueryContext(ctx, `SELECT id, text FROM chunks WHERE record_id IN (`+live+`)`, fileID)
	if err != nil {
		return err
	}
	type ch struct {
		id   int64
		text string
	}
	var chunks []ch
	for rows.Next() {
		var c ch
		if err := rows.Scan(&c.id, &c.text); err != nil {
			rows.Close()
			return err
		}
		chunks = append(chunks, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range chunks {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_vec WHERE rowid=?`, c.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_fts(chunk_fts, rowid, text) VALUES('delete', ?, ?)`, c.id, c.text); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DELETE FROM tool_outputs WHERE record_id IN (` + live + `)`,
		`DELETE FROM file_fts WHERE change_id IN (SELECT id FROM file_changes WHERE record_id IN (` + live + `))`,
		`DELETE FROM summaries WHERE change_id IN (SELECT id FROM file_changes WHERE record_id IN (` + live + `))`,
		`DELETE FROM summaries WHERE record_id IN (` + live + `)`,
		`DELETE FROM file_changes WHERE record_id IN (` + live + `)`,
		`DELETE FROM chunks WHERE record_id IN (` + live + `)`,
		`DELETE FROM tool_fts WHERE record_id IN (` + live + `)`,
		`DELETE FROM messages WHERE record_id IN (` + live + `)`,
	} {
		if _, err := tx.ExecContext(ctx, q, fileID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE records SET superseded=1, line_no=-id WHERE file_id=? AND superseded=0`, fileID)
	return err
}

// readLine reads one physical line. total is the byte length including the
// newline. complete is false at EOF without a trailing newline (partial line)
// or at clean EOF. line excludes the newline; a line over maxLine yields
// errOversize.
func readLine(rd *bufio.Reader) (line []byte, total int, complete bool, err error) {
	var buf []byte
	for {
		chunk, e := rd.ReadSlice('\n')
		total += len(chunk)
		if len(buf)+len(chunk) > maxLine {
			return nil, total, false, errOversize
		}
		buf = append(buf, chunk...)
		switch e {
		case nil:
			return buf[:len(buf)-1], total, true, nil
		case bufio.ErrBufferFull:
			continue
		default:
			return nil, total, false, e
		}
	}
}

var errOversize = errors.New("oversize line")

type stmts struct {
	rec, msg, chunk, cfts, tfts, fc, ffts, fcres, tout *sql.Stmt
}

func prepare(ctx context.Context, c *sql.Conn) (*stmts, error) {
	s := &stmts{}
	var err error
	defs := []struct {
		dst **sql.Stmt
		q   string
	}{
		{&s.rec, `INSERT INTO records(file_id,line_no,byte_off,byte_len,uuid,type,subtype,session_id,parent_uuid,ts,raw) VALUES(?,?,?,?,?,?,?,?,?,?,?)`},
		{&s.msg, `INSERT INTO messages(record_id,role,model,text,text_len,has_thinking,tool_names,cost_usd,input_tokens,output_tokens) VALUES(?,?,?,?,?,?,?,?,?,?)`},
		{&s.chunk, `INSERT INTO chunks(record_id,session_id,kind,role,ts,seq,text) VALUES(?,?,?,?,?,?,?)`},
		{&s.cfts, `INSERT INTO chunk_fts(rowid,text) VALUES(?,?)`},
		{&s.tfts, `INSERT INTO tool_fts(text,record_id) VALUES(?,?)`},
		{&s.fc, `INSERT INTO file_changes(record_id,tool_use_id,session_id,profile,ts,tool,file_path,op,added_chars,removed_chars,excerpt) VALUES(?,?,?,?,?,?,?,?,?,?,?)`},
		{&s.tout, `INSERT INTO tool_outputs(record_id,idx,tool_use_id,inline_bytes,saved_path) VALUES(?,?,?,?,?)`},
		{&s.ffts, `INSERT INTO file_fts(file_path,change_id) VALUES(?,?)`},
		{&s.fcres, `UPDATE file_changes SET result_record_id=?, result_ok=? WHERE tool_use_id=? AND session_id=? AND result_record_id IS NULL`},
	}
	for _, d := range defs {
		if *d.dst, err = c.PrepareContext(ctx, d.q); err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

func (s *stmts) close() {
	for _, st := range []*sql.Stmt{s.rec, s.msg, s.chunk, s.cfts, s.tfts, s.fc, s.ffts, s.fcres, s.tout} {
		if st != nil {
			st.Close()
		}
	}
}

// lineExtras holds top-level fields jsonl.Record does not expose.
type lineExtras struct {
	Cwd       string          `json:"cwd"`
	GitBranch string          `json:"gitBranch"`
	Message   json.RawMessage `json:"message"`
}

type usageProbe struct {
	Usage struct {
		ServiceTier string `json:"service_tier"`
	} `json:"usage"`
}

func probe(line []byte, hasMsg bool) (cwd, branch string, hasUsage bool) {
	var e lineExtras
	_ = json.Unmarshal(line, &e) // type errors still fill what decoded
	if hasMsg && len(e.Message) > 0 && e.Message[0] == '{' {
		var u usageProbe
		_ = json.Unmarshal(e.Message, &u)
		hasUsage = u.Usage.ServiceTier != ""
	}
	return e.Cwd, e.GitBranch, hasUsage
}

func (s *stmts) insertLine(ctx context.Context, info paths.Info, fileID, lineNo, off, total int64, line []byte, sess *session) (int, error) {
	var rec jsonl.Record
	var typ string
	var raw []byte
	var cwd, branch string
	hasUsage := false
	switch {
	case len(line) == 0:
		typ = "<blank>"
		// Never store a zero-length blob: ncruces v0.21.3 fails to scan one
		// ("another row available"). A zstd frame of nothing decompresses to "".
		raw = zst.Compress(nil)
	default:
		raw = zst.Compress(line)
		var perr error
		rec, perr = jsonl.Parse(line)
		typ = rec.Type
		if perr == nil {
			cwd, branch, hasUsage = probe(line, rec.Msg != nil)
		}
	}
	r, err := s.rec.ExecContext(ctx, fileID, lineNo, off, total, nullStr(rec.UUID), typ, nullStr(rec.Subtype),
		info.SessionID, nullStr(rec.ParentUUID), nullStr(rec.Timestamp), raw)
	if err != nil {
		return 0, err
	}
	if cwd != "" {
		sess.cwd = cwd
	}
	if branch != "" {
		sess.branch = branch
	}
	if rec.AITitle != "" {
		sess.title = rec.AITitle
	}
	if rec.CustomTitle != "" {
		sess.customTitle = rec.CustomTitle
	}
	if rec.Msg == nil {
		return 0, nil
	}
	recID, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return s.derive(ctx, info, recID, rec, hasUsage)
}

// derive writes the derived rows (tool_fts, messages, chunks, chunk_fts) for
// one stored record whose parsed message is non-nil. Shared by insertLine and
// Rederive.
func (s *stmts) derive(ctx context.Context, info paths.Info, recID int64, rec jsonl.Record, hasUsage bool) (int, error) {
	m := rec.Msg
	for _, tu := range m.ToolUses {
		if err := s.insertTool(ctx, tu.Name+" "+string(tu.InputJSON), recID); err != nil {
			return 0, err
		}
	}
	if err := s.deriveExtras(ctx, info, recID, rec); err != nil {
		return 0, err
	}
	for _, tr := range m.ToolResults {
		if tr == "" {
			continue
		}
		if err := s.insertTool(ctx, tr, recID); err != nil {
			return 0, err
		}
	}
	if rec.Type != "user" && rec.Type != "assistant" {
		return 0, nil
	}
	text := strings.Join(m.Texts, "\n\n")
	names := make([]string, 0, len(m.ToolUses))
	for _, tu := range m.ToolUses {
		names = append(names, tu.Name)
	}
	in, out, cost := 0, 0, 0.0
	if hasUsage { // several lines per API response share usage; count one
		in, out, cost = m.Usage.Input, m.Usage.Output, m.CostUSD
	}
	role := m.Role
	if role == "" {
		role = rec.Type
	}
	if _, err := s.msg.ExecContext(ctx, recID, role, nullStr(m.Model), text, utf8.RuneCountInString(text),
		boolInt(m.HasThinking), strings.Join(names, ","), cost, in, out); err != nil {
		return 0, err
	}
	n := 0
	for i, c := range chunkTexts(m.Texts) {
		cr, err := s.chunk.ExecContext(ctx, recID, info.SessionID, info.Kind, role, nullStr(rec.Timestamp), i, c)
		if err != nil {
			return n, err
		}
		cid, err := cr.LastInsertId()
		if err != nil {
			return n, err
		}
		if _, err := s.cfts.ExecContext(ctx, cid, c); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// deriveExtras writes the file_changes, file_fts and tool_outputs rows of one
// stored record and links tool results to the file changes they answer.
func (s *stmts) deriveExtras(ctx context.Context, info paths.Info, recID int64, rec jsonl.Record) error {
	m := rec.Msg
	for _, tu := range m.ToolUses {
		for _, fc := range ExtractFileChanges(tu.Name, tu.InputJSON) {
			r, err := s.fc.ExecContext(ctx, recID, nullStr(tu.ID), nullStr(info.SessionID), nullStr(info.Profile), nullStr(rec.Timestamp),
				fc.Tool, fc.FilePath, fc.Op, fc.AddedChars, fc.RemovedCh, fc.Excerpt)
			if err != nil {
				return err
			}
			cid, err := r.LastInsertId()
			if err != nil {
				return err
			}
			if _, err := s.ffts.ExecContext(ctx, fc.FilePath, cid); err != nil {
				return err
			}
		}
	}

	for i, tr := range m.ToolResults {
		var tuid any
		if i < len(m.ResultRefs) {
			tuid = nullStr(m.ResultRefs[i].ToolUseID)
		}
		if _, err := s.tout.ExecContext(ctx, recID, i, tuid, len(tr), nullStr(SavedPath(tr))); err != nil {
			return err
		}
	}
	for _, ref := range m.ResultRefs {
		if ref.ToolUseID == "" {
			continue
		}
		if _, err := s.fcres.ExecContext(ctx, recID, boolInt(!ref.IsError), ref.ToolUseID, info.SessionID); err != nil {
			return err
		}
	}
	return nil
}

func (s *stmts) insertTool(ctx context.Context, text string, recID int64) error {
	_, err := s.tfts.ExecContext(ctx, truncBytes(text, maxToolBytes), recID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// truncBytes cuts s to at most n bytes on a rune boundary.
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func upsertSession(ctx context.Context, tx *sql.Conn, info paths.Info, s session) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(session_id, project, cwd, git_branch, title, custom_title, kind, message_count, profile)
		VALUES(?,?,?,?,?,?,?,0,?)
		ON CONFLICT(session_id) DO UPDATE SET
			project=COALESCE(project, excluded.project),
			profile=COALESCE(excluded.profile, profile),
			cwd=COALESCE(excluded.cwd, cwd),
			git_branch=COALESCE(excluded.git_branch, git_branch),
			title=COALESCE(excluded.title, title),
			custom_title=COALESCE(excluded.custom_title, custom_title),
			kind=CASE WHEN excluded.kind='main' THEN 'main' ELSE COALESCE(kind, excluded.kind) END`,
		info.SessionID, info.ProjectSlug, nullStr(s.cwd), nullStr(s.branch), nullStr(s.title), nullStr(s.customTitle), info.Kind, nullStr(info.Profile)); err != nil {
		return err
	}
	return refreshSession(ctx, tx, info.SessionID)
}

// refreshSession recomputes started, ended and message_count of a session
// from its non-superseded records.
func refreshSession(ctx context.Context, tx *sql.Conn, sessionID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET
		started=(SELECT MIN(ts) FROM records WHERE session_id=sessions.session_id AND superseded=0 AND type IN ('user','assistant') AND ts IS NOT NULL AND ts<>''),
		ended=(SELECT MAX(ts) FROM records WHERE session_id=sessions.session_id AND superseded=0 AND type IN ('user','assistant') AND ts IS NOT NULL AND ts<>''),
		message_count=(SELECT COUNT(*) FROM messages m JOIN records r ON r.id=m.record_id
			WHERE r.session_id=sessions.session_id AND r.superseded=0)
		WHERE session_id=?`, sessionID)
	return err
}

// chunkTexts splits each text on blank-line boundaries, packs paragraphs into
// pieces of at most 2000 characters (hard-splitting longer paragraphs), and
// drops pieces shorter than 20 characters.
func chunkTexts(texts []string) []string {
	var out []string
	emit := func(p string) {
		if utf8.RuneCountInString(strings.TrimSpace(p)) >= minChunkRunes {
			out = append(out, p)
		}
	}
	for _, t := range texts {
		cur := ""
		curLen := 0
		flush := func() {
			if curLen > 0 {
				emit(cur)
			}
			cur, curLen = "", 0
		}
		for _, para := range strings.Split(t, "\n\n") {
			rs := []rune(para)
			if len(rs) == 0 {
				continue
			}
			if len(rs) > maxChunkRunes {
				flush()
				for len(rs) > 0 {
					n := min(maxChunkRunes, len(rs))
					emit(string(rs[:n]))
					rs = rs[n:]
				}
				continue
			}
			if curLen > 0 && curLen+2+len(rs) > maxChunkRunes {
				flush()
			}
			if curLen > 0 {
				cur += "\n\n"
				curLen += 2
			}
			cur += para
			curLen += len(rs)
		}
		flush()
	}
	return out
}
