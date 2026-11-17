package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/store"
)

// fakeOpenAI serves an OpenAI-compatible API with dims-sized embeddings.
func fakeOpenAI(t *testing.T, dims int, auth *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth != nil {
			*auth = append(*auth, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/embeddings":
			var req struct {
				Input []string `json:"input"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			data := make([]map[string]any, len(req.Input))
			for i := range data {
				v := make([]float32, dims)
				v[i%dims] = 1
				data[i] = map[string]any{"index": i, "embedding": v}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Build output listing zorblax lines."}}]}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"emb-model"},{"id":"chat-model"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useConfig points SESSION_INDEX_CONFIG at a new file with body and clears
// the provider environment overrides.
func useConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if body != "" {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SESSION_INDEX_CONFIG", p)
	for _, k := range []string{"EMBED", "SUMMARIZE"} {
		for _, f := range []string{"PROVIDER", "MODEL", "BASE_URL"} {
			t.Setenv("SESSION_INDEX_"+k+"_"+f, "")
		}
	}
	return p
}

func metaOf(t *testing.T, key string) string {
	t.Helper()
	st, err := store.OpenReadOnly(dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var v string
	if err := st.DB().QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v); err != nil {
		t.Fatalf("meta %s: %v", key, err)
	}
	return v
}

func TestCLIConfigPrintsEffectiveDefaults(t *testing.T) {
	useConfig(t, "")
	t.Setenv("SESSION_INDEX_OLLAMA", "")
	code, out, e := runCLI(t, "config")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	var m struct {
		Embed struct {
			Provider, Model string
			BaseURL         string `json:"base_url"`
			Dims            int
			DocPrefix       string `json:"doc_prefix"`
			QueryPrefix     string `json:"query_prefix"`
		}
		Summarize struct {
			Provider, Model string
			BaseURL         string `json:"base_url"`
			MaxTokens       int    `json:"max_tokens"`
			Temperature     float64
			Enabled         bool
		}
	}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if m.Embed.Provider != "ollama" || m.Embed.BaseURL != "http://localhost:11434" || m.Embed.Model != "embeddinggemma" || m.Embed.Dims != 768 ||
		m.Embed.DocPrefix != "title: none | text: " || m.Embed.QueryPrefix != "task: search result | query: " ||
		m.Summarize.Provider != "ollama" || m.Summarize.Model != "gemma4:e4b" || m.Summarize.MaxTokens != 120 || m.Summarize.Temperature != 0.2 || !m.Summarize.Enabled {
		t.Fatalf("%s", out)
	}
}

func TestCLIConfigInit(t *testing.T) {
	p := useConfig(t, "")
	if code, out, e := runCLI(t, "config", "init"); code != 0 || !strings.Contains(out, p) {
		t.Fatalf("%d %s %s", code, out, e)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"_comment"`) {
		t.Fatalf("no _comment: %s", b)
	}
	if code, _, e := runCLI(t, "config", "init"); code != 1 || !strings.Contains(e, "--force") {
		t.Fatalf("overwrote without --force: %d %s", code, e)
	}
	if code, _, _ := runCLI(t, "config", "init", "--force"); code != 0 {
		t.Fatal("--force failed")
	}
	if code, _, e := runCLI(t, "config"); code != 0 {
		t.Fatalf("init file does not load: %d %s", code, e)
	}
}

func TestCLIOpenAIEmbedAutoDetectsDimsOnNewArchive(t *testing.T) {
	seedRoot(t)
	var auth []string
	srv := fakeOpenAI(t, 8, &auth)
	t.Setenv("SA_TEST_KEY", "k-123")
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","api_key_env":"SA_TEST_KEY"}}`, srv.URL))
	code, out, e := runCLI(t, "sweep", "--embed")
	if code != 0 || !strings.Contains(out, "embedded") {
		t.Fatalf("sweep %d %s %s", code, out, e)
	}
	if metaOf(t, "vec_dims") != "8" || metaOf(t, "embedder_fingerprint") != "openai|emb-model|8||v1" {
		t.Fatalf("meta %s %s", metaOf(t, "vec_dims"), metaOf(t, "embedder_fingerprint"))
	}
	for _, a := range auth {
		if a != "Bearer k-123" {
			t.Fatalf("auth %q", a)
		}
	}
	code, out, e = runCLI(t, "search", "zebrafish", "--mode", "vector", "--json")
	if code != 0 || !strings.Contains(out, `"hits"`) || strings.Contains(out, `"hits": []`) {
		t.Fatalf("vector search %d %s %s", code, out, e)
	}
}

func TestCLIDimsMismatchThenRebuildDerived(t *testing.T) {
	seedRoot(t)
	useConfig(t, "")
	t.Setenv("SESSION_INDEX_OLLAMA", "http://127.0.0.1:1")
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	if metaOf(t, "vec_dims") != "768" {
		t.Fatal("default archive is not 768")
	}
	srv := fakeOpenAI(t, 16, nil)
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","dims":16}}`, srv.URL))
	code, _, e := runCLI(t, "embed")
	if code != 1 || !strings.Contains(e, "768") || !strings.Contains(e, "16") || !strings.Contains(e, "rebuild-derived") {
		t.Fatalf("embed with mismatching dims: %d %q", code, e)
	}
	// doctor still opens the archive and reports the mismatch
	if code, out, _ := runCLI(t, "doctor"); code != 1 || !strings.Contains(out, "MISMATCH") {
		t.Fatalf("doctor %d %s", code, out)
	}
	code, out, e := runCLI(t, "rebuild-derived")
	if code != 0 || !strings.Contains(out, "16 dims") {
		t.Fatalf("rebuild-derived %d %s %s", code, out, e)
	}
	if metaOf(t, "vec_dims") != "16" || metaOf(t, "embedder_fingerprint") != "openai|emb-model|16||v1" {
		t.Fatalf("meta %s %s", metaOf(t, "vec_dims"), metaOf(t, "embedder_fingerprint"))
	}
	if code, out, e := runCLI(t, "embed"); code != 0 || !strings.Contains(out, "embedded") {
		t.Fatalf("embed after rebuild %d %s %s", code, out, e)
	}
}

func TestCLIDoctorShowsProvidersAndKeyPresenceNotValue(t *testing.T) {
	seedRoot(t)
	srv := fakeOpenAI(t, 8, nil)
	t.Setenv("SA_TEST_KEY", "top-secret-value")
	t.Setenv("SA_TEST_UNSET", "")
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","dims":8,"api_key_env":"SA_TEST_KEY"},
	 "summarize":{"provider":"openai","base_url":%q,"model":"chat-model","api_key_env":"SA_TEST_UNSET"}}`, srv.URL, srv.URL))
	_, out, _ := runCLI(t, "doctor")
	for _, want := range []string{
		"embed: provider=openai base_url=" + srv.URL + " model=emb-model dims=8",
		"embed api key env: SA_TEST_KEY: set",
		"embed health: ok",
		"summarize: provider=openai base_url=" + srv.URL + " model=chat-model",
		"summarize api key env: SA_TEST_UNSET: unset",
		"summarize health: ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "top-secret-value") {
		t.Fatal("doctor printed the key value")
	}
}

func TestCLISummarizeThroughOpenAIAndDisabled(t *testing.T) {
	seedAssetsFixture(t)
	srv := fakeOpenAI(t, 8, nil)
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model"},
	 "summarize":{"provider":"openai","base_url":%q,"model":"chat-model","max_items_per_run":1}}`, srv.URL, srv.URL))
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %s", e)
	}
	code, out, e := runCLI(t, "summarize")
	if code != 0 || !strings.Contains(out, "summarized 1 items") || !strings.Contains(out, "model openai:chat-model") {
		t.Fatalf("summarize %d %q %q", code, out, e)
	}
	st, _ := store.OpenReadOnly(dbPath())
	defer st.Close()
	var model string
	if err := st.DB().QueryRow(`SELECT model FROM summaries WHERE model NOT LIKE 'failed:%' LIMIT 1`).Scan(&model); err != nil || model != "openai:chat-model" {
		t.Fatalf("summaries.model = %q (%v)", model, err)
	}
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model"},"summarize":{"enabled":false}}`, srv.URL))
	code, out, e = runCLI(t, "summarize")
	if code != 0 || !strings.Contains(e, "disabled") || strings.Contains(out, "summarized") {
		t.Fatalf("disabled: %d %q %q", code, out, e)
	}
}

func countOf(t *testing.T, q string) int {
	t.Helper()
	st, err := store.OpenReadOnly(dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func unauthorized(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	t.Cleanup(srv.Close)
	return srv
}

func TestCLIEmbedAuthFailureExitsOneAndParksNothing(t *testing.T) {
	seedRoot(t)
	srv := unauthorized(t)
	t.Setenv("SA_TEST_KEY", "")
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","dims":8,"api_key_env":"SA_TEST_KEY"}}`, srv.URL))
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	pending := countOf(t, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`)
	if pending == 0 {
		t.Fatal("no chunks to embed")
	}
	code, _, e := runCLI(t, "embed")
	want := "authentication failed (HTTP 401) for openai at " + srv.URL + "; check SA_TEST_KEY (unset)"
	if code != 1 || !strings.Contains(e, want) {
		t.Fatalf("embed %d %q, want %q", code, e, want)
	}
	if got := countOf(t, `SELECT count(*) FROM chunks WHERE embedded_at IS NULL`); got != pending {
		t.Fatalf("pending %d -> %d: rows were parked or embedded", pending, got)
	}
	// a non-strict path (sweep --embed) exits 1 too
	if code, _, e := runCLI(t, "sweep", "--embed"); code != 1 || !strings.Contains(e, want) {
		t.Fatalf("sweep --embed %d %q", code, e)
	}
	code, _, e = runCLI(t, "search", "zebrafish", "--mode", "vector")
	if code != 1 || !strings.Contains(e, want) {
		t.Fatalf("search %d %q", code, e)
	}
	_, out, _ := runCLI(t, "doctor")
	if !strings.Contains(out, "embed health: FAILED: "+want) {
		t.Fatalf("doctor:\n%s", out)
	}
}

func TestCLISummarizeAuthFailureExitsOne(t *testing.T) {
	seedAssetsFixture(t)
	srv := unauthorized(t)
	t.Setenv("SA_TEST_KEY", "x")
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","dims":8},
	 "summarize":{"provider":"openai","base_url":%q,"model":"chat-model","api_key_env":"SA_TEST_KEY"}}`, srv.URL, srv.URL))
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %s", e)
	}
	code, _, e := runCLI(t, "summarize")
	if code != 1 || !strings.Contains(e, "authentication failed (HTTP 401) for openai at "+srv.URL+"; check SA_TEST_KEY (set)") {
		t.Fatalf("%d %q", code, e)
	}
	if n := countOf(t, `SELECT count(*) FROM summaries`); n != 0 {
		t.Fatalf("%d summaries parked", n)
	}
}

func TestCLIArchivingIgnoresMismatchedOrInvalidConfig(t *testing.T) {
	root, _ := seedRoot(t)
	useConfig(t, "")
	t.Setenv("SESSION_INDEX_OLLAMA", "http://127.0.0.1:1")
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	before := countOf(t, `SELECT count(*) FROM chunks`)
	add := func() {
		f, _ := os.OpenFile(filepath.Join(root, "proj", tsid+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, `{"type":"user","uuid":"u%d","sessionId":"%s","timestamp":"2026-01-02T00:00:01Z","message":{"role":"user","content":"Another long enough message about narwhal telemetry pipelines %d"}}`+"\n", before, tsid, before)
		f.Close()
	}
	// dims mismatch: archive is 768, config wants 16
	useConfig(t, `{"embed":{"provider":"openai","base_url":"http://127.0.0.1:1","model":"emb-model","dims":16}}`)
	add()
	code, _, e := runCLI(t, "sweep") // the note goes to the process stderr
	if code != 0 {
		t.Fatalf("mismatched sweep %d %q", code, e)
	}
	mid := countOf(t, `SELECT count(*) FROM chunks`)
	if mid <= before {
		t.Fatalf("nothing archived under a mismatched config: %d -> %d", before, mid)
	}
	// invalid config file
	p := useConfig(t, `{`)
	add()
	code, _, e = runCLI(t, "sweep")
	if code != 0 {
		t.Fatalf("invalid-config sweep %d %q", code, e)
	}
	if countOf(t, `SELECT count(*) FROM chunks`) <= mid {
		t.Fatal("nothing archived under an invalid config")
	}
	for _, args := range [][]string{{"stats"}, {"files", "main"}} {
		if code, _, e := runCLI(t, args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, e)
		}
	}
	// embed and summarize are strict
	if code, _, e := runCLI(t, "embed"); code != 1 || !strings.Contains(e, p) {
		t.Fatalf("embed %d %q", code, e)
	}
	// doctor still reports, naming the config error
	code, out, _ := runCLI(t, "doctor")
	if code != 1 || !strings.Contains(out, "config: FAILED:") || !strings.Contains(out, "quick_check: ok") || !strings.Contains(out, "hooks:") {
		t.Fatalf("doctor %d\n%s", code, out)
	}
}

func TestCLINewArchiveRefusedUnderInvalidConfig(t *testing.T) {
	_, db := seedRoot(t)
	p := useConfig(t, `{"embed":`)
	code, _, e := runCLI(t, "sweep")
	if code != 1 || !strings.Contains(e, "open archive") || !strings.Contains(e, p) || !strings.Contains(e, "unexpected") {
		t.Fatalf("%d %q", code, e)
	}
	if _, err := os.Stat(db); err == nil {
		t.Fatal("archive created under an invalid config")
	}
}

func TestCLIProbeFailureNoteOnNewArchive(t *testing.T) {
	seedRoot(t)
	useConfig(t, `{"embed":{"provider":"openai","base_url":"http://127.0.0.1:1","model":"unknown-m"}}`)
	var notes bytes.Buffer
	old := noteWriter
	noteWriter = &notes
	defer func() { noteWriter = old }()
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	want := "creating chunk_vec with 768 dims and fingerprint openai|unknown-m|768||v1 (provider probe failed:"
	if !strings.Contains(notes.String(), want) {
		t.Fatalf("note %q lacks %q", notes.String(), want)
	}
	if metaOf(t, "vec_dims") != "768" || metaOf(t, "embedder_fingerprint") != "openai|unknown-m|768||v1" {
		t.Fatal("meta not as announced")
	}
}

func TestCLISearchDimsMismatchFallsBackToFTS(t *testing.T) {
	seedRoot(t)
	useConfig(t, "")
	t.Setenv("SESSION_INDEX_OLLAMA", "http://127.0.0.1:1")
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %s", e)
	}
	useConfig(t, `{"embed":{"provider":"openai","base_url":"http://127.0.0.1:1","model":"emb-model","dims":16}}`)
	code, out, e := runCLI(t, "search", "zebrafish", "--json")
	if code != 0 || !strings.Contains(e, "rebuild-derived") || !strings.Contains(out, `"route": "fts-fallback"`) || strings.Contains(out, `"hits": []`) {
		t.Fatalf("auto: %d\n%s\n%s", code, out, e)
	}
	if code, _, e := runCLI(t, "search", "zebrafish", "--mode", "hybrid"); code != 0 || !strings.Contains(e, "rebuild-derived") {
		t.Fatalf("hybrid: %d %s", code, e)
	}
	if code, _, e := runCLI(t, "search", "zebrafish", "--mode", "vector"); code != 1 || !strings.Contains(e, "rebuild-derived") {
		t.Fatalf("vector: %d %s", code, e)
	}
}

func TestCLIIngestUnderMismatchedConfigArchives(t *testing.T) {
	root, _ := seedRoot(t)
	useConfig(t, "")
	t.Setenv("SESSION_INDEX_OLLAMA", "http://127.0.0.1:1")
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %s", e)
	}
	before := countOf(t, `SELECT count(*) FROM chunks`)
	useConfig(t, `{"embed":{"provider":"openai","base_url":"http://127.0.0.1:1","model":"emb-model","dims":16}}`)
	sid2 := "33333333-3333-3333-3333-333333333333"
	p := filepath.Join(root, "proj", sid2+".jsonl")
	line := fmt.Sprintf(`{"type":"user","uuid":"x1","sessionId":"%s","timestamp":"2026-01-03T00:00:01Z","cwd":"/w","message":{"role":"user","content":"A second session about narwhal telemetry pipelines and their retention"}}`, sid2) + "\n"
	os.WriteFile(p, []byte(line), 0o644)
	if code, _, e := runCLI(t, "ingest", "--transcript", p); code != 0 {
		t.Fatalf("ingest %d %s", code, e)
	}
	if countOf(t, `SELECT count(*) FROM chunks`) <= before {
		t.Fatal("ingest archived nothing under a mismatched config")
	}
}

func TestHookPathAuthFailureReachesEmbedLog(t *testing.T) {
	root, db := seedRoot(t)
	srv := unauthorized(t)
	t.Setenv("SA_TEST_KEY", "")
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","dims":8,"api_key_env":"SA_TEST_KEY"}}`, srv.URL))
	// stand-in for the detached child: run `embed` with its output in embed.log
	old := spawnEmbed
	spawnEmbed = func(st *store.Store) error {
		lf, err := os.OpenFile(filepath.Join(filepath.Dir(st.Path()), "embed.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer lf.Close()
		run([]string{"embed"}, lf, lf)
		return nil
	}
	defer func() { spawnEmbed = old }()
	p := filepath.Join(root, "proj", tsid+".jsonl")
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"%s","transcript_path":%q,"hook_event_name":"Stop"}`, tsid, p))
	defer func() { stdin = os.Stdin }()
	if code, _, e := runCLI(t, "ingest", "--stdin-hook", "--embed"); code != 0 {
		t.Fatalf("hook exit %d %s", code, e)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(db), "embed.log"))
	want := "authentication failed (HTTP 401) for openai at " + srv.URL + "; check SA_TEST_KEY (unset)"
	if err != nil || !strings.Contains(string(b), want) {
		t.Fatalf("embed.log %q (%v) lacks %q", b, err, want)
	}
}

func TestCLISummarizeRetryFailed(t *testing.T) {
	seedAssetsFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	t.Cleanup(srv.Close)
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","dims":8},"summarize":{"provider":"openai","base_url":%q,"model":"chat-model"}}`, srv.URL, srv.URL))
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %s", e)
	}
	if code, out, e := runCLI(t, "summarize"); code != 0 || !strings.Contains(out, "(3 failed)") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if code, out, e := runCLI(t, "summarize"); code != 0 || !strings.Contains(out, "summarized 0 items") {
		t.Fatalf("parked items retried: %d %q %q", code, out, e)
	}
	code, out, e := runCLI(t, "summarize", "--retry-failed", "http 400")
	if code != 0 || !strings.Contains(out, "retry-failed: removed 3 parked failures") || !strings.Contains(out, "(3 failed)") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if code, _, _ := runCLI(t, "summarize", "extra-arg"); code != 2 {
		t.Fatal("stray argument accepted")
	}
}

func TestCLISummarizeDailyCapStopsExitsZeroAndDoctorShowsUsage(t *testing.T) {
	seedAssetsFixture(t)
	srv := fakeOpenAI(t, 8, nil)
	useConfig(t, fmt.Sprintf(`{"embed":{"provider":"openai","base_url":%q,"model":"emb-model","dims":8,"max_items_per_day":3},
	 "summarize":{"provider":"openai","base_url":%q,"model":"chat-model","max_items_per_day":1}}`, srv.URL, srv.URL))
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %s", e)
	}
	code, out, e := runCLI(t, "summarize")
	if code != 0 || !strings.Contains(out, "summarized 1 items") || !strings.Contains(out, "daily cap reached: 1 of 1 summaries today") || !strings.Contains(out, "remaining items stay pending") {
		t.Fatalf("first run %d %q %q", code, out, e)
	}
	pending := countOf(t, `SELECT count(*) FROM tool_outputs`)
	code, out, e = runCLI(t, "summarize")
	if code != 0 || !strings.Contains(out, "summarized 0 items") || !strings.Contains(out, "daily cap reached") {
		t.Fatalf("second run %d %q %q", code, out, e)
	}
	if countOf(t, `SELECT count(*) FROM summaries WHERE model='failed:%'`) != 0 || pending == 0 {
		t.Fatal("capped items must stay pending, not parked")
	}
	code, out, e = runCLI(t, "embed")
	t.Logf("embed: %q %q", out, e)
	if code != 0 || !strings.Contains(out, "embedded 3 chunks; embed: daily cap reached: 3 of 3 chunks embedded today") {
		t.Fatalf("embed %d %q %q", code, out, e)
	}
	_, out, _ = runCLI(t, "doctor")
	day := time.Now().UTC().Format("2006-01-02")
	for _, want := range []string{
		"summarize usage today (UTC " + day + "): 1 / 1 (configured)",
		"embed usage today (UTC " + day + "): ",
		"/ 3 (configured)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
}

func TestCLISummarizeKindsFromConfig(t *testing.T) {
	seedAssetsFixture(t)
	srv := fakeOpenAI(t, 8, nil)
	useConfig(t, fmt.Sprintf(`{"summarize":{"provider":"openai","base_url":%q,"model":"chat-model","kinds":["asset"]}}`, srv.URL))
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %s", e)
	}
	toolsBefore := countOf(t, `SELECT count(*) FROM summaries WHERE record_id IS NOT NULL`)
	if code, out, e := runCLI(t, "summarize"); code != 0 || !strings.Contains(out, "summarized") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if countOf(t, `SELECT count(*) FROM summaries WHERE asset_id IS NOT NULL`) == 0 || countOf(t, `SELECT count(*) FROM summaries WHERE record_id IS NOT NULL`) != toolsBefore ||
		countOf(t, `SELECT count(*) FROM summaries WHERE change_id IS NOT NULL`) != 0 {
		t.Fatal("kinds=[asset] must summarize only assets")
	}
	useConfig(t, fmt.Sprintf(`{"summarize":{"provider":"openai","base_url":%q,"model":"chat-model","kinds":["nonsense"]}}`, srv.URL))
	if code, _, e := runCLI(t, "summarize"); code != 1 || !strings.Contains(e, "nonsense") {
		t.Fatalf("bad kinds: %d %q", code, e)
	}
}

func TestCLIEmbedAnthropicIsConfigErrorWithHint(t *testing.T) {
	seedRoot(t)
	useConfig(t, `{"embed":{"provider":"anthropic","model":"x"}}`)
	code, out, e := runCLI(t, "embed")
	if code != 1 || !strings.Contains(e+out, "no embeddings endpoint") || !strings.Contains(e+out, "https://api.voyageai.com") {
		t.Fatalf("%d %q %q", code, out, e)
	}
}
