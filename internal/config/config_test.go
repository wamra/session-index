package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPath(t *testing.T) {
	if got := Path("/h", env(nil)); got != filepath.FromSlash("/h/.config/session-index/config.json") {
		t.Fatal(got)
	}
	if got := Path("/h", env(map[string]string{"SESSION_INDEX_CONFIG": "/x/c.json"})); got != "/x/c.json" {
		t.Fatal(got)
	}
}

func TestMissingFileGivesTodaysDefaults(t *testing.T) {
	c, found, err := Load(filepath.Join(t.TempDir(), "none.json"), env(nil))
	if err != nil || found {
		t.Fatal(err, found)
	}
	r := c.Resolved()
	if r.Validate() != nil || r.Embed.Provider != "ollama" || r.Embed.BaseURL != "http://localhost:11434" || r.Embed.Model != "embeddinggemma" ||
		r.Embed.Dims != 768 || r.Summarize.Provider != "ollama" || r.Summarize.BaseURL != "http://localhost:11434" || r.Summarize.Model != "gemma4:e4b" ||
		r.Summarize.MaxTokens != 120 || *r.Summarize.Temperature != 0.2 || !*r.Summarize.Enabled {
		t.Fatalf("%+v", r)
	}
}

func TestFileThenEnvPrecedence(t *testing.T) {
	p := write(t, `{"embed":{"provider":"openai","base_url":"http://file:1","model":"file-model","dims":16},
	  "summarize":{"provider":"openai","base_url":"http://file:2","model":"file-sum","max_tokens":50,"enabled":false,"max_items_per_run":7}}`)
	c, found, err := Load(p, env(nil))
	if err != nil || !found {
		t.Fatal(err)
	}
	r := c.Resolved()
	if r.Embed.BaseURL != "http://file:1" || r.Embed.Model != "file-model" || r.Embed.Dims != 16 ||
		r.Summarize.Model != "file-sum" || r.Summarize.MaxTokens != 50 || *r.Summarize.Enabled || r.Summarize.MaxItemsPerRun != 7 {
		t.Fatalf("file only: %+v", r)
	}
	c, _, err = Load(p, env(map[string]string{
		"SESSION_INDEX_EMBED_PROVIDER": "ollama", "SESSION_INDEX_EMBED_MODEL": "env-model", "SESSION_INDEX_EMBED_BASE_URL": "http://env:1",
		"SESSION_INDEX_SUMMARIZE_PROVIDER": "ollama", "SESSION_INDEX_SUMMARIZE_MODEL": "env-sum", "SESSION_INDEX_SUMMARIZE_BASE_URL": "http://env:2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	r = c.Resolved()
	if r.Embed.Provider != "ollama" || r.Embed.Model != "env-model" || r.Embed.BaseURL != "http://env:1" || r.Embed.Dims != 16 ||
		r.Summarize.Provider != "ollama" || r.Summarize.Model != "env-sum" || r.Summarize.BaseURL != "http://env:2" || r.Summarize.MaxTokens != 50 {
		t.Fatalf("env over file: %+v", r)
	}
}

func TestOllamaEnvAliasOnlyForOllamaProvider(t *testing.T) {
	c, _, _ := Load(filepath.Join(t.TempDir(), "none.json"), env(map[string]string{"SESSION_INDEX_OLLAMA": "http://alias:9"}))
	r := c.Resolved()
	if r.Embed.BaseURL != "http://alias:9" || r.Summarize.BaseURL != "http://alias:9" {
		t.Fatalf("%+v", r)
	}
	// the specific variable beats the alias
	c, _, _ = Load(filepath.Join(t.TempDir(), "none.json"), env(map[string]string{"SESSION_INDEX_OLLAMA": "http://alias:9", "SESSION_INDEX_EMBED_BASE_URL": "http://spec:1"}))
	if r := c.Resolved(); r.Embed.BaseURL != "http://spec:1" || r.Summarize.BaseURL != "http://alias:9" {
		t.Fatalf("%+v", r)
	}
	// a section whose provider is openai ignores the alias
	p := write(t, `{"embed":{"provider":"openai","model":"m"}}`)
	c, _, _ = Load(p, env(map[string]string{"SESSION_INDEX_OLLAMA": "http://alias:9"}))
	if r := c.Resolved(); r.Embed.BaseURL != "https://api.openai.com" || r.Summarize.BaseURL != "http://alias:9" {
		t.Fatalf("%+v", r)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, body := range map[string]string{"bad json": `{`, "unknown field": `{"embed":{"modle":"x"}}`} {
		if _, _, err := Load(write(t, body), env(nil)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c, _, _ := Load(write(t, `{"embed":{"provider":"openai"}}`), env(nil))
	if err := c.Resolved().Validate(); err == nil || !strings.Contains(err.Error(), "embed.model") {
		t.Fatalf("%v", err)
	}
}

func TestEffectiveMasksKeys(t *testing.T) {
	p := write(t, `{"embed":{"provider":"openai","model":"text-embedding-3-small","api_key_env":"SA_K"}}`)
	g := env(map[string]string{"SA_K": "super-secret-value"})
	c, _, _ := Load(p, g)
	out := string(Effective(c, p, g))
	if strings.Contains(out, "super-secret-value") {
		t.Fatal("key value leaked")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if m["_embed_api_key"] != "set" || m["_summarize_api_key"] != "none" || m["embed"].(map[string]any)["dims"] != float64(1536) {
		t.Fatalf("%s", out)
	}
	out = string(Effective(c, p, env(nil)))
	if !strings.Contains(out, `"_embed_api_key": "unset"`) {
		t.Fatal(out)
	}
}

func TestInitFileLoadsBackToDefaults(t *testing.T) {
	p := write(t, string(Init()))
	if !strings.Contains(string(Init()), `"_comment"`) {
		t.Fatal("no _comment")
	}
	c, found, err := Load(p, env(nil))
	if err != nil || !found {
		t.Fatal(err)
	}
	d, _, _ := Load(filepath.Join(t.TempDir(), "none.json"), env(nil))
	a, _ := json.Marshal(c.Resolved().out(""))
	b, _ := json.Marshal(d.Resolved().out(""))
	if string(a) != string(b) {
		t.Fatalf("init file differs from defaults:\n%s\n%s", a, b)
	}
}

func TestInitOmitsPrefixesForKnownModel(t *testing.T) {
	b := string(Init())
	// the comment may mention the key names; the embed object must not carry them
	var m struct {
		Embed map[string]any `json:"embed"`
	}
	if err := json.Unmarshal([]byte(b), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Embed["doc_prefix"]; ok {
		t.Fatalf("doc_prefix written for a known model:\n%s", b)
	}
	if _, ok := m.Embed["query_prefix"]; ok {
		t.Fatal("query_prefix written for a known model")
	}
	if m.Embed["model"] != "embeddinggemma" || m.Embed["dims"] != float64(768) {
		t.Fatalf("%v", m.Embed)
	}
}

func TestAnthropicEmbedRejectedWithHint(t *testing.T) {
	p := write(t, `{"embed":{"provider":"anthropic","model":"x"}}`)
	c, _, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Resolved().Validate()
	if err == nil || !strings.Contains(err.Error(), "no embeddings endpoint") || !strings.Contains(err.Error(), "https://api.voyageai.com") {
		t.Fatalf("err = %v", err)
	}
}

func TestCloudSectionsLoadAndCapsResolve(t *testing.T) {
	p := write(t, `{"embed":{"provider":"bedrock","model":"amazon.titan-embed-text-v2:0","dims":512,"region":"us-east-1","profile":"work","max_items_per_day":-1},
	"summarize":{"provider":"anthropic","max_input_chars":4000,"kinds":["asset","file_change"]}}`)
	c, _, err := Load(p, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Resolved()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.Embed.Region != "us-east-1" || r.Embed.Profile != "work" || r.Embed.Dims != 512 || r.Summarize.Model != "claude-haiku-4-5" || r.Summarize.MaxInputChars != 4000 {
		t.Fatalf("%+v", r)
	}
	if n, src := r.Embed.DailyCap(); n != 0 || !strings.Contains(src, "unlimited") {
		t.Fatalf("embed cap %d %s", n, src)
	}
	if n, src := r.Summarize.DailyCap(); n != 2000 || src != "hosted default" {
		t.Fatalf("summarize cap %d %s", n, src)
	}
	eff := string(Effective(c, "test", env(nil)))
	if !strings.Contains(eff, `"_summarize_daily_cap": "2000 (hosted default)"`) || !strings.Contains(eff, `"_embed_daily_cap": "unlimited (configured)"`) {
		t.Fatalf("%s", eff)
	}
	bad := write(t, `{"summarize":{"kinds":["tool_output","oops"]}}`)
	c, _, _ = Load(bad, env(nil))
	if err := c.Resolved().Validate(); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("err = %v", err)
	}
}

func TestLocalDefaultsStayUnlimitedAndInitHasExamples(t *testing.T) {
	r := Config{}.Resolved()
	if a, _ := r.Embed.DailyCap(); a != 0 {
		t.Fatal("local embed must be unlimited")
	}
	if a, _ := r.Summarize.DailyCap(); a != 0 {
		t.Fatal("local summarize must be unlimited")
	}
	init := string(Init())
	for _, want := range []string{"anthropic", "amazon.titan-embed-text-v2:0", "voyage-3", "api.openai.com", "LM Studio", "max_items_per_day"} {
		if !strings.Contains(init, want) {
			t.Errorf("config init comment lacks %q", want)
		}
	}
	if strings.Contains(init, `"region"`) || strings.Contains(init, `"kinds"`) {
		t.Fatal("unset cloud fields must not appear in the default file")
	}
}

func TestPathsDefaultsFollowClaudeDir(t *testing.T) {
	p := Config{}.ResolvePaths("/h", env(nil))
	want := Paths{
		DB:        filepath.FromSlash("/h/.claude/cache/session-archive/archive.sqlite"),
		ClaudeDir: filepath.FromSlash("/h/.claude"),
		Root:      filepath.FromSlash("/h/.claude/projects"),
		KeyDir:    filepath.FromSlash("/h/.config/session-index/keys"),
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("%+v", p)
	}
	c := Config{Paths: Paths{ClaudeDir: "/p/workstation"}}
	q := c.ResolvePaths("/h", env(nil))
	if q.DB != filepath.FromSlash("/p/workstation/cache/session-archive/archive.sqlite") || q.Root != filepath.FromSlash("/p/workstation/projects") {
		t.Fatalf("%+v", q)
	}
}

func TestPathsFilePrecedenceTildeAndEnv(t *testing.T) {
	c := Config{Paths: Paths{DB: "~/db/a.sqlite", KeyDir: "~/keys", Peers: []string{"~/a.sqlite", "/b.sqlite"}}}
	p := c.ResolvePaths("/h", env(nil))
	if p.DB != filepath.FromSlash("/h/db/a.sqlite") || p.KeyDir != filepath.FromSlash("/h/keys") ||
		!reflect.DeepEqual(p.Peers, []string{filepath.FromSlash("/h/a.sqlite"), "/b.sqlite"}) {
		t.Fatalf("%+v", p)
	}
	e := env(map[string]string{"SESSION_INDEX_DB": "/env/a.sqlite", "SESSION_INDEX_ROOT": "/env/root",
		"SESSION_INDEX_KEY_DIR": "/env/k", "SESSION_INDEX_PEERS": "/x" + string(os.PathListSeparator) + "/y"})
	p = c.ResolvePaths("/h", e)
	if p.DB != "/env/a.sqlite" || p.Root != "/env/root" || p.KeyDir != "/env/k" || !reflect.DeepEqual(p.Peers, []string{"/x", "/y"}) {
		t.Fatalf("%+v", p)
	}
	// the legacy SESSION_ARCHIVE_* names still work
	if got := c.ResolvePaths("/h", env(map[string]string{"SESSION_ARCHIVE_DB": "/old/a.sqlite"})).DB; got != "/old/a.sqlite" {
		t.Fatal(got)
	}
}

func TestPathsSectionRejectsUnknownKeysAndLoadsBack(t *testing.T) {
	if _, _, err := Load(write(t, `{"paths":{"dbb":"x"}}`), env(nil)); err == nil {
		t.Fatal("unknown key in paths must be rejected")
	}
	c, _, err := Load(write(t, `{"paths":{"db":"/d.sqlite","peers":["/p.sqlite"]},"search":{"rrf_k":30},"ingest":{"backoff_seconds":5}}`), env(nil))
	if err != nil || c.Paths.DB != "/d.sqlite" || c.Search.RRFK != 30 || c.Ingest.BackoffSeconds != 5 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestLoadPathsIgnoresABrokenEmbedSection(t *testing.T) {
	p := write(t, `{"paths":{"db":"/where/a.sqlite"},"embed":{"provider":"nope"},"future_key":1}`)
	got, found, err := LoadPaths(p)
	if err != nil || !found || got.DB != "/where/a.sqlite" {
		t.Fatalf("%+v %v %v", got, found, err)
	}
	if _, _, err := Load(p, env(nil)); err == nil {
		t.Fatal("the strict load must still reject the file")
	}
	if _, found, err := LoadPaths(filepath.Join(t.TempDir(), "none.json")); err != nil || found {
		t.Fatal(found, err)
	}
	if _, _, err := LoadPaths(write(t, `{"paths":`)); err == nil {
		t.Fatal("a syntax error must be an error, not silent defaults")
	}
}

func TestTuningDefaultsAndValidation(t *testing.T) {
	r := Config{}.Resolved()
	if r.Search.DefaultK != 10 || r.Search.RRFK != 60 || r.Search.EmbedTimeoutMS != 3000 || r.Search.MaxTerms != 24 || r.Ingest.BackoffSeconds != 30 {
		t.Fatalf("%+v %+v", r.Search, r.Ingest)
	}
	bad := Config{Search: SearchConfig{RRFK: -1}}.Resolved()
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "search.rrf_k") {
		t.Fatal(err)
	}
	if !strings.Contains(string(Init()), `"paths"`) || !strings.Contains(string(Effective(Config{}, "x", env(nil))), `"rrf_k": 60`) {
		t.Fatal("config init / show must list the new sections")
	}
}
