package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/summarize"
)

// fakeOllama answers /api/embed (constant-ish vectors) and /api/chat.
func fakeOllama(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embed":
			var req struct {
				Input []string `json:"input"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			vs := make([][]float32, len(req.Input))
			for i := range vs {
				v := make([]float32, 768)
				v[i%768] = 1
				vs[i] = v
			}
			json.NewEncoder(w).Encode(map[string]any{"embeddings": vs})
		case "/api/chat":
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "single file change") {
				fmt.Fprint(w, `{"message":{"content":"Replaced foo with bar in main.go quasarflux handler."}}`)
			} else {
				fmt.Fprint(w, `{"message":{"content":"Build output listing zorblax lines."}}`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seedAssetsFixture(t *testing.T) (root string) {
	root, _ = seedRoot(t)
	big := strings.Repeat("build output line\n", 200)
	a := fmt.Sprintf(`{"type":"assistant","uuid":"a9","sessionId":"%s","timestamp":"2026-03-04T00:00:03Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Edit","input":{"file_path":"/src/main.go","old_string":"foo()","new_string":"bar()"}},{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"go build ./..."}}]}}`, tsid) + "\n"
	u := fmt.Sprintf(`{"type":"user","uuid":"u10","sessionId":"%s","timestamp":"2026-03-04T00:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"},{"type":"tool_result","tool_use_id":"tu2","content":%q}]}}`, tsid, big) + "\n"
	p := filepath.Join(root, "proj", tsid+".jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(a + u)
	f.Close()
	os.MkdirAll(filepath.Join(root, "proj", "memory"), 0o755)
	os.WriteFile(filepath.Join(root, "proj", "memory", "MEMORY.md"), []byte("# Memory\n\nThe narwhalprotocol deployment notes live in this file."), 0o644)
	os.MkdirAll(filepath.Join(root, "proj", tsid, "tool-results"), 0o755)
	os.WriteFile(filepath.Join(root, "proj", tsid, "tool-results", "tu2.txt"), []byte(big), 0o644)
	return root
}

func TestCLISummarizeFilesShowSearchDoctor(t *testing.T) {
	root := seedAssetsFixture(t)
	srv := fakeOllama(t)
	t.Setenv("SESSION_INDEX_OLLAMA", srv.URL)
	code, out, e := runCLI(t, "sweep", "--embed")
	if code != 0 || !strings.Contains(out, "assets (") || !strings.Contains(out, "embedded") {
		t.Fatalf("sweep %d %s %s", code, out, e)
	}
	code, out, e = runCLI(t, "summarize", "--parallel", "2")
	if code != 0 || !strings.Contains(out, "summarized 3 items (0 failed)") {
		t.Fatalf("summarize %d %q %q", code, out, e)
	}
	// summaries are chunks: embed them, then search finds them with markers
	if code, _, e := runCLI(t, "embed"); code != 0 {
		t.Fatalf("embed %d %s", code, e)
	}
	code, out, _ = runCLI(t, "search", "quasarflux", "--mode", "fts")
	if code != 0 || !strings.Contains(out, "[file] /src/main.go (Edit 2026-03-04)") || !strings.Contains(out, "route: fts") {
		t.Fatalf("search: %q", out)
	}
	if code, out, _ = runCLI(t, "search", "zorblax", "--mode", "fts"); code != 0 || !strings.Contains(out, "[summary] Build output") {
		t.Fatalf("summary marker: %q", out)
	}
	code, out, _ = runCLI(t, "search", "narwhalprotocol", "--mode", "fts", "--json")
	if code != 0 || !strings.Contains(out, `"kind": "memory"`) || !strings.Contains(out, "memory:proj") {
		t.Fatalf("memory search: %q", out)
	}
	code, out, _ = runCLI(t, "search", "zorblax", "--mode", "fts", "--kind", "summary", "--json")
	if code != 0 || !strings.Contains(out, `"kind": "summary"`) {
		t.Fatalf("summary search: %q", out)
	}
	code, out, _ = runCLI(t, "files", "main.go")
	if code != 0 || !strings.Contains(out, "/src/main.go") || !strings.Contains(out, "Edit") || !strings.Contains(out, "quasarflux") || !strings.Contains(out, " ok ") {
		t.Fatalf("files: %q", out)
	}
	code, out, _ = runCLI(t, "files", "main.go", "--json", "--profile", "nope")
	if code != 0 || strings.Contains(out, "/src/main.go") {
		t.Fatalf("files --profile: %q", out)
	}
	code, out, _ = runCLI(t, "files", "main.go", "--json")
	if code != 0 || !strings.Contains(out, `"tool": "Edit"`) || !strings.Contains(out, `"result_ok": 1`) {
		t.Fatalf("files json: %q", out)
	}
	code, out, _ = runCLI(t, "show", tsid[:8], "--changes")
	if code != 0 || !strings.Contains(out, "/src/main.go\n") || !strings.Contains(out, "Edit") || !strings.Contains(out, "quasarflux") {
		t.Fatalf("show --changes: %q", out)
	}
	code, out, _ = runCLI(t, "show", tsid[:8], "--n", "0")
	if code != 0 || !strings.Contains(out, "[tool output summary]") {
		t.Fatalf("show: %q", out)
	}
	code, out, _ = runCLI(t, "doctor", "--full")
	for _, want := range []string{"foreign_key_check: 0 violations", "tool_outputs coverage: 2 rows = 2 tool_result blocks", "sample verification:", "pointers: 0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor missing %q:\n%s", want, out)
		}
	}
	code, out, e = runCLI(t, "rebuild-extras")
	if code != 0 || !strings.Contains(out, "file_changes 1 -> 1") || !strings.Contains(out, "summaries kept 1, deleted 1") {
		t.Fatalf("rebuild-extras %d %q %q", code, out, e)
	}
	// restore round-trips the assets next to the transcript
	to := t.TempDir()
	code, out, e = runCLI(t, "restore", tsid[:8], "--to", to)
	if code != 0 {
		t.Fatalf("restore %d %s %s", code, out, e)
	}
	if _, err := os.Stat(filepath.Join(to, "proj", tsid, "tool-results", "tu2.txt")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(to, "proj", tsid+".jsonl"))
	if err != nil || fi.Mode().Perm() != portableMode(0o644) {
		t.Fatalf("mode %v %v", fi, err)
	}
	_ = root
}

func TestCLISummarizeOllamaDownAndLock(t *testing.T) {
	seedAssetsFixture(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	// ollama unreachable (seedRoot points at 127.0.0.1:1): manual run exits 1, nothing parked
	code, _, e := runCLI(t, "summarize")
	if code != 1 || !strings.Contains(e, "ollama unreachable") {
		t.Fatalf("%d %s", code, e)
	}
	st, _ := openStore()
	var n int
	st.DB().QueryRow(`SELECT count(*) FROM summaries`).Scan(&n)
	if n != 0 {
		t.Fatalf("parked %d", n)
	}
	// the hook-style (non-strict) path exits 0
	if c := runSummarize(st, summarizeOptsForTest(), "gemma4:e4b", time.Second, false, os.Stdout, os.Stderr); c != 0 {
		t.Fatalf("non-strict code %d", c)
	}
	st.Close()
}

func TestCLIHookArchivesSessionSubtreeAndMemory(t *testing.T) {
	root := seedAssetsFixture(t)
	p := filepath.Join(root, "proj", tsid+".jsonl")
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"%s","transcript_path":%q,"hook_event_name":"Stop"}`, tsid, p))
	defer func() { stdin = os.Stdin }()
	if code, _, e := runCLI(t, "ingest", "--stdin-hook"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	st, _ := openStore()
	defer st.Close()
	var kinds string
	st.DB().QueryRow(`SELECT group_concat(kind) FROM (SELECT kind FROM assets ORDER BY kind)`).Scan(&kinds)
	if kinds != "memory,tool-result" {
		t.Fatalf("assets %q", kinds)
	}
	var m int
	st.DB().QueryRow(`SELECT mode FROM files WHERE path=?`, p).Scan(&m)
	if m != int(portableMode(0o644)) {
		t.Fatalf("files.mode %o", m)
	}
	var ptr int
	st.DB().QueryRow(`SELECT count(*) FROM tool_outputs WHERE asset_id IS NOT NULL`).Scan(&ptr)
	_ = store.VecDim
}

func summarizeOptsForTest() summarize.Opts { return summarize.Opts{Parallel: 1} }

func TestCLISearchFindsPlans(t *testing.T) {
	root := seedAssetsFixture(t)
	p := filepath.Join(filepath.Dir(root), "plans", "calm-river.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("# Plan: ledger migration\n\nThe wombatledger tables are copied in batches before the cutover."), 0o644)
	srv := fakeOllama(t)
	t.Setenv("SESSION_INDEX_OLLAMA", srv.URL)
	if code, out, e := runCLI(t, "sweep", "--embed"); code != 0 {
		t.Fatalf("sweep %d %s %s", code, out, e)
	}
	profile, _ := ownProfile()
	code, out, _ := runCLI(t, "search", "wombatledger", "--mode", "fts", "--json")
	if code != 0 || !strings.Contains(out, `"kind": "plan"`) || !strings.Contains(out, "plans:"+profile) {
		t.Fatalf("plan search: %q", out)
	}
	if code, out, _ = runCLI(t, "search", "wombatledger", "--mode", "fts"); code != 0 || !strings.Contains(out, "[plan] calm-river.md") {
		t.Fatalf("plan marker: %q", out)
	}
	if code, out, _ = runCLI(t, "search", "wombatledger", "--mode", "fts", "--kind", "plan", "--json"); code != 0 || !strings.Contains(out, `"kind": "plan"`) {
		t.Fatalf("--kind plan: %q", out)
	}
	if code, out, _ = runCLI(t, "search", "wombatledger batches cutover", "--mode", "vector", "--json"); code != 0 || !strings.Contains(out, `"kind": "plan"`) {
		t.Fatalf("vector search: %q", out)
	}
	st, _ := openStore()
	defer st.Close()
	var n int
	st.DB().QueryRow(`SELECT count(*) FROM chunks WHERE kind='plan' AND embedded_at IS NOT NULL`).Scan(&n)
	if n != 1 {
		t.Fatalf("embedded plan chunks %d", n)
	}
}

func TestCLIHookArchivesProfileDirs(t *testing.T) {
	root := seedAssetsFixture(t)
	prof := filepath.Dir(root)
	for rel, c := range map[string]string{
		filepath.Join("plans", "p.md"):                       "# plan",
		filepath.Join("paste-cache", "0123456789abcdef.txt"): "pasted",
		filepath.Join("tasks", tsid, "1.json"):               "{}",
		filepath.Join("tasks", tsid, ".lock"):                "",
		filepath.Join("tasks", "other", "1.json"):            "{}",
		filepath.Join("uploads", tsid, "a.png"):              "png",
		filepath.Join("uploads", "other", "b.png"):           "png",
	} {
		p := filepath.Join(prof, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	p := filepath.Join(root, "proj", tsid+".jsonl")
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"%s","transcript_path":%q,"hook_event_name":"Stop"}`, tsid, p))
	defer func() { stdin = os.Stdin }()
	if code, _, e := runCLI(t, "ingest", "--stdin-hook"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	st, _ := openStore()
	defer st.Close()
	got := map[string]string{} // path relative to the profile -> kind
	rows, err := st.DB().Query(`SELECT path, kind || ':' || COALESCE(session_id,'') FROM assets WHERE kind IN ('plans','paste-cache','tasks','uploads')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var path, v string
		rows.Scan(&path, &v)
		rel, _ := filepath.Rel(prof, path)
		got[rel] = v
	}
	rows.Close()
	// the pseudo-session id carries this machine's profile name
	profile, _ := ownProfile()
	want := map[string]string{
		filepath.Join("plans", "p.md"):                       "plans:plans:" + profile,
		filepath.Join("paste-cache", "0123456789abcdef.txt"): "paste-cache:paste-cache:" + profile,
		filepath.Join("tasks", tsid, "1.json"):               "tasks:" + tsid,
		filepath.Join("uploads", tsid, "a.png"):              "uploads:" + tsid,
	}
	if len(got) != len(want) {
		t.Fatalf("assets %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
}
