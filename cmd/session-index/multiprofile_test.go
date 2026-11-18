package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
)

// fakeProfiles builds <tmp>/profiles/{desktop,laptop,default}/projects with one
// transcript each, points HOME/.claude at desktop and the archive at desktop's
// cache dir. It returns the profiles dir.
func fakeProfiles(t *testing.T) string {
	t.Helper()
	profiles := t.TempDir()
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("SESSION_INDEX_OLLAMA", "http://127.0.0.1:1")
	t.Setenv("SESSION_INDEX_PEERS", "")
	for i, n := range []string{"desktop", "laptop", "default"} {
		sid := fmt.Sprintf("00000000-0000-0000-0000-00000000000%d", i+1)
		p := filepath.Join(profiles, n, "projects", "proj", sid+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		l1 := fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":"%s","timestamp":"2026-01-01T00:00:01Z","cwd":"/w","message":{"role":"user","content":"Please investigate the %s quokka migration problem in detail"}}`, sid, n)
		l2 := fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"%s","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"The %s quokka migration fails because of a missing index."}]}}`, sid, n)
		os.WriteFile(p, []byte(l1+"\n"+l2+"\n"), 0o644)
	}
	if err := os.Symlink(filepath.Join(profiles, "desktop"), filepath.Join(home, ".claude")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink needs privilege on Windows: %v", err)
		}
		t.Fatal(err)
	}
	t.Setenv("SESSION_INDEX_DB", filepath.Join(home, ".claude", "cache", "session-archive", "archive.sqlite"))
	t.Setenv("SESSION_INDEX_ROOT", filepath.Join(home, ".claude", "projects"))
	return profiles
}

func TestCLISweepAllProfilesSetsProfilePerRoot(t *testing.T) {
	fakeProfiles(t)
	code, out, e := runCLI(t, "sweep", "--all-profiles")
	if code != 0 {
		t.Fatalf("sweep %d %s", code, e)
	}
	for _, n := range []string{"default", "desktop", "laptop"} {
		if !strings.Contains(out, "profile "+n+": swept 1 files: 2 new lines") {
			t.Fatalf("missing summary for %s:\n%s", n, out)
		}
	}
	st, err := store.Open(dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.DB().Query(`SELECT s.profile, f.profile FROM sessions s JOIN files f ON f.session_id=s.session_id ORDER BY s.session_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var sp, fp string
		if err := rows.Scan(&sp, &fp); err != nil {
			t.Fatal(err)
		}
		got = append(got, sp+"/"+fp)
	}
	if strings.Join(got, ",") != "desktop/desktop,laptop/laptop,default/default" {
		t.Fatalf("profiles = %v", got)
	}
	if code, _, _ := runCLI(t, "sweep", "--all-profiles", "--root", "x"); code != 2 {
		t.Fatalf("--all-profiles with --root: %d", code)
	}
}

func TestCLISweepRootProfileFlag(t *testing.T) {
	root, _ := seedRoot(t)
	if code, _, e := runCLI(t, "sweep", "--root", root, "--profile", "laptop"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	st, err := store.Open(dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var p string
	if err := st.DB().QueryRow(`SELECT profile FROM sessions`).Scan(&p); err != nil || p != "laptop" {
		t.Fatalf("%q %v", p, err)
	}
}

func TestCLIWritersLeaveWALEmpty(t *testing.T) {
	_, db := seedRoot(t)
	// Precondition: a writer that does not checkpoint leaves a non-empty -wal.
	pre, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pre.DB().Exec(`INSERT INTO meta(key,value) VALUES('probe','1')`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(db + "-wal"); store.JournalMode == "WAL" && (err != nil || fi.Size() == 0) {
		t.Fatalf("-wal should be non-empty before closeWriter: %v %v", fi, err)
	}
	closeWriter(pre, os.Stderr)
	if fi, err := os.Stat(db + "-wal"); err == nil && fi.Size() != 0 {
		t.Fatalf("closeWriter left -wal at %d bytes", fi.Size())
	}
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if fi, err := os.Stat(db + "-wal"); err == nil && fi.Size() != 0 {
		t.Fatalf("-wal is %d bytes after sweep", fi.Size())
	}
	if code, _, e := runCLI(t, "rebuild-derived"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if fi, err := os.Stat(db + "-wal"); err == nil && fi.Size() != 0 {
		t.Fatalf("-wal is %d bytes after rebuild-derived", fi.Size())
	}
}

func TestCLISearchDiscoversPeersAndFiltersProfile(t *testing.T) {
	profiles := fakeProfiles(t)
	// Peer archive for laptop, built independently of the own archive.
	laptopDB := filepath.Join(profiles, "laptop", paths.ArchiveRel)
	os.MkdirAll(filepath.Dir(laptopDB), 0o755)
	t.Setenv("SESSION_INDEX_DB", laptopDB)
	if code, _, e := runCLI(t, "sweep", "--root", filepath.Join(profiles, "laptop", "projects"), "--profile", "laptop"); code != 0 {
		t.Fatalf("laptop sweep %d %s", code, e)
	}
	// Own archive only knows desktop.
	t.Setenv("SESSION_INDEX_DB", filepath.Join(os.Getenv("HOME"), ".claude", "cache", "session-archive", "archive.sqlite"))
	if code, _, e := runCLI(t, "sweep", "--root", filepath.Join(profiles, "desktop", "projects")); code != 0 {
		t.Fatalf("own sweep %d %s", code, e)
	}
	t.Setenv("SESSION_INDEX_PEERS", "")
	os.Unsetenv("SESSION_INDEX_PEERS")

	code, out, e := runCLI(t, "search", "quokka", "--mode", "fts", "--json")
	if code != 0 {
		t.Fatalf("search %d %s", code, e)
	}
	var got struct {
		Hits []struct {
			SessionID string `json:"session_id"`
			Profile   string `json:"profile"`
		}
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	profs := map[string]bool{}
	for _, h := range got.Hits {
		profs[h.Profile] = true
	}
	if len(got.Hits) != 2 || !profs["desktop"] || !profs["laptop"] {
		t.Fatalf("hits = %+v", got.Hits)
	}

	code, out, e = runCLI(t, "search", "quokka", "--mode", "fts", "--profile", "laptop")
	if code != 0 || !strings.Contains(out, " laptop ") || strings.Contains(out, "desktop") {
		t.Fatalf("profile filter %d\n%s\n%s", code, out, e)
	}

	// An unreadable --db is reported on stderr and skipped.
	bad := filepath.Join(t.TempDir(), "bad.sqlite")
	os.WriteFile(bad, []byte(strings.Repeat("junk ", 400)), 0o644)
	code, out, e = runCLI(t, "search", "quokka", "--mode", "fts", "--db", bad)
	if code != 0 || !strings.Contains(e, "skipping peer bad") || !strings.Contains(out, "desktop") {
		t.Fatalf("bad --db: %d\n%s\n%s", code, out, e)
	}

	// SESSION_INDEX_PEERS replaces discovery.
	t.Setenv("SESSION_INDEX_PEERS", bad)
	code, out, e = runCLI(t, "search", "quokka", "--mode", "fts")
	if code != 0 || strings.Contains(out, " laptop ") || !strings.Contains(e, "skipping peer bad") {
		t.Fatalf("env peers: %d\n%s\n%s", code, out, e)
	}
}

func TestCLIInstallSkill(t *testing.T) {
	profiles := fakeProfiles(t)
	settings := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
	os.WriteFile(settings, []byte("{}\n"), 0o644)
	code, out, e := runCLI(t, "install-hooks", "--settings", settings, "--skill")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	own := filepath.Join(profiles, "desktop", "skills", "session-search", "SKILL.md")
	b, err := os.ReadFile(own)
	if err != nil || !strings.Contains(string(b), "name: session-search") {
		t.Fatalf("own skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profiles, "laptop", "skills")); err == nil {
		t.Fatal("--skill wrote to a peer profile")
	}
	code, out, e = runCLI(t, "install-hooks", "--settings", settings, "--skill-all-profiles")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	for _, n := range []string{"desktop", "laptop", "default"} {
		if _, err := os.Stat(filepath.Join(profiles, n, "skills", "session-search", "SKILL.md")); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
}

// The own profile is already archived under its ~/.claude/projects spelling;
// --all-profiles must reuse that path, not add a second copy via the resolved path.
func TestCLISweepAllProfilesDoesNotDuplicateOwnProfile(t *testing.T) {
	fakeProfiles(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("plain sweep %d %s", code, e)
	}
	code, out, e := runCLI(t, "sweep", "--all-profiles")
	if code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	if !strings.Contains(out, "profile desktop: swept 1 files: 0 new lines") {
		t.Fatalf("own profile re-ingested:\n%s", out)
	}
	st, err := store.Open(dbPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM files`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("files = %d, %v (want 3)", n, err)
	}
}

func TestCLISearchDoesNotWriteMigratedArchive(t *testing.T) {
	seedRoot(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	db := dbPath()
	before, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	code, out, e := runCLI(t, "search", "zebrafish", "--mode", "fts")
	if code != 0 || !strings.Contains(out, "zebrafish") {
		t.Fatalf("search %d %s %s", code, out, e)
	}
	if strings.Contains(e, "migrating") {
		t.Fatalf("unexpected migration: %s", e)
	}
	after, err := os.Stat(db)
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("archive changed by search: %v -> %v (%v)", before.ModTime(), after.ModTime(), err)
	}
	if !strings.Contains(e, "no profiles tree found; searching own archive only") {
		t.Fatalf("missing discovery note: %q", e)
	}
}

func TestCLIRepairAndDoctorFull(t *testing.T) {
	seedRoot(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	code, out, e := runCLI(t, "repair")
	if code != 0 || !strings.Contains(out, "repair: deleted 0 orphan messages, 0 chunks, 0 chunk_vec rows, 0 tool_fts rows; recomputed 0 sessions") {
		t.Fatalf("repair %d %s %s", code, out, e)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"embeddinggemma:latest","model":"embeddinggemma:latest"}]}`)
	}))
	defer srv.Close()
	t.Setenv("SESSION_INDEX_OLLAMA", srv.URL)
	_, out, _ = runCLI(t, "doctor")
	if !strings.Contains(out, "chunk_fts integrity-check: ok") || !strings.Contains(out, "tool_fts integrity-check: ok") || !strings.Contains(out, "asset_fts integrity-check: ok") || !strings.Contains(out, "file_fts integrity-check: ok") || strings.Contains(out, "integrity_check:") {
		t.Fatalf("default doctor:\n%s", out)
	}
	_, out, _ = runCLI(t, "doctor", "--full")
	if !strings.Contains(out, "integrity_check: ok") {
		t.Fatalf("doctor --full:\n%s", out)
	}
}

func TestCLIVacuumShrinksArchive(t *testing.T) {
	seedRoot(t)
	if code, _, e := runCLI(t, "sweep"); code != 0 {
		t.Fatalf("%d %s", code, e)
	}
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 2000)
	tx, _ := st.DB().Begin()
	for i := 0; i < 5000; i++ {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, fmt.Sprintf("junk%d", i), payload); err != nil {
			t.Fatal(err)
		}
	}
	tx.Commit()
	if _, err := st.DB().Exec(`DELETE FROM meta WHERE key LIKE 'junk%'`); err != nil {
		t.Fatal(err)
	}
	closeWriter(st, os.Stderr)
	before := fileSize(dbPath())

	if code, _, e := runCLI(t, "vacuum", "--db", "/x/peer.sqlite"); code == 0 || !strings.Contains(e, "only the own archive") {
		t.Fatalf("--db accepted: %d %q", code, e)
	}
	code, out, e := runCLI(t, "vacuum")
	if code != 0 || !strings.Contains(out, "reclaimed") {
		t.Fatalf("vacuum %d %s %s", code, out, e)
	}
	after := fileSize(dbPath())
	if after >= before {
		t.Fatalf("file did not shrink: %d -> %d (%s)", before, after, out)
	}
	if fi, err := os.Stat(dbPath() + "-wal"); err == nil && fi.Size() != 0 {
		t.Fatalf("-wal is %d bytes", fi.Size())
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"models":[{"name":"embeddinggemma:latest","model":"embeddinggemma:latest"}]}`)
	}))
	defer srv.Close()
	t.Setenv("SESSION_INDEX_OLLAMA", srv.URL)
	home := t.TempDir()
	setHome(t, home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte("{}\n"), 0o644)
	if c, o, e := runCLI(t, "install-hooks", "--settings", filepath.Join(home, ".claude", "settings.json")); c != 0 {
		t.Fatalf("%d %s %s", c, o, e)
	}
	code, out, e = runCLI(t, "doctor", "--full")
	if code != 0 || !strings.Contains(out, "integrity_check: ok") {
		t.Fatalf("doctor after vacuum %d\n%s\n%s", code, out, e)
	}
}
