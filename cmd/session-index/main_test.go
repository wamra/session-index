package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/store"
)

// setHome points the home directory at dir: os.UserHomeDir reads HOME on Unix
// and USERPROFILE on Windows.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// portableMode maps a Unix permission to what the OS reports back: Windows
// only has 0666 (writable) and 0444 (read-only).
func portableMode(m os.FileMode) os.FileMode {
	if runtime.GOOS != "windows" {
		return m
	}
	if m&0o200 != 0 {
		return 0o666
	}
	return 0o444
}

const tsid = "22222222-2222-2222-2222-222222222222"

func seedRoot(t *testing.T) (root, db string) {
	t.Helper()
	root = t.TempDir()
	setHome(t, t.TempDir()) // never discover the real profiles tree
	db = filepath.Join(t.TempDir(), "sub", "archive.sqlite")
	t.Setenv("SESSION_INDEX_DB", db)
	t.Setenv("SESSION_INDEX_ROOT", root)
	t.Setenv("SESSION_INDEX_OLLAMA", "http://127.0.0.1:1")
	p := filepath.Join(root, "proj", tsid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	l1 := fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":"%s","timestamp":"2026-01-01T00:00:01Z","cwd":"/w","message":{"role":"user","content":"Please investigate the zebrafish migration problem in detail"}}`, tsid)
	l2 := fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"%s","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"The zebrafish migration fails because of a missing index on the table."}]}}`, tsid)
	os.WriteFile(p, []byte(l1+"\n"+l2+"\n"), 0o644)
	return root, db
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := run(args, &o, &e)
	return code, o.String(), e.String()
}

func TestCLISmokeSearchJSON(t *testing.T) {
	seedRoot(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	code, out, e := runCLI(t, "search", "zebrafish", "--mode", "fts", "--json")
	if code != 0 {
		t.Fatalf("search %d %s", code, e)
	}
	var got struct {
		Route string
		Hits  []struct {
			SessionID string `json:"session_id"`
			Chunk     string `json:"chunk"`
		}
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.Route != "fts" || len(got.Hits) != 1 || got.Hits[0].SessionID != tsid || !strings.Contains(got.Hits[0].Chunk, "zebrafish") {
		t.Fatalf("%+v", got)
	}
	code, out, _ = runCLI(t, "search", "zebrafish", "--mode", "fts")
	if code != 0 || !lineRE.MatchString(out) || !strings.Contains(out, "route: fts") {
		t.Fatalf("text output: %d %q", code, out)
	}
	if code, out, _ = runCLI(t, "show", tsid[:8], "--around", "a1", "--n", "2"); code != 0 || !strings.Contains(out, "[assistant]") || !strings.Contains(out, "[user]") {
		t.Fatalf("show: %d %q", code, out)
	}
	if code, out, _ = runCLI(t, "stats"); code != 0 || !strings.Contains(out, "records") {
		t.Fatalf("stats: %d %q", code, out)
	}
}

func TestCLIStdinHookIngests(t *testing.T) {
	root, _ := seedRoot(t)
	p := filepath.Join(root, "proj", tsid+".jsonl")
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"%s","transcript_path":%q,"hook_event_name":"Stop"}`, tsid, p))
	defer func() { stdin = os.Stdin }()
	if code, _, e := runCLI(t, "ingest", "--stdin-hook"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	code, out, _ := runCLI(t, "search", "zebrafish", "--mode", "fts", "--json")
	if code != 0 || !strings.Contains(out, tsid) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCLIRebuildDerived(t *testing.T) {
	seedRoot(t)
	runCLI(t, "sweep")
	if code, _, e := runCLI(t, "rebuild-derived"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	code, out, _ := runCLI(t, "search", "zebrafish", "--mode", "fts", "--json")
	if code != 0 || !strings.Contains(out, tsid) {
		t.Fatalf("%d %s", code, out)
	}
}

var lineRE = regexp.MustCompile(`(?m)^\[\d\.\d{3}\] 2026-01-01 default 22222222 proj — .* — .*zebrafish.*$`)

func TestCLIHookStdinTimeoutExitsZero(t *testing.T) {
	seedRoot(t)
	pr, pw := io.Pipe() // never written to
	defer pw.Close()
	stdin = pr
	defer func() { stdin = os.Stdin }()
	start := time.Now()
	code, _, e := runCLI(t, "ingest", "--stdin-hook")
	if code != 0 || !strings.Contains(e, "timed out") {
		t.Fatalf("%d %q", code, e)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestCLIHookMissingTranscriptExitsZero(t *testing.T) {
	root, _ := seedRoot(t)
	p := filepath.Join(root, "proj", "99999999-9999-9999-9999-999999999999.jsonl")
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"99999999-9999-9999-9999-999999999999","transcript_path":%q}`, p))
	defer func() { stdin = os.Stdin }()
	code, _, e := runCLI(t, "ingest", "--stdin-hook")
	if code != 0 || !strings.Contains(e, "99999999") {
		t.Fatalf("%d %q", code, e)
	}
}

func TestCLIHookEmbedSpawnsViaInjectedSpawner(t *testing.T) {
	root, _ := seedRoot(t)
	p := filepath.Join(root, "proj", tsid+".jsonl")
	called := 0
	old := spawnEmbed
	spawnEmbed = func(*store.Store) error { called++; return nil }
	defer func() { spawnEmbed = old }()
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"%s","transcript_path":%q}`, tsid, p))
	defer func() { stdin = os.Stdin }()
	if code, _, e := runCLI(t, "ingest", "--stdin-hook", "--embed"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if called != 1 {
		t.Fatalf("spawn called %d times", called)
	}
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":"%s","transcript_path":%q}`, tsid, p))
	runCLI(t, "ingest", "--stdin-hook")
	if called != 1 {
		t.Fatal("spawned without --embed")
	}
}

// The real spawnEmbed runs `<self> embed` detached with output in embed.log.
func TestSpawnEmbedRunsSelfEmbed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake self is a #!/bin/sh script; Windows cannot exec it")
	}
	_, db := seedRoot(t)
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	marker := filepath.Join(dir, "args.txt")
	fake := filepath.Join(dir, "fake-self")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" > "+marker+"\necho hello-log\n"), 0o755)
	old := selfPath
	selfPath = func() (string, error) { return fake, nil }
	defer func() { selfPath = old }()
	if err := spawnEmbed(st); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for i := 0; i < 100; i++ {
		if b, _ = os.ReadFile(marker); len(b) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.TrimSpace(string(b)) != "embed" {
		t.Fatalf("args %q", b)
	}
	time.Sleep(200 * time.Millisecond)
	lb, _ := os.ReadFile(filepath.Join(filepath.Dir(db), "embed.log"))
	if !strings.Contains(string(lb), "hello-log") {
		t.Fatalf("embed.log %q", lb)
	}
}

func derivedCounts(t *testing.T, db string) [3]int {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var c [3]int
	for i, q := range []string{`SELECT count(*) FROM chunks`, `SELECT count(*) FROM messages`, `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH '"zebrafish"'`} {
		if err := st.DB().QueryRow(q).Scan(&c[i]); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestCLIRebuildDerivedCountsAndLock(t *testing.T) {
	_, db := seedRoot(t)
	runCLI(t, "sweep")
	before := derivedCounts(t, db)
	if before[0] == 0 || before[1] == 0 || before[2] == 0 {
		t.Fatalf("empty fixture %v", before)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	lk, err := embed.TryLock(st)
	if err != nil {
		t.Fatal(err)
	}
	code, _, e := runCLI(t, "rebuild-derived")
	if code != 1 || !strings.Contains(e, "embed running, retry") {
		t.Fatalf("locked: code=%d stderr=%q", code, e)
	}
	if got := derivedCounts(t, db); got != before {
		t.Fatalf("locked rebuild changed data: %v -> %v", before, got)
	}
	lk.Release()
	st.Close()
	if code, _, e := runCLI(t, "rebuild-derived"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if got := derivedCounts(t, db); got != before {
		t.Fatalf("rebuild changed counts: %v -> %v", before, got)
	}
}

func TestCLIHookModePanicExitsZero(t *testing.T) {
	_, _ = seedRoot(t)
	p := filepath.Join(t.TempDir(), "proj", tsid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"type":"user","uuid":"u9","sessionId":"`+tsid+`","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"hello there general kenobi"}}`+"\n"), 0o644)
	stdin = strings.NewReader(fmt.Sprintf(`{"session_id":%q,"transcript_path":%q,"hook_event_name":"Stop"}`, tsid, p))
	defer func() { stdin = os.Stdin }()
	old := spawnEmbed
	spawnEmbed = func(*store.Store) error { panic("boom") }
	defer func() { spawnEmbed = old }()
	code, _, e := runCLI(t, "ingest", "--stdin-hook", "--embed", "--root", filepath.Dir(filepath.Dir(p)))
	if code != 0 || !strings.Contains(e, "panic in hook mode: boom") {
		t.Fatalf("code=%d stderr=%q", code, e)
	}
	// non-hook mode still propagates (no recover)
	if code, _, _ := runCLI(t, "ingest", "--stdin-hook", "--bogus-flag"); code != 0 {
		t.Fatalf("hook flag-parse error code=%d", code)
	}
}

func TestCLIInstallHooksLaunchdOptIn(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	settings := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.WriteFile(settings, []byte("{}\n"), 0o644)
	plist := plistPathDefault()
	code, out, e := runCLI(t, "install-hooks", "--settings", settings)
	if code != 0 || strings.Contains(out, "launchctl") {
		t.Fatalf("default: %d %s %s", code, out, e)
	}
	if _, err := os.Stat(plist); err == nil {
		t.Fatal("plist written without --launchd")
	}
	code, out, e = runCLI(t, "install-hooks", "--settings", settings, "--launchd")
	if code != 0 || !strings.Contains(out, "launchctl bootstrap") {
		t.Fatalf("--launchd: %d %s %s", code, out, e)
	}
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("plist missing with --launchd: %v", err)
	}
}

func TestCLIDoctorPlistOptionalAndQuickCheck(t *testing.T) {
	seedRoot(t)
	home := t.TempDir()
	setHome(t, home)
	settings := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.WriteFile(settings, []byte("{}\n"), 0o644)
	if code, _, e := runCLI(t, "install-hooks", "--settings", settings); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"embeddinggemma:latest","model":"embeddinggemma:latest"}]}`)
	}))
	defer srv.Close()
	t.Setenv("SESSION_INDEX_OLLAMA", srv.URL)
	code, out, e := runCLI(t, "doctor")
	if code != 0 {
		t.Fatalf("doctor %d\n%s\n%s", code, out, e)
	}
	if !strings.Contains(out, "quick_check: ok") || !strings.Contains(out, "launchd: not installed (optional") || strings.Contains(out, "WARNING") {
		t.Fatalf("out=%s", out)
	}
}

func TestCloudStorageDetection(t *testing.T) {
	for p, want := range map[string]bool{
		"/Users/bob/Library/CloudStorage/OneDrive-Personal/x/archive.sqlite": true,
		"/Users/bob/Library/Application Support/archive.sqlite":              false,
		"/private/tmp/archive.sqlite":                                        false,
		"/Users/bob/.claude/archive.sqlite":                                  false,
		"/Users/bob/Library/Mobile Documents/com~apple~CloudDocs/a.sqlite":   true,
		`C:\Users\bob\OneDrive\claude\cache\archive.sqlite`:                  true,
		"/home/bob/Dropbox/claude/archive.sqlite":                            true,
		"/home/bob/dev/onedrive-tools.sqlite":                                false,
		"/home/bob/.claude/cache/session-archive/archive.sqlite":             false,
	} {
		if got := cloudStorage(p); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
}
