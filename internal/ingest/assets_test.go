package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/zst"
)

func writeMode(t *testing.T, path, content string, mode os.FileMode, mt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, mode)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestAssetRoundTripAndKinds(t *testing.T) {
	st, root := setup(t)
	mt := time.Unix(1700000000, 123456789)
	files := map[string]string{
		filepath.Join("proj", sid, "tool-results", "x.txt"):                    "tool output body",
		filepath.Join("proj", sid, "subagents", "agent-1.meta.json"):           `{"a":1}`,
		filepath.Join("proj", sid, "subagents", "workflows", "wf_1", "s.json"): `{"w":1}`,
		filepath.Join("proj", sid, "workflows", "scripts", "s.js"):             "x=1",
		filepath.Join("proj", sid, "misc.json"):                                "{}",
		filepath.Join("proj", "bridge-pointer.json"):                           "{}",
	}
	for rel, c := range files {
		writeMode(t, filepath.Join(root, rel), c, 0o600, mt)
	}
	write(t, mainPath(root), userLine(1, longText), false)
	sum, err := Sweep(context.Background(), st, root, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Assets != 6 || sum.AssetsArchived != 6 {
		t.Fatalf("%+v", sum)
	}
	kinds := map[string]string{}
	rows, _ := st.DB().Query(`SELECT path, kind FROM assets`)
	for rows.Next() {
		var p, k string
		rows.Scan(&p, &k)
		kinds[filepath.Base(p)] = k
	}
	rows.Close()
	want := map[string]string{"x.txt": "tool-result", "agent-1.meta.json": "meta", "s.json": "workflow", "s.js": "workflow", "misc.json": "other", "bridge-pointer.json": "other"}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("%s: kind %q want %q", k, kinds[k], v)
		}
	}
	p := filepath.Join(root, "proj", sid, "tool-results", "x.txt")
	var raw []byte
	var shaDB string
	var mode, mtime int64
	if err := st.DB().QueryRow(`SELECT raw, sha256, mode, mtime FROM assets WHERE path=?`, p).Scan(&raw, &shaDB, &mode, &mtime); err != nil {
		t.Fatal(err)
	}
	// Windows reports only 0666 (writable) or 0444 (read-only).
	wantMode := int64(0o600)
	if runtime.GOOS == "windows" {
		wantMode = 0o666
	}
	got, err := zst.Decompress(raw)
	h := sha256.Sum256([]byte("tool output body"))
	if err != nil || !bytes.Equal(got, []byte("tool output body")) || shaDB != hex.EncodeToString(h[:]) || mode != wantMode || mtime != mt.UnixNano() {
		t.Fatalf("round trip: %q %s %o %d %v", got, shaDB, mode, mtime, err)
	}
	if n := count(t, st, `SELECT count(*) FROM asset_fts`); n != 6 {
		t.Fatalf("asset_fts rows %d", n)
	}
	// second sweep: nothing changes
	sum, _ = Sweep(context.Background(), st, root, "p1")
	if sum.AssetsArchived != 0 {
		t.Fatalf("resweep archived %d", sum.AssetsArchived)
	}
	// changed content: re-archived, latest version only
	writeMode(t, p, "tool output body v2", 0o600, mt.Add(time.Hour))
	sum, _ = Sweep(context.Background(), st, root, "p1")
	if sum.AssetsArchived != 1 || count(t, st, `SELECT count(*) FROM assets WHERE path=?`, p) != 1 {
		t.Fatalf("%+v", sum)
	}
	st.DB().QueryRow(`SELECT raw FROM assets WHERE path=?`, p).Scan(&raw)
	if got, _ := zst.Decompress(raw); string(got) != "tool output body v2" {
		t.Fatalf("%q", got)
	}
	if n := count(t, st, `SELECT count(*) FROM asset_fts WHERE asset_fts MATCH '"body v2"'`); n != 1 {
		t.Fatalf("fts %d", n)
	}
}

func TestAssetTooLargeSkipped(t *testing.T) {
	st, root := setup(t)
	old := MaxAssetBytes
	MaxAssetBytes = 10
	defer func() { MaxAssetBytes = old }()
	var errb bytes.Buffer
	oe := Stderr
	Stderr = &errb
	defer func() { Stderr = oe }()
	p := filepath.Join(root, "proj", sid, "tool-results", "big.txt")
	writeMode(t, p, strings.Repeat("x", 11), 0o600, time.Now())
	sum, _ := Sweep(context.Background(), st, root, "")
	if sum.AssetsSkipped != 1 || count(t, st, `SELECT count(*) FROM assets`) != 0 || !strings.Contains(errb.String(), "big.txt") {
		t.Fatalf("%+v %q", sum, errb.String())
	}
}

func TestMemoryChunkedAndSession(t *testing.T) {
	st, root := setup(t)
	p := filepath.Join(root, "proj", "memory", "MEMORY.md")
	writeMode(t, p, "# Memory\n\nThe zebrafish deployment uses a blue green rollout strategy.", 0o644, time.Now())
	if _, err := Sweep(context.Background(), st, root, "p1"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE kind='memory' AND session_id='memory:proj' AND embedded_at IS NULL`); n != 1 {
		t.Fatalf("memory chunks %d", n)
	}
	var title, kind string
	if err := st.DB().QueryRow(`SELECT title, kind FROM sessions WHERE session_id='memory:proj'`).Scan(&title, &kind); err != nil || title != "memory" || kind != "memory" {
		t.Fatalf("%q %q %v", title, kind, err)
	}
	if n := count(t, st, `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH 'zebrafish'`); n != 1 {
		t.Fatalf("chunk_fts %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE kind='memory' AND text LIKE 'MEMORY.md'||char(10)||'%'`); n != 1 {
		t.Fatalf("memory chunk text must start with the file name")
	}
	if n := count(t, st, `SELECT count(*) FROM records r JOIN files f ON f.id=r.file_id WHERE f.path='asset:anchor' AND r.session_id IS NOT NULL`); n != 0 {
		t.Fatalf("anchor record with a session")
	}
	// changed memory file replaces its chunks
	writeMode(t, p, "# Memory\n\nCompletely different narwhal content lives here now.", 0o644, time.Now().Add(time.Hour))
	Sweep(context.Background(), st, root, "p1")
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE kind='memory'`); n != 1 {
		t.Fatalf("memory chunks after change %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH 'zebrafish'`); n != 0 {
		t.Fatalf("stale fts %d", n)
	}
	// Rederive and Repair keep assets and rebuild memory chunks
	if _, err := Rederive(context.Background(), st, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE kind='memory'`); n != 1 || count(t, st, `SELECT count(*) FROM assets`) != 1 {
		t.Fatalf("after rederive")
	}
	if n := count(t, st, `SELECT count(*) FROM sessions WHERE session_id='memory:proj'`); n != 1 {
		t.Fatal("memory session lost")
	}
	if _, err := Repair(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE kind='memory'`); n != 1 {
		t.Fatalf("repair removed asset chunks: %d", n)
	}
}

func TestFileHistoryAsset(t *testing.T) {
	st, root := setup(t)
	prof := t.TempDir()
	proj := filepath.Join(prof, "projects")
	os.MkdirAll(proj, 0o755)
	fh := filepath.Join(prof, "file-history", sid, "abc@v1")
	writeMode(t, fh, "snapshot", 0o644, time.Now())
	_ = root
	sum, err := Sweep(context.Background(), st, proj, "p1")
	if err != nil || sum.AssetsArchived != 1 {
		t.Fatalf("%+v %v", sum, err)
	}
	var kind, s string
	st.DB().QueryRow(`SELECT kind, session_id FROM assets`).Scan(&kind, &s)
	if kind != "file-history" || s != sid {
		t.Fatalf("%s %s", kind, s)
	}
}

func TestExtractFileChanges(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string // "op:path"
	}{
		{"Edit", `{"file_path":"/a/b.go","old_string":"aa","new_string":"bbbb"}`, []string{"edit:/a/b.go"}},
		{"MultiEdit", `{"file_path":"/a/c.go","edits":[{"old_string":"x","new_string":"yy"},{"old_string":"","new_string":"z"}]}`, []string{"edit:/a/c.go"}},
		{"Write", `{"file_path":"/a/d.md","content":"hello"}`, []string{"write:/a/d.md"}},
		{"NotebookEdit", `{"notebook_path":"/a/n.ipynb","new_source":"print(1)"}`, []string{"edit:/a/n.ipynb"}},
		{"Bash", `{"command":"sed -i 's/a/b/' /x/f.txt"}`, []string{"shell:/x/f.txt"}},
		{"Bash", `{"command":"sed -i.bak -e 's/a/b/' g.txt h.txt"}`, []string{"shell:g.txt", "shell:h.txt"}},
		{"Bash", `{"command":"echo hi > out.txt && echo more >> log.txt"}`, []string{"shell:out.txt", "shell:log.txt"}},
		{"Bash", `{"command":"cat > /tmp/x.sh <<'EOF'\necho a > ignored.txt\nEOF"}`, []string{"shell:/tmp/x.sh"}},
		{"Bash", `{"command":"make 2>&1 | tee -a build.log"}`, []string{"shell:build.log"}},
		{"Bash", `{"command":"ls -la 2>/dev/null; echo x > /dev/null"}`, nil},
		{"Bash", `{"command":"echo done"}`, nil},
		{"Bash", `{"command":"sed -i '' 's/a/b/' file.go"}`, []string{"shell:file.go"}},
		{"Bash", `{"command":"sed -i '' -e 's/a/b/' src/x.go src/y.go"}`, []string{"shell:src/x.go", "shell:src/y.go"}},
		{"Bash", "{\"command\":\"git commit -m \\\"fix\\n> quoted\\\"\"}", nil},
		{"Bash", "{\"command\":\"python3 -c \\\"\\nx = a > b\\\"\"}", nil},
		{"Bash", `{"command":"[ 3 > 2 ] && echo hi"}`, nil},
		{"Bash", "{\"command\":\"cat <<'EOF' > notes/a.md\\nx > y.txt\\nEOF\\necho z >> log.txt\"}", []string{"shell:notes/a.md", "shell:log.txt"}},
		{"Bash", `{"command":"echo x > \"my dir/f.txt\""}`, []string{"shell:my dir/f.txt"}},
		{"Bash", `{"command":"echo x > Makefile"}`, nil},
		{"Bash", `{"command":"sed -n '1,5p' file.txt"}`, nil},
		{"Bash", `{"command":"echo $X > $OUT"}`, nil},
		{"Read", `{"file_path":"/a"}`, nil},
	}
	for _, c := range cases {
		var got []string
		for _, fc := range ExtractFileChanges(c.name, []byte(c.in)) {
			got = append(got, fc.Op+":"+fc.FilePath)
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s %s: got %v want %v", c.name, c.in, got, c.want)
		}
	}
	fc := ExtractFileChanges("Edit", []byte(`{"file_path":"f","old_string":"aa","new_string":"`+strings.Repeat("n", 400)+`"}`))[0]
	if fc.AddedChars != 400 || fc.RemovedCh != 2 || len([]rune(fc.Excerpt)) != 300 {
		t.Fatalf("%+v", fc)
	}
}

func TestFileChangeRowsAndResultJoin(t *testing.T) {
	st, root := setup(t)
	a := fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"%s","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_ok","name":"Edit","input":{"file_path":"/p/one.go","old_string":"a","new_string":"b"}},{"type":"tool_use","id":"toolu_bad","name":"Write","input":{"file_path":"/p/two.go","content":"zz"}}]}}`, sid) + "\n"
	u := fmt.Sprintf(`{"type":"user","uuid":"u2","sessionId":"%s","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_ok","content":"edited"},{"type":"tool_result","tool_use_id":"toolu_bad","is_error":true,"content":"permission denied"}]}}`, sid) + "\n"
	write(t, mainPath(root), a+u, false)
	if _, err := File(context.Background(), st, root, mainPath(root), "p1"); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		type row struct {
			rec, res int64
			ok       int
			tuid, p  string
		}
		rows, err := st.DB().Query(`SELECT record_id, result_record_id, result_ok, tool_use_id, file_path FROM file_changes ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.rec, &r.res, &r.ok, &r.tuid, &r.p); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		var aID, uID int64
		st.DB().QueryRow(`SELECT id FROM records WHERE uuid='a1'`).Scan(&aID)
		st.DB().QueryRow(`SELECT id FROM records WHERE uuid='u2'`).Scan(&uID)
		if len(got) != 2 || got[0] != (row{aID, uID, 1, "toolu_ok", "/p/one.go"}) || got[1] != (row{aID, uID, 0, "toolu_bad", "/p/two.go"}) {
			t.Fatalf("%+v (a=%d u=%d)", got, aID, uID)
		}
		if n := count(t, st, `SELECT count(*) FROM file_fts WHERE file_fts MATCH '"one.go"'`); n != 1 {
			t.Fatalf("file_fts %d", n)
		}
	}
	check()
	// summary survives Rederive and is re-linked; its chunk is recreated
	var cid int64
	st.DB().QueryRow(`SELECT id FROM file_changes WHERE tool_use_id='toolu_ok'`).Scan(&cid)
	st.DB().Exec(`INSERT INTO summaries(change_id, text, model, created_at) VALUES(?, 'did a thing', 'm', 'now')`, cid)
	if _, err := Rederive(context.Background(), st, ""); err != nil {
		t.Fatal(err)
	}
	check()
	var sum string
	if err := st.DB().QueryRow(`SELECT s.text FROM summaries s JOIN file_changes fc ON fc.id=s.change_id WHERE fc.tool_use_id='toolu_ok'`).Scan(&sum); err != nil || sum != "did a thing" {
		t.Fatalf("relink: %q %v", sum, err)
	}
	var txt string
	if err := st.DB().QueryRow(`SELECT text FROM chunks WHERE kind='filechange'`).Scan(&txt); err != nil || txt != "/p/one.go (Edit 2026-01-01): did a thing" {
		t.Fatalf("%q %v", txt, err)
	}
}

func TestSavedPathAndPointerResolution(t *testing.T) {
	p := "/Users/x/Library/CloudStorage/OneDrive-Personal/profiles/desktop/projects/proj/" + sid + "/tool-results/toolu_1.txt"
	if got := SavedPath("Error: Output too large (31 KB). Full output saved to: " + p + "\n\nPreview (first 2 KB): ..."); got != p {
		t.Fatalf("%q", got)
	}
	if got := SavedPath("Output too large (31 KB). Full output saved to: " + p + "\nsee also Full output saved to: /other/tool-results/x.txt"); got != p {
		t.Fatalf("first match only: %q", got)
	}
	for _, quoted := range []string{
		"see " + p + " for details",
		"grep result:\n" + p + ":12: matched",
		"The file says: Full output saved to: " + p,
		"Output too large (3 KB).\nread " + p,
	} {
		if got := SavedPath(quoted); got != "" {
			t.Fatalf("quoted path treated as pointer: %q -> %q", quoted, got)
		}
	}
	if SavedPath("nothing here") != "" {
		t.Fatal("false positive")
	}
	st, root := setup(t)
	tr := filepath.Join(root, "proj", sid, "tool-results", "toolu_1.txt")
	writeMode(t, tr, "output body", 0o600, time.Now())
	msg := "Output too large (31 KB). Full output saved to: /elsewhere/home/proj/" + sid + "/tool-results/toolu_1.txt\nPreview"
	renamed := "Output too large (9 KB). Full output saved to: /old/home/other-slug/" + sid + "/tool-results/toolu_1.txt"
	_ = renamed
	missing := "Output too large (9 KB). Full output saved to: /elsewhere/home/proj/" + sid + "/tool-results/gone.txt"
	a := userLine(1, "x") // plain user line, then a tool_result user record
	u := fmt.Sprintf(`{"type":"user","uuid":"u9","sessionId":"%s","timestamp":"2026-01-01T00:00:09Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","content":%q},{"type":"tool_result","tool_use_id":"t8","content":%q},{"type":"tool_result","tool_use_id":"t7","content":%q}]}}`, sid, msg, missing, renamed) + "\n"
	write(t, mainPath(root), a+u, false)
	if _, err := Sweep(context.Background(), st, root, "p"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM tool_outputs`); n != 3 {
		t.Fatalf("tool_outputs %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM tool_outputs o JOIN assets a ON a.id=o.asset_id WHERE o.idx IN (0,2)`); n != 2 {
		t.Fatalf("resolved %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM tool_outputs WHERE saved_path IS NOT NULL AND asset_id IS NULL`); n != 1 {
		t.Fatalf("unresolved %d", n)
	}
	// resolution survives Rederive
	if _, err := Rederive(context.Background(), st, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM tool_outputs WHERE asset_id IS NOT NULL`); n != 2 {
		t.Fatalf("resolved after rederive %d", n)
	}
}

func TestBackfillExtrasOnce(t *testing.T) {
	st, root := setup(t)
	a := fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"%s","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tb","name":"Write","input":{"file_path":"/p/w.go","content":"package w"}}]}}`, sid) + "\n"
	u := fmt.Sprintf(`{"type":"user","uuid":"u2","sessionId":"%s","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tb","content":"done"}]}}`, sid) + "\n"
	write(t, mainPath(root), a+u, false)
	if _, err := File(context.Background(), st, root, mainPath(root), "p"); err != nil {
		t.Fatal(err)
	}
	// simulate an archive that predates the tables
	for _, q := range []string{`DELETE FROM file_fts`, `DELETE FROM file_changes`, `DELETE FROM tool_outputs`, `DELETE FROM meta WHERE key='extras_derived_v1'`} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	n, err := BackfillExtras(context.Background(), st)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if count(t, st, `SELECT count(*) FROM file_changes WHERE result_ok=1 AND result_record_id IS NOT NULL`) != 1 || count(t, st, `SELECT count(*) FROM tool_outputs`) != 1 {
		t.Fatal("backfill rows missing")
	}
	if n, _ := BackfillExtras(context.Background(), st); n != 0 {
		t.Fatalf("second run processed %d", n)
	}
}

func TestAssetFailurePersistedWithBackoffAndBinaryMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0 does not make a file unreadable on Windows")
	}
	st, root := setup(t)
	bad := filepath.Join(root, "proj", sid, "tool-results", "locked.txt")
	writeMode(t, bad, "secret", 0o600, time.Now())
	os.Chmod(bad, 0)
	defer os.Chmod(bad, 0o600)
	pdf := filepath.Join(root, "proj", sid, "tool-results", "doc.pdf")
	writeMode(t, pdf, "%PDF-1.4 fake", 0o600, time.Now())
	ds := filepath.Join(root, "proj", sid, ".DS_Store")
	writeMode(t, ds, "x", 0o600, time.Now())
	var errb bytes.Buffer
	oe := Stderr
	Stderr = &errb
	defer func() { Stderr = oe }()
	sum, _ := Sweep(context.Background(), st, root, "p")
	if sum.AssetsFailed != 1 || sum.AssetsIgnored != 1 || sum.AssetsArchived != 1 || sum.AssetsSkipped != 0 {
		t.Fatalf("%+v", sum)
	}
	var reason, retry string
	var raw []byte
	if err := st.DB().QueryRow(`SELECT fail_reason, next_retry_at, raw FROM assets WHERE path=? AND failed_at IS NOT NULL`, bad).Scan(&reason, &retry, &raw); err != nil || raw != nil || reason == "" || retry == "" {
		t.Fatalf("failure row: %q %q %v %v", reason, retry, raw, err)
	}
	// in backoff: a second sweep neither reads nor prints again
	errb.Reset()
	sum, _ = Sweep(context.Background(), st, root, "p")
	if sum.AssetsFailed != 1 || errb.Len() != 0 {
		t.Fatalf("%+v %q", sum, errb.String())
	}
	// non-text tool-result: failed:binary summary row, not pending
	if n := count(t, st, `SELECT count(*) FROM summaries s JOIN assets a ON a.id=s.asset_id WHERE s.model='failed:binary' AND a.path LIKE '%doc.pdf'`); n != 1 {
		t.Fatalf("binary marker %d", n)
	}
	// once readable and out of backoff the asset is archived and the failure cleared
	os.Chmod(bad, 0o600)
	st.DB().Exec(`UPDATE assets SET next_retry_at='2000-01-01T00:00:00Z' WHERE path=?`, bad)
	sum, _ = Sweep(context.Background(), st, root, "p")
	if sum.AssetsArchived != 1 || count(t, st, `SELECT count(*) FROM assets WHERE failed_at IS NOT NULL`) != 0 || count(t, st, `SELECT count(*) FROM assets WHERE raw IS NULL`) != 0 {
		t.Fatalf("%+v", sum)
	}
}

func TestRebuildExtrasKeepsToolSummariesDropsChangeSummaries(t *testing.T) {
	st, root := setup(t)
	a := fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"%s","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tc","name":"Write","input":{"file_path":"/p/w.go","content":"package w"}}]}}`, sid) + "\n"
	u := fmt.Sprintf(`{"type":"user","uuid":"u2","sessionId":"%s","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tc","content":%q}]}}`, sid, strings.Repeat("o", 3000)) + "\n"
	write(t, mainPath(root), a+u, false)
	if _, err := Sweep(context.Background(), st, root, "p"); err != nil {
		t.Fatal(err)
	}
	var recU, cid int64
	st.DB().QueryRow(`SELECT id FROM records WHERE uuid='u2'`).Scan(&recU)
	st.DB().QueryRow(`SELECT id FROM file_changes`).Scan(&cid)
	var recA int64
	st.DB().QueryRow(`SELECT id FROM records WHERE uuid='a1'`).Scan(&recA)
	st.DB().Exec(`INSERT INTO summaries(record_id, idx, text, model, created_at) VALUES(?,0,'tool sum','m','n')`, recU)
	AddChunk(context.Background(), st.DB(), recU, sid, "summary", "", "", 1000, "tool sum")
	st.DB().Exec(`INSERT INTO summaries(change_id, text, model, created_at) VALUES(?,'change sum','m','n')`, cid)
	AddChunk(context.Background(), st.DB(), recA, sid, "filechange", "", "", 1000, "/p/w.go (Write 2026-01-01): change sum")
	// an orphaned tool summary (its output no longer exists) is dropped with its chunk
	st.DB().Exec(`INSERT INTO summaries(record_id, idx, text, model, created_at) VALUES(?,7,'gone','m','n')`, recU)
	AddChunk(context.Background(), st.DB(), recU, sid, "summary", "", "", 1007, "gone")
	rep, err := RebuildExtras(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FileChangesAfter != 1 || rep.ToolOutputsAfter != 1 || rep.SummariesKept != 1 || rep.SummariesDeleted != 2 {
		t.Fatalf("%+v", rep)
	}
	if count(t, st, `SELECT count(*) FROM summaries WHERE text='tool sum'`) != 1 || count(t, st, `SELECT count(*) FROM chunks WHERE text='tool sum'`) != 1 ||
		count(t, st, `SELECT count(*) FROM chunks WHERE kind='filechange'`) != 0 || count(t, st, `SELECT count(*) FROM chunks WHERE text='gone'`) != 0 ||
		count(t, st, `SELECT count(*) FROM summaries WHERE change_id IS NOT NULL`) != 0 {
		t.Fatalf("summaries/chunks wrong: %+v", rep)
	}
	if count(t, st, `SELECT count(*) FROM pragma_foreign_key_check`) != 0 || count(t, st, `SELECT count(*) FROM file_fts WHERE file_fts MATCH '"w.go"'`) != 1 {
		t.Fatal("integrity after rebuild-extras")
	}
}
