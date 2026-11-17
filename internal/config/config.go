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
	Embed     provider.EmbedConfig     `json:"embed"`
	Summarize provider.SummarizeConfig `json:"summarize"`
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
	return c
}

// Validate checks both sections of a resolved config.
func (c Config) Validate() error {
	if err := c.Embed.Validate(); err != nil {
		return err
	}
	return c.Summarize.Validate()
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
	Embed     embedOut     `json:"embed"`
	Summarize summarizeOut `json:"summarize"`
}

func (c Config) out(comment string) out {
	e, s := c.Embed, c.Summarize
	o := out{Comment: comment}
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
