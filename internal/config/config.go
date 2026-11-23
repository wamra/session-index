// Package config loads the session-index configuration: an optional JSON
// file overridden by environment variables.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wamra/session-index/internal/provider"
)

// Config is the contents of config.json.
type Config struct {
	Comment   string                   `json:"_comment,omitempty"`
	Paths     Paths                    `json:"paths"`
	Embed     provider.EmbedConfig     `json:"embed"`
	Summarize provider.SummarizeConfig `json:"summarize"`
	Search    SearchConfig             `json:"search"`
	Ingest    IngestConfig             `json:"ingest"`
}

// Paths locates the archive and what it reads. An empty value means the default
// (see ResolvePaths); a leading "~" is the home directory. The environment
// variables SESSION_INDEX_{DB,ROOT,KEY_DIR,PEERS} override these.
type Paths struct {
	DB        string   `json:"db,omitempty"`         // the archive file
	ClaudeDir string   `json:"claude_dir,omitempty"` // the Claude Code profile folder, default ~/.claude
	Root      string   `json:"root,omitempty"`       // the transcripts folder, default <claude_dir>/projects
	KeyDir    string   `json:"key_dir,omitempty"`    // unlocked keys on this machine
	Peers     []string `json:"peers,omitempty"`      // peer archives to search; empty = discover next to the profile folder
}

// SearchConfig holds the search knobs that change no stored data.
type SearchConfig struct {
	DefaultK       int `json:"default_k,omitempty"`        // results when --k is not given
	RRFK           int `json:"rrf_k,omitempty"`            // reciprocal-rank-fusion constant
	EmbedTimeoutMS int `json:"embed_timeout_ms,omitempty"` // how long a query waits for its embedding before falling back to full text
	MaxTerms       int `json:"max_terms,omitempty"`        // query terms used for full-text matching
}

// IngestConfig holds the ingest knobs that change no stored data.
type IngestConfig struct {
	BackoffSeconds int `json:"backoff_seconds,omitempty"` // first wait before a failed asset read is retried
}

// Path returns the config file path: $SESSION_INDEX_CONFIG, else
// <home>/.config/session-index/config.json.
func Path(home string, getenv func(string) string) string {
	getenv = WithLegacy(getenv)
	if p := getenv("SESSION_INDEX_CONFIG"); p != "" {
		return p
	}
	p := filepath.Join(home, ".config", "session-index", "config.json")
	if _, err := os.Stat(p); err != nil {
		old := filepath.Join(home, ".config", "session-archive", "config.json")
		if _, oerr := os.Stat(old); oerr == nil {
			return old
		}
	}
	return p
}

// ResolvePaths returns the effective locations: environment over file over
// default, with "~" expanded. Defaults derive from claude_dir, so a profile
// folder moves the archive, the transcripts and the asset folders together.
func (c Config) ResolvePaths(home string, getenv func(string) string) Paths {
	getenv = WithLegacy(getenv)
	exp := func(p string) string {
		switch {
		case p == "~":
			return home
		case strings.HasPrefix(p, "~/"), strings.HasPrefix(p, `~\`):
			return filepath.Join(home, p[2:])
		}
		return p
	}
	pick := func(env, file, def string) string {
		if v := getenv(env); v != "" {
			return exp(v)
		}
		if file != "" {
			return exp(file)
		}
		return def
	}
	var p Paths
	p.ClaudeDir = exp(c.Paths.ClaudeDir)
	if p.ClaudeDir == "" {
		p.ClaudeDir = filepath.Join(home, ".claude")
	}
	p.DB = pick("SESSION_INDEX_DB", c.Paths.DB, filepath.Join(p.ClaudeDir, "cache", "session-archive", "archive.sqlite"))
	p.Root = pick("SESSION_INDEX_ROOT", c.Paths.Root, filepath.Join(p.ClaudeDir, "projects"))
	p.KeyDir = pick("SESSION_INDEX_KEY_DIR", c.Paths.KeyDir, filepath.Join(home, ".config", "session-index", "keys"))
	if v := getenv("SESSION_INDEX_PEERS"); v != "" {
		for _, e := range filepath.SplitList(v) {
			if e != "" {
				p.Peers = append(p.Peers, e)
			}
		}
	} else {
		for _, e := range c.Paths.Peers {
			if e != "" {
				p.Peers = append(p.Peers, exp(e))
			}
		}
	}
	return p
}

// LoadPaths reads only the "paths" section of the file at path. It ignores
// every other key and never validates the embed or summarize sections, so a
// mistake there cannot send a hook to a different archive. A missing file is
// not an error (found is false); a file that is not valid JSON is.
func LoadPaths(path string) (p Paths, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Paths{}, false, nil
	}
	if err != nil {
		return Paths{}, false, fmt.Errorf("config %s: %w", path, err)
	}
	var f struct {
		Paths Paths `json:"paths"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return Paths{}, true, fmt.Errorf("config %s: %w", path, err)
	}
	return f.Paths, true, nil
}

// WithLegacy wraps getenv so a SESSION_INDEX_* lookup falls back to the
// SESSION_ARCHIVE_* name the tool used before it was renamed.
func WithLegacy(getenv func(string) string) func(string) string {
	return func(k string) string {
		if v := getenv(k); v != "" {
			return v
		}
		if rest, ok := strings.CutPrefix(k, "SESSION_INDEX_"); ok {
			return getenv("SESSION_ARCHIVE_" + rest)
		}
		return ""
	}
}

// Load reads the file at path (a missing file means defaults) and applies the
// environment overrides. The result is NOT resolved: call Resolved for the
// effective values. found reports whether the file existed.
//
// Precedence, lowest first: defaults, file, SESSION_INDEX_OLLAMA (alias for
// both base URLs while the provider is ollama), then
// SESSION_INDEX_EMBED_{PROVIDER,MODEL,BASE_URL} and
// SESSION_INDEX_SUMMARIZE_{PROVIDER,MODEL,BASE_URL}.
func Load(path string, getenv func(string) string) (c Config, found bool, err error) {
	getenv = WithLegacy(getenv)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		found = true
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return Config{}, true, fmt.Errorf("config %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return Config{}, false, fmt.Errorf("config %s: %w", path, err)
	}
	applyEnv(&c, getenv)
	return c, found, nil
}

func applyEnv(c *Config, getenv func(string) string) {
	set := func(dst *string, key string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	set(&c.Embed.Provider, "SESSION_INDEX_EMBED_PROVIDER")
	set(&c.Summarize.Provider, "SESSION_INDEX_SUMMARIZE_PROVIDER")
	if v := getenv("SESSION_INDEX_OLLAMA"); v != "" {
		if c.Embed.Provider == "" || c.Embed.Provider == provider.Ollama {
			c.Embed.BaseURL = v
		}
		if c.Summarize.Provider == "" || c.Summarize.Provider == provider.Ollama {
			c.Summarize.BaseURL = v
		}
	}
	set(&c.Embed.Model, "SESSION_INDEX_EMBED_MODEL")
	set(&c.Embed.BaseURL, "SESSION_INDEX_EMBED_BASE_URL")
	set(&c.Summarize.Model, "SESSION_INDEX_SUMMARIZE_MODEL")
	set(&c.Summarize.BaseURL, "SESSION_INDEX_SUMMARIZE_BASE_URL")
}

// Resolved returns c with every default filled in.
func (c Config) Resolved() Config {
	c.Embed = c.Embed.Resolved()
	c.Summarize = c.Summarize.Resolved()
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&c.Search.DefaultK, 10)
	def(&c.Search.RRFK, 60)
	def(&c.Search.EmbedTimeoutMS, 3000)
	def(&c.Search.MaxTerms, 24)
	def(&c.Ingest.BackoffSeconds, 30)
	return c
}

// Validate checks both sections of a resolved config.
func (c Config) Validate() error {
	if err := c.Embed.Validate(); err != nil {
		return err
	}
	if err := c.Summarize.Validate(); err != nil {
		return err
	}
	for _, n := range []struct {
		key string
		v   int
	}{{"search.default_k", c.Search.DefaultK}, {"search.rrf_k", c.Search.RRFK}, {"search.embed_timeout_ms", c.Search.EmbedTimeoutMS},
		{"search.max_terms", c.Search.MaxTerms}, {"ingest.backoff_seconds", c.Ingest.BackoffSeconds}} {
		if n.v < 0 {
			return fmt.Errorf("%s must be positive, got %d", n.key, n.v)
		}
	}
	return nil
}

type embedOut struct {
	Provider    string  `json:"provider"`
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	Dims        int     `json:"dims"`
	DocPrefix   *string `json:"doc_prefix,omitempty"`
	QueryPrefix *string `json:"query_prefix,omitempty"`
	APIKeyEnv   string  `json:"api_key_env"`
	Region      string  `json:"region,omitempty"`
	Profile     string  `json:"profile,omitempty"`
	// MaxItemsPerDay is shown as configured (0 = unset: hosted default or unlimited).
	MaxItemsPerDay int `json:"max_items_per_day,omitempty"`
}

type summarizeOut struct {
	Provider       string  `json:"provider"`
	BaseURL        string  `json:"base_url"`
	Model          string  `json:"model"`
	APIKeyEnv      string  `json:"api_key_env"`
	MaxTokens      int     `json:"max_tokens"`
	Temperature    float64 `json:"temperature"`
	Enabled        bool    `json:"enabled"`
	MaxItemsPerRun int     `json:"max_items_per_run"`
	Region         string  `json:"region,omitempty"`
	Profile        string  `json:"profile,omitempty"`
	MaxItemsPerDay int     `json:"max_items_per_day,omitempty"`
	// MaxInputChars is always shown (default 8000).
	MaxInputChars int      `json:"max_input_chars"`
	Kinds         []string `json:"kinds,omitempty"`
}

type out struct {
	Comment   string       `json:"_comment,omitempty"`
	Paths     Paths        `json:"paths"`
	Embed     embedOut     `json:"embed"`
	Summarize summarizeOut `json:"summarize"`
	Search    SearchConfig `json:"search"`
	Ingest    IngestConfig `json:"ingest"`
}

func (c Config) out(comment string) out {
	e, s := c.Embed, c.Summarize
	o := out{Comment: comment, Paths: c.Paths, Search: c.Search, Ingest: c.Ingest}
	o.Embed = embedOut{e.Provider, e.BaseURL, e.Model, e.Dims, e.DocPrefix, e.QueryPrefix, e.APIKeyEnv, e.Region, e.Profile, e.MaxItemsPerDay}
	o.Summarize = summarizeOut{s.Provider, s.BaseURL, s.Model, s.APIKeyEnv, s.MaxTokens, *s.Temperature, *s.Enabled, s.MaxItemsPerRun,
		s.Region, s.Profile, s.MaxItemsPerDay, s.MaxInputChars, s.Kinds}
	return o
}

// Effective renders the resolved config as JSON. API keys are never read:
// only the NAME of the key variable is shown, with whether it is set. src
// describes where the file was looked up.
func Effective(c Config, src string, getenv func(string) string) []byte {
	r := c.Resolved()
	if home, err := os.UserHomeDir(); err == nil {
		r.Paths = r.ResolvePaths(home, getenv) // show where the tool really looks
	}
	state := func(name string) string {
		switch {
		case name == "":
			return "none"
		case getenv(name) != "":
			return "set"
		}
		return "unset"
	}
	b, _ := json.MarshalIndent(struct {
		Source string `json:"_source"`
		out
		EmbedKey     string `json:"_embed_api_key"`
		SummarizeKey string `json:"_summarize_api_key"`
		EmbedCap     string `json:"_embed_daily_cap"`
		SummarizeCap string `json:"_summarize_daily_cap"`
	}{src, r.out(""), state(r.Embed.APIKeyEnv), state(r.Summarize.APIKeyEnv), capText(r.Embed.DailyCap), capText(r.Summarize.DailyCap)}, "", "  ")
	return append(b, '\n')
}

func capText(f func() (int, string)) string {
	n, src := f()
	if n == 0 {
		return src
	}
	return fmt.Sprintf("%d (%s)", n, src)
}

// InitComment is the "_comment" of the file written by `config init`.
const InitComment = "session-index configuration. Providers: embed = \"ollama\", \"openai\" (any OpenAI-compatible server) or \"bedrock\"; summarize = \"ollama\", \"openai\", \"anthropic\" or \"bedrock\". api_key_env is the NAME of an environment variable holding the API key (never the key itself). Set dims to 0 to auto-detect (a new archive is created with the detected size); doc_prefix/query_prefix are written only for models outside the known-model table; for known models they are left out so that changing the model picks up that model's prefixes (set them to override). Environment overrides: SESSION_INDEX_{EMBED,SUMMARIZE}_{PROVIDER,MODEL,BASE_URL}, SESSION_INDEX_OLLAMA. Changing the embedder of an existing archive needs `session-index rebuild-derived` and `session-index embed`. Cost guards: summarize.max_items_per_day / embed.max_items_per_day (per UTC day; hosted providers default to 2000, local ones are unlimited, -1 = unlimited), summarize.max_input_chars (default 8000), summarize.kinds (subset of tool_output, file_change, asset). Examples (replace the sections above): Anthropic summaries: {\"summarize\": {\"provider\": \"anthropic\", \"model\": \"claude-haiku-4-5\", \"api_key_env\": \"ANTHROPIC_API_KEY\"}} (Anthropic has no embeddings). Bedrock embeddings, Titan: {\"embed\": {\"provider\": \"bedrock\", \"model\": \"amazon.titan-embed-text-v2:0\", \"dims\": 1024, \"region\": \"us-east-1\", \"profile\": \"default\"}} (dims 256, 512 or 1024; Cohere: cohere.embed-english-v3 or cohere.embed-multilingual-v3, dims 1024). Bedrock summaries with Claude: {\"summarize\": {\"provider\": \"bedrock\", \"model\": \"<an anthropic.claude-* or us.anthropic.claude-* model id enabled in your account>\", \"region\": \"us-east-1\"}}. Voyage embeddings: {\"embed\": {\"provider\": \"openai\", \"base_url\": \"https://api.voyageai.com\", \"model\": \"voyage-3\", \"api_key_env\": \"VOYAGE_API_KEY\"}} (voyage-3: 1024 dims, voyage-3-lite: 512). OpenAI: {\"embed\": {\"provider\": \"openai\", \"base_url\": \"https://api.openai.com\", \"model\": \"text-embedding-3-small\", \"api_key_env\": \"OPENAI_API_KEY\"}}. LM Studio: {\"embed\": {\"provider\": \"openai\", \"base_url\": \"http://localhost:1234\", \"model\": \"<the model id LM Studio lists>\"}}. AWS credentials come from the default AWS chain (environment, shared config/profile, SSO), never from this file."

// Init renders the default config file.
func Init() []byte {
	r := Config{}.Resolved()
	o := r.out(InitComment)
	if _, known := provider.KnownEmbedModel(r.Embed.Model); known {
		o.Embed.DocPrefix, o.Embed.QueryPrefix = nil, nil // follow the model if it is changed later
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	return append(b, '\n')
}
