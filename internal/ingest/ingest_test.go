package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/zst"
)

const sid = "11111111-1111-1111-1111-111111111111"

func setup(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, t.TempDir()
}

func mainPath(root string) string { return filepath.Join(root, "proj", sid+".jsonl") }

func write(t *testing.T, path, content string, appendMode bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func userLine(n int, text string) string {
	return fmt.Sprintf(`{"type":"user","uuid":"u%d","sessionId":"%s","timestamp":"2026-01-01T00:00:%02dZ","cwd":"/work/x","gitBranch":"main","message":{"role":"user","content":%q}}`, n, sid, n%60, text) + "\n"
}

func asstLine(n int, text string) string {
	return fmt.Sprintf(`{"type":"assistant","uuid":"a%d","sessionId":"%s","timestamp":"2026-01-01T00:01:%02dZ","costUSD":0.5,"message":{"role":"assistant","model":"m1","usage":{"input_tokens":10,"output_tokens":20,"service_tier":"standard"},"content":[{"type":"text","text":%q},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo zebrafish"}}]}}`, n, sid, n%60, text) + "\n"
}

func count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const longText = "This is a sufficiently long paragraph of synthetic text for chunking."

func TestIngestTwiceIsIdempotent(t *testing.T) {
	st, root := setup(t)
	write(t, mainPath(root), userLine(1, longText)+asstLine(2, longText), false)
	r1, err := File(context.Background(), st, root, mainPath(root), "")
	if err != nil || r1.NewLines != 2 {
		t.Fatalf("r1=%+v err=%v", r1, err)
	}
	n := count(t, st, `SELECT count(*) FROM records`)
	r2, err := File(context.Background(), st, root, mainPath(root), "")
	if err != nil || r2.NewLines != 0 || r2.Reset {
		t.Fatalf("r2=%+v err=%v", r2, err)
	}
	if got := count(t, st, `SELECT count(*) FROM records`); got != n || n != 2 {
		t.Fatalf("records %d -> %d", n, got)
	}
}

func TestIngestAppendOnlyNewLines(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText), false)
	File(context.Background(), st, root, p, "")
	write(t, p, userLine(2, longText)+asstLine(3, longText), true)
	r, err := File(context.Background(), st, root, p, "")
	if err != nil || r.NewLines != 2 || r.Reset {
		t.Fatalf("%+v %v", r, err)
	}
	if n := count(t, st, `SELECT count(*) FROM records`); n != 3 {
		t.Fatalf("records=%d", n)
	}
}

func TestIngestIgnoresPartialTrailingLine(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	full := userLine(1, longText)
	partial := strings.TrimSuffix(userLine(2, longText), "\n")
	write(t, p, full+partial, false)
	r, err := File(context.Background(), st, root, p, "")
	if err != nil || r.NewLines != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	var off int
	st.DB().QueryRow(`SELECT "offset" FROM files WHERE path=?`, p).Scan(&off)
	if off != len(full) {
		t.Fatalf("offset=%d want %d", off, len(full))
	}
	write(t, p, "\n", true)
	r, err = File(context.Background(), st, root, p, "")
	if err != nil || r.NewLines != 1 || r.Reset {
		t.Fatalf("%+v %v", r, err)
	}
	if n := count(t, st, `SELECT count(*) FROM records`); n != 2 {
		t.Fatalf("records=%d", n)
	}
}

func TestIngestHandles8MBLine(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	big := strings.Repeat("a", 8<<20)
	write(t, p, userLine(1, big)+userLine(2, longText), false)
	r, err := File(context.Background(), st, root, p, "")
	if err != nil || r.NewLines != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	var raw []byte
	st.DB().QueryRow(`SELECT raw FROM records WHERE line_no=1`).Scan(&raw)
	line, err := zst.Decompress(raw)
	if err != nil || len(line) != len(userLine(1, big))-1 {
		t.Fatalf("len=%d err=%v", len(line), err)
	}
	var tl int
	st.DB().QueryRow(`SELECT text_len FROM messages m JOIN records r ON r.id=m.record_id WHERE r.line_no=1`).Scan(&tl)
	if tl != len(big) {
		t.Fatalf("text_len=%d (message text must not be truncated)", tl)
	}
}

func TestIngestDetectsTruncation(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText)+userLine(2, longText)+userLine(3, longText), false)
	File(context.Background(), st, root, p, "")
	write(t, p, userLine(9, "A different first line that is long enough to chunk."), false)
	r, err := File(context.Background(), st, root, p, "")
	if err != nil || !r.Reset || r.NewLines != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if n := count(t, st, `SELECT count(*) FROM records WHERE superseded=1`); n != 3 {
		t.Fatalf("superseded=%d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM records WHERE superseded=0`); n != 1 {
		t.Fatalf("live=%d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks`); n != 1 {
		t.Fatalf("chunks=%d (old chunks must be deleted)", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH '"sufficiently"'`); n != 0 {
		t.Fatalf("stale chunk_fts rows: %d", n)
	}
	if n := count(t, st, `SELECT message_count FROM sessions WHERE session_id=?`, sid); n != 1 {
		t.Fatalf("message_count=%d", n)
	}
}

func TestIngestDetectsRewriteSameOrLargerSize(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText), false)
	File(context.Background(), st, root, p, "")
	write(t, p, userLine(2, "Completely different content, longer than before, x.")+userLine(3, longText), false)
	r, err := File(context.Background(), st, root, p, "")
	if err != nil || !r.Reset || r.NewLines != 2 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestConcurrentIngestNoDuplicates(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		sb.WriteString(userLine(i, longText))
	}
	write(t, p, sb.String(), false)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := File(context.Background(), st, root, p, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, st, `SELECT count(*) FROM records`); n != 200 {
		t.Fatalf("records=%d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks`); n != 200 {
		t.Fatalf("chunks=%d", n)
	}
}

func TestSubagentRecordsAttributedToParentSession(t *testing.T) {
	st, root := setup(t)
	p := filepath.Join(root, "proj", sid, "subagents", "agent-abc.jsonl")
	write(t, p, userLine(1, longText), false) // line carries its own sessionId == sid; rewrite below
	other := strings.Replace(userLine(2, longText), sid, "99999999-9999-9999-9999-999999999999", 1)
	write(t, p, other, true)
	if _, err := File(context.Background(), st, root, p, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM records WHERE session_id=?`, sid); n != 2 {
		t.Fatalf("records for parent=%d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE session_id=?`, sid); n != 2 {
		t.Fatalf("chunks for parent=%d", n)
	}
	var kind string
	st.DB().QueryRow(`SELECT kind FROM files WHERE path=?`, p).Scan(&kind)
	if kind != "subagent" {
		t.Fatalf("kind=%q", kind)
	}
	var raw []byte
	st.DB().QueryRow(`SELECT raw FROM records WHERE line_no=2`).Scan(&raw)
	line, _ := zst.Decompress(raw)
	if !strings.Contains(string(line), "99999999-9999") {
		t.Fatal("raw must keep the line's own sessionId")
	}
}

func TestBlankAndUnparseableLinesKeepLineNumbers(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	content := userLine(1, longText) + "\n" + "{not json\n" + userLine(2, longText)
	write(t, p, content, false)
	r, err := File(context.Background(), st, root, p, "")
	if err != nil || r.NewLines != 4 {
		t.Fatalf("%+v %v", r, err)
	}
	rows, err := st.DB().Query(`SELECT line_no, type, byte_off, byte_len, raw FROM records ORDER BY line_no`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var rebuilt strings.Builder
	var types []string
	want := []string{"user", "<blank>", "<unparseable>", "user"}
	i := 0
	for rows.Next() {
		var ln, off, bl int
		var typ string
		var raw []byte
		if err := rows.Scan(&ln, &typ, &off, &bl, &raw); err != nil {
			t.Fatal(err)
		}
		if ln != i+1 {
			t.Fatalf("line_no %d at pos %d", ln, i)
		}
		b, err := zst.Decompress(raw)
		if err != nil {
			t.Fatal(err)
		}
		if off != rebuilt.Len() || bl != len(b)+1 {
			t.Fatalf("line %d off=%d len=%d", ln, off, bl)
		}
		rebuilt.Write(b)
		rebuilt.WriteString("\n")
		types = append(types, typ)
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("types=%v", types)
	}
	if rebuilt.String() != content {
		t.Fatal("restore does not reproduce file bytes")
	}
	if n := count(t, st, `SELECT count(*) FROM messages`); n != 2 {
		t.Fatalf("messages=%d", n)
	}
}

func TestParsedRowsAndSessions(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText)+asstLine(2, longText+"\n\nshort\n\nAnother paragraph that is long enough to keep."), false)
	r, err := File(context.Background(), st, root, p, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.NewChunks != 2 { // user 1 + assistant 1 (its paragraphs pack into one piece)
		t.Fatalf("NewChunks=%d", r.NewChunks)
	}
	var role, model, tools string
	var in, out int
	var cost float64
	err = st.DB().QueryRow(`SELECT role, model, tool_names, input_tokens, output_tokens, cost_usd FROM messages m JOIN records r ON r.id=m.record_id WHERE r.type='assistant'`).Scan(&role, &model, &tools, &in, &out, &cost)
	if err != nil || role != "assistant" || model != "m1" || tools != "Bash" || in != 10 || out != 20 || cost != 0.5 {
		t.Fatalf("%v %s %s %s %d %d %v", err, role, model, tools, in, out, cost)
	}
	if n := count(t, st, `SELECT count(*) FROM tool_fts WHERE tool_fts MATCH '"zebrafish"'`); n != 1 {
		t.Fatalf("tool_fts=%d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH '"sufficiently"'`); n != 2 {
		t.Fatalf("chunk_fts=%d", n)
	}
	var proj, cwd, br, started, ended, kind string
	var mc int
	err = st.DB().QueryRow(`SELECT project, cwd, git_branch, started, ended, message_count, kind FROM sessions WHERE session_id=?`, sid).Scan(&proj, &cwd, &br, &started, &ended, &mc, &kind)
	if err != nil || proj != "proj" || cwd != "/work/x" || br != "main" || mc != 2 || kind != "main" ||
		started != "2026-01-01T00:00:01Z" || ended != "2026-01-01T00:01:02Z" {
		t.Fatalf("%v %s %s %s %s %s %d %s", err, proj, cwd, br, started, ended, mc, kind)
	}
	var cts, ck, cr string
	st.DB().QueryRow(`SELECT ts, kind, role FROM chunks LIMIT 1`).Scan(&cts, &ck, &cr)
	if cts == "" || ck != "main" || cr == "" {
		t.Fatalf("chunk meta %q %q %q", cts, ck, cr)
	}
}

func TestUsageOnlyCountedWithServiceTier(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	noTier := strings.Replace(asstLine(1, longText), `,"service_tier":"standard"`, "", 1)
	write(t, p, noTier, false)
	File(context.Background(), st, root, p, "")
	var in int
	var cost float64
	st.DB().QueryRow(`SELECT input_tokens, cost_usd FROM messages`).Scan(&in, &cost)
	if in != 0 || cost != 0 {
		t.Fatalf("in=%d cost=%v", in, cost)
	}
}

func TestChunkTexts(t *testing.T) {
	if got := chunkTexts([]string{"tiny", "short para"}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	long := strings.Repeat("é", 4500)
	got := chunkTexts([]string{long})
	if len(got) != 3 || len([]rune(got[0])) != 2000 || len([]rune(got[2])) != 500 {
		t.Fatalf("lens %d", len(got))
	}
	two := strings.Repeat("a", 1500) + "\n\n" + strings.Repeat("b", 1500)
	if got := chunkTexts([]string{two}); len(got) != 2 {
		t.Fatalf("%d", len(got))
	}
	three := strings.Repeat("a", 30) + "\n\nxx\n\n" + strings.Repeat("b", 30)
	got = chunkTexts([]string{three})
	if len(got) != 1 || !strings.Contains(got[0], "xx") {
		t.Fatalf("%q", got)
	}
}

func TestSweepBackoffAndSummary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0 does not make a file unreadable on Windows")
	}
	st, root := setup(t)
	good := mainPath(root)
	write(t, good, userLine(1, longText), false)
	// unrecognised layout: skipped, not failed
	write(t, filepath.Join(root, "stray.jsonl"), "x\n", false)
	sum, err := Sweep(context.Background(), st, root, "")
	if err != nil || sum.Files != 1 || sum.NewLines != 1 || sum.Skipped != 1 || sum.Failed != 0 {
		t.Fatalf("%+v %v", sum, err)
	}
	// failing file: unreadable
	bad := filepath.Join(root, "proj", "22222222-2222-2222-2222-222222222222.jsonl")
	write(t, bad, userLine(1, longText), false)
	os.Chmod(bad, 0)
	defer os.Chmod(bad, 0o644)
	if os.Geteuid() == 0 {
		t.Skip("root can read chmod 0 files")
	}
	sum, _ = Sweep(context.Background(), st, root, "")
	if sum.Failed != 1 {
		t.Fatalf("%+v", sum)
	}
	var retry string
	st.DB().QueryRow(`SELECT next_retry_at FROM files WHERE path=?`, bad).Scan(&retry)
	ts, err := time.Parse(time.RFC3339, retry)
	if err != nil || ts.Before(time.Now().Add(20*time.Second)) || ts.After(time.Now().Add(40*time.Second)) {
		t.Fatalf("next_retry_at=%q err=%v", retry, err)
	}
	sum, _ = Sweep(context.Background(), st, root, "")
	if sum.Failed != 0 || sum.Skipped != 2 {
		t.Fatalf("second sweep should skip backoff file: %+v", sum)
	}
	// backoff doubles on consecutive failure, success clears
	st.DB().Exec(`UPDATE files SET next_retry_at=? WHERE path=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339), bad)
	sum, _ = Sweep(context.Background(), st, root, "")
	st.DB().QueryRow(`SELECT next_retry_at FROM files WHERE path=?`, bad).Scan(&retry)
	ts, _ = time.Parse(time.RFC3339, retry)
	if sum.Failed != 1 || ts.Before(time.Now().Add(50*time.Second)) {
		t.Fatalf("expected ~60s backoff: %+v %q", sum, retry)
	}
	os.Chmod(bad, 0o644)
	st.DB().Exec(`UPDATE files SET next_retry_at=? WHERE path=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339), bad)
	sum, _ = Sweep(context.Background(), st, root, "")
	if sum.Failed != 0 {
		t.Fatalf("%+v", sum)
	}
	var nr *string
	st.DB().QueryRow(`SELECT next_retry_at FROM files WHERE path=?`, bad).Scan(&nr)
	if nr != nil {
		t.Fatalf("success must clear next_retry_at, got %q", *nr)
	}
}

func TestOversizeLineFailsFileWithoutWrites(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	old := maxLine
	maxLine = 1000
	defer func() { maxLine = old }()
	write(t, p, userLine(1, longText)+userLine(2, strings.Repeat("a", 2000)), false)
	if _, err := File(context.Background(), st, root, p, ""); err == nil {
		t.Fatal("expected error for line over cap")
	}
	if n := count(t, st, `SELECT count(*) FROM records`); n != 0 {
		t.Fatalf("records=%d, tx must roll back", n)
	}
	if n := count(t, st, `SELECT "offset" FROM files WHERE path=?`, p); n != 0 {
		t.Fatalf("offset=%d", n)
	}
	var retry *string
	st.DB().QueryRow(`SELECT next_retry_at FROM files WHERE path=?`, p).Scan(&retry)
	if retry == nil {
		t.Fatal("expected backoff")
	}
}

func TestIngest65MBLineRoundTrips(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	line := userLine(1, strings.Repeat("b", 65<<20))
	write(t, p, line, false)
	if r, err := File(context.Background(), st, root, p, ""); err != nil || r.NewLines != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	var raw []byte
	st.DB().QueryRow(`SELECT raw FROM records WHERE line_no=1`).Scan(&raw)
	got, err := zst.Decompress(raw)
	if err != nil || string(got) != strings.TrimSuffix(line, "\n") {
		t.Fatalf("round trip mismatch err=%v len=%d", err, len(got))
	}
}

func TestResetDeletesChunkVecRows(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText)+userLine(2, longText), false)
	File(context.Background(), st, root, p, "")
	vec := make([]byte, 768*4)
	rows, _ := st.DB().Query(`SELECT id FROM chunks`)
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("chunks=%d", len(ids))
	}
	for _, id := range ids {
		if _, err := st.DB().Exec(`INSERT INTO chunk_vec(rowid, embedding, session_id, project, kind, role, ts) VALUES (?,?,?,?,?,?,?)`,
			id, vec, sid, "proj", "main", "user", 1000); err != nil {
			t.Fatal(err)
		}
	}
	write(t, p, userLine(9, "A different first line that is long enough to chunk."), false)
	if r, err := File(context.Background(), st, root, p, ""); err != nil || !r.Reset {
		t.Fatalf("%+v %v", r, err)
	}
	if n := count(t, st, `SELECT count(*) FROM chunk_vec`); n != 0 {
		t.Fatalf("orphan chunk_vec rows: %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM messages m JOIN records r ON r.id=m.record_id WHERE r.superseded=1`); n != 0 {
		t.Fatalf("messages rows referencing superseded records: %d", n)
	}
}

func TestStaleStatDoesNotSupersede(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	ctx := context.Background()
	write(t, p, userLine(1, longText)+userLine(2, longText)+userLine(3, longText), false)
	if _, err := File(ctx, st, root, p, ""); err != nil {
		t.Fatal(err)
	}
	write(t, p, userLine(4, longText), true)
	// Between this call's fast-path stat and its transaction, a competing
	// process appends one more line and ingests everything.
	fired := false
	beforeTxn = func() {
		if fired {
			return
		}
		fired = true
		write(t, p, userLine(5, longText), true)
		if _, err := File(ctx, st, root, p, ""); err != nil {
			t.Error(err)
		}
	}
	defer func() { beforeTxn = nil }()
	res, err := File(ctx, st, root, p, "")
	if err != nil || !fired {
		t.Fatalf("err=%v fired=%v", err, fired)
	}
	if res.Reset {
		t.Fatal("stale pre-transaction stat caused a reset")
	}
	if n := count(t, st, `SELECT COUNT(*) FROM records`); n != 5 {
		t.Fatalf("records = %d, want 5", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM records WHERE superseded=1`); n != 0 {
		t.Fatalf("superseded = %d", n)
	}
}

func TestSessionStartedUsesMessageRecords(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	snap := fmt.Sprintf(`{"type":"file-history-snapshot","sessionId":"%s","timestamp":"2025-06-01T00:00:00Z"}`, sid) + "\n"
	write(t, p, snap+userLine(1, longText)+asstLine(2, longText)+
		fmt.Sprintf(`{"type":"queue-operation","sessionId":"%s","timestamp":"2027-01-01T00:00:00Z"}`, sid)+"\n", false)
	if _, err := File(context.Background(), st, root, p, ""); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		var started, ended string
		if err := st.DB().QueryRow(`SELECT started, ended FROM sessions WHERE session_id=?`, sid).Scan(&started, &ended); err != nil {
			t.Fatal(err)
		}
		if started != "2026-01-01T00:00:01Z" || ended != "2026-01-01T00:01:02Z" {
			t.Fatalf("%s: started=%s ended=%s", when, started, ended)
		}
	}
	check("ingest")
	if _, err := st.DB().Exec(`UPDATE sessions SET started=NULL, ended=NULL`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM messages`, `DELETE FROM chunks`, `DELETE FROM tool_fts`, `INSERT INTO chunk_fts(chunk_fts) VALUES('delete-all')`} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Rederive(context.Background(), st, ""); err != nil {
		t.Fatal(err)
	}
	check("rederive")
}

func TestProfileWrittenAndPreservedByRederive(t *testing.T) {
	st, root := setup(t)
	p := mainPath(root)
	write(t, p, userLine(1, longText)+asstLine(2, longText), false)
	if _, err := File(context.Background(), st, root, p, "laptop"); err != nil {
		t.Fatal(err)
	}
	var fp, sp string
	if err := st.DB().QueryRow(`SELECT profile FROM files WHERE path=?`, p).Scan(&fp); err != nil || fp != "laptop" {
		t.Fatalf("files.profile = %q, %v", fp, err)
	}
	if err := st.DB().QueryRow(`SELECT profile FROM sessions WHERE session_id=?`, sid).Scan(&sp); err != nil || sp != "laptop" {
		t.Fatalf("sessions.profile = %q, %v", sp, err)
	}
	if _, err := Rederive(context.Background(), st, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT profile FROM sessions WHERE session_id=?`, sid).Scan(&sp); err != nil || sp != "laptop" {
		t.Fatalf("after rederive sessions.profile = %q, %v", sp, err)
	}
	if err := st.DB().QueryRow(`SELECT profile FROM files WHERE path=?`, p).Scan(&fp); err != nil || fp != "laptop" {
		t.Fatalf("after rederive files.profile = %q, %v", fp, err)
	}
}
