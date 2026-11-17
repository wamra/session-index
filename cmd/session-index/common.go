package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wamra/session-index/internal/config"
	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/provider"
	"github.com/wamra/session-index/internal/store"
)

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func envOr(key, def string) string {
	if v := config.WithLegacy(os.Getenv)(key); v != "" {
		return v
	}
	return def
}

func dbPath() string {
	return envOr("SESSION_INDEX_DB", filepath.Join(homeDir(), ".claude", "cache", "session-archive", "archive.sqlite"))
}

func defaultRoot() string {
	return envOr("SESSION_INDEX_ROOT", filepath.Join(homeDir(), ".claude", "projects"))
}

// configPath is the config file location (SESSION_INDEX_CONFIG or
// ~/.config/session-index/config.json).
func configPath() string { return config.Path(homeDir(), os.Getenv) }

// loadConfig returns the effective (resolved, validated) configuration.
func loadConfig() (config.Config, error) {
	c, _, err := config.Load(configPath(), os.Getenv)
	if err != nil {
		return config.Config{}, err
	}
	c = c.Resolved()
	if err := c.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("config %s: %w", configPath(), err)
	}
	return c, nil
}

// newEmbedder builds the configured embedder. When the config leaves the
// vector size open (unknown model) and st is given, the archive's stored size
// is used.
func newEmbedder(cfg config.Config, st *store.Store) (provider.Embedder, error) {
	ec := cfg.Embed
	if ec.Dims == 0 && st != nil {
		if d, err := store.EffectiveVecDims(st.DB()); err == nil {
			ec.Dims = d
		}
	}
	return provider.NewEmbedder(ec)
}

// newSummarizer builds the configured summarizer; a non-empty model overrides
// summarize.model (the --model flag).
func newSummarizer(cfg config.Config, model string) (provider.Summarizer, error) {
	sc := cfg.Summarize
	if model != "" {
		sc.Model = model
	}
	return provider.NewSummarizer(sc)
}

func claudeDir() string { return filepath.Join(homeDir(), ".claude") }

// ownProfile returns this machine's profile name (basename of the resolved
// ~/.claude) and the profiles directory (its parent). When ~/.claude is a
// plain directory (not under a profiles tree) the name is "default" and the
// profiles directory is empty, which disables peer discovery.
func ownProfile() (name, profilesDir string) {
	name, profilesDir = paths.Profiles(claudeDir())
	if name == ".claude" {
		return "default", ""
	}
	return name, profilesDir
}

// openStore opens the archive for archiving (ingest, sweep, backfill, hooks,
// restore, files, show, stats, repair, ...), creating its directory if missing,
// and backfills rows without a profile with the own profile name. It never
// fails on the embedder configuration: an invalid config file or a vector-size
// mismatch is noted on stderr and the archive's stored size is used. Only
// embed, summarize, search and doctor evaluate those checks (openStoreStrict).
func openStore() (*store.Store, error) {
	return openStoreWith(dbPath(), true, noteWriter)
}

// noteWriter receives the non-fatal notes of the lenient open (a var so tests
// can capture them).
var noteWriter io.Writer = os.Stderr

// openStoreStrict is openStore that fails on an invalid config or a
// configured vector size that differs from the archive's.
func openStoreStrict() (*store.Store, error) {
	return openStoreWith(dbPath(), false, nil)
}

// openStoreWith opens the archive with the configured embedder. A new archive
// gets chunk_vec at the configured size (auto-detected with one request only
// when the config leaves it open; 768 if the provider cannot answer, which
// never fails the open). An existing archive keeps its size. With notes == nil
// an invalid config is an error and, unless allowDimsChange, so is a different
// configured size (naming both values); with notes != nil (io.Discard to stay
// silent) an invalid config falls back to the defaults (never for a NEW archive,
// which is refused) and a mismatch is only noted.
func openStoreWith(p string, allowDimsChange bool, notes io.Writer) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	_, existing := os.Stat(p)
	cfg, err := loadConfig()
	if err != nil {
		if notes == nil || existing != nil {
			// strict, or nothing to fall back to: a new archive needs a valid
			// config to decide its vector size
			return nil, err
		}
		fmt.Fprintf(notes, "session-index: ignoring the embedder configuration to open the archive: %v\n", err)
		cfg = config.Config{}.Resolved()
	}
	emb, err := newEmbedder(cfg, nil)
	if err != nil {
		return nil, err
	}
	dims := emb.Dims()
	var probeErr error
	if existing != nil && dims == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, probeErr = emb.Embed(ctx, []string{"dimension probe"}, "")
		cancel()
		dims = emb.Dims()
	}
	fpDims := dims
	if fpDims == 0 {
		fpDims = store.VecDim
	}
	fp := provider.Fingerprint(cfg.Embed.Provider, cfg.Embed.Model, fpDims, *cfg.Embed.DocPrefix)
	if probeErr != nil && notes != nil {
		fmt.Fprintf(notes, "session-index: creating chunk_vec with %d dims and fingerprint %s (provider probe failed: %v); set embed.dims, then run rebuild-derived\n", store.VecDim, fp, probeErr)
	}
	own, _ := ownProfile()
	st, err := store.OpenWith(p, store.Opts{
		Profile: own, Dims: dims, AllowDimsChange: allowDimsChange, Fingerprint: fp,
	})
	if err != nil {
		return nil, err
	}
	if sd, _ := store.VecDims(st.DB()); notes != nil && sd > 0 && dims > 0 && sd != dims {
		fmt.Fprintf(notes, "session-index: note: %v (using the archive's %d; embed and search will refuse until fixed)\n", &store.DimsMismatchError{Stored: sd, Config: dims}, sd)
	}
	return st, nil
}

// embedEndpoint and sumEndpoint describe where a provider is reached: the base
// URL, or the AWS region and profile for bedrock.
func embedEndpoint(c provider.EmbedConfig) string {
	return endpointOf(c.Provider, c.BaseURL, c.Region, c.Profile)
}

func sumEndpoint(c provider.SummarizeConfig) string {
	return endpointOf(c.Provider, c.BaseURL, c.Region, c.Profile)
}

// endpointField is the doctor's "base_url=..." (or "region=... profile=...").
func endpointField(p, base, region, profile string) string {
	if p != provider.Bedrock {
		return "base_url=" + base
	}
	s := "region=" + region
	if profile != "" {
		s += " profile=" + profile
	}
	return s
}

func endpointOf(p, base, region, profile string) string {
	if p != provider.Bedrock {
		return base
	}
	s := "AWS region " + region
	if region == "" {
		s = "AWS default region"
	}
	if profile != "" {
		s += ", profile " + profile
	}
	return s
}

// authLine renders the message for a rejected credential.
func authLine(err error, providerName, baseURL, keyEnv string) string {
	env := keyEnv
	state := "set"
	switch {
	case keyEnv == "":
		env, state = "api_key_env", "not configured"
	case os.Getenv(keyEnv) == "":
		state = "unset"
	}
	code := ""
	if c := provider.AuthCode(err); c != 0 {
		code = fmt.Sprintf(" (HTTP %d)", c)
	}
	where := providerName
	if baseURL != "" {
		where += " at " + baseURL
	}
	if providerName == provider.Bedrock {
		return fmt.Sprintf("authentication failed%s for %s: %v; check the AWS credentials (environment, profile, SSO)", code, where, err)
	}
	return fmt.Sprintf("authentication failed%s for %s; check %s (%s)", code, where, env, state)
}

// closeWriter checkpoints the WAL (so archive.sqlite is complete on its own)
// and closes the store. A busy checkpoint (another writer is active) is not
// an error: that writer checkpoints when it finishes.
func closeWriter(st *store.Store, errw io.Writer) {
	if err := st.Checkpoint(); err != nil && !errors.Is(err, store.ErrCheckpointBusy) {
		fmt.Fprintf(errw, "session-index: wal checkpoint: %v\n", err)
	}
	st.Close()
}

func binaryPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

// parseSince accepts 7d, 24h or YYYY-MM-DD.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if len(s) > 1 && (strings.HasSuffix(s, "d") || strings.HasSuffix(s, "h")) {
		if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n >= 0 {
			unit := time.Hour
			if s[len(s)-1] == 'd' {
				unit = 24 * time.Hour
			}
			return now.Add(-time.Duration(n) * unit), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want 7d, 24h or YYYY-MM-DD)", s)
}

// resolveSession expands a unique session id prefix to the full id.
func resolveSession(ctx context.Context, db *sql.DB, id string) (string, error) {
	rows, err := db.QueryContext(ctx, `SELECT session_id FROM sessions WHERE session_id=? OR session_id LIKE ? ESCAPE '\' ORDER BY session_id LIMIT 3`,
		id, strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(id)+"%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		if s == id {
			return s, nil
		}
		ids = append(ids, s)
	}
	switch len(ids) {
	case 0:
		return id, nil // let the caller report "not found" in its own words
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("session id prefix %q is ambiguous", id)
}

// drainReport runs embed.Drain and prints the outcome. It returns the exit
// code: ErrLocked is success; provider.ErrDown is exit 0 unless strict.
func drainReport(ctx context.Context, st *store.Store, batch int, strict bool, out, errw io.Writer) int {
	code, _ := drainLocked(ctx, st, batch, strict, out, errw)
	return code
}

// drainLocked is drainReport that also reports whether another process held
// the embed lock (in which case nothing is printed).
func drainLocked(ctx context.Context, st *store.Store, batch int, strict bool, out, errw io.Writer) (code int, locked bool) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(errw, "embed: %v\n", err)
		return 1, false
	}
	emb, err := newEmbedder(cfg, st)
	if err != nil {
		fmt.Fprintf(errw, "embed: %v\n", err)
		return 1, false
	}
	limit, _ := cfg.Embed.DailyCap()
	n, err := embed.DrainCapped(ctx, st, emb, batch, limit, nil)
	var capErr *embed.ErrDailyCap
	switch {
	case errors.As(err, &capErr):
		fmt.Fprintf(out, "embedded %d chunks; embed: %v (embed.max_items_per_day)\n", n, capErr)
		return 0, false
	case err == nil:
		fmt.Fprintf(out, "embedded %d chunks\n", n)
		return 0, false
	case errors.Is(err, embed.ErrLocked):
		return 0, true
	case errors.Is(err, provider.ErrAuth):
		fmt.Fprintf(errw, "embed: %s; embedded %d chunks, rest stay pending\n", authLine(err, cfg.Embed.Provider, cfg.Embed.BaseURL, cfg.Embed.APIKeyEnv), n)
		return 1, false
	case errors.Is(err, provider.ErrDown):
		fmt.Fprintf(errw, "warning: %s unreachable at %s; embedded %d chunks, rest stay pending\n", cfg.Embed.Provider, embedEndpoint(cfg.Embed), n)
		if strict {
			return 1, false
		}
		return 0, false
	case errors.Is(err, embed.ErrFingerprintMismatch):
		fmt.Fprintln(errw, "embedder fingerprint mismatch: run `session-index rebuild-derived`")
		return 1, false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(out, "embedded %d chunks (stopped early)\n", n)
		return 0, false
	case errors.Is(err, embed.ErrEmbedderBroken):
		fmt.Fprintf(errw, "embedder is failing: %v\n", err)
		return 1, false
	default:
		fmt.Fprintf(errw, "embed: %v (embedded %d)\n", err, n)
		return 1, false
	}
}
