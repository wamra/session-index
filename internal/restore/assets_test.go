package restore

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/ingest"
)

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

func TestRestoreWritesAssetsWithModeAndMtime(t *testing.T) {
	st := openStore(t)
	prof := t.TempDir()
	root := filepath.Join(prof, "projects")
	// a multiple of 100 ns, the resolution of Windows file times
	mt := time.Unix(1700000000, 987654300)
	main := filepath.Join(root, "proj", sid+".jsonl")
	writeFile(t, main, userLine(1, longText))
	os.Chmod(main, 0o600)
	os.Chtimes(main, mt, mt)
	tr := filepath.Join(root, "proj", sid, "tool-results", "out.txt")
	writeFile(t, tr, "big tool output")
	os.Chmod(tr, 0o640)
	os.Chtimes(tr, mt.Add(time.Hour), mt.Add(time.Hour))
	mem := filepath.Join(root, "proj", "memory", "MEMORY.md")
	writeFile(t, mem, "# memory notes for the project")
	fh := filepath.Join(prof, "file-history", sid, "abc@v1")
	writeFile(t, fh, "old snapshot")
	if _, err := ingest.Sweep(context.Background(), st, root, "p"); err != nil {
		t.Fatal(err)
	}

	to := t.TempDir()
	placed, err := Session(context.Background(), st, sid, to, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 2 {
		t.Fatalf("placed %v (file-history must not be restored without the flag)", placed)
	}
	for _, c := range []struct {
		src, rel string
		mode     os.FileMode
		mt       time.Time
	}{
		{main, "proj/" + sid + ".jsonl", portableMode(0o600), mt},
		{tr, "proj/" + sid + "/tool-results/out.txt", portableMode(0o640), mt.Add(time.Hour)},
	} {
		got := filepath.Join(to, c.rel)
		fi, err := os.Stat(got)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != c.mode || !fi.ModTime().Equal(c.mt) || fileSHA(t, got) != fileSHA(t, c.src) {
			t.Errorf("%s: mode %o mtime %v sha mismatch=%v", c.rel, fi.Mode().Perm(), fi.ModTime(), fileSHA(t, got) != fileSHA(t, c.src))
		}
	}
	// refuses existing targets
	if _, err := Session(context.Background(), st, sid, to, false); err == nil {
		t.Fatal("expected refusal")
	}
	// file-history only with the flag, under file-history/<sid>/
	to2 := t.TempDir()
	if _, err := SessionOpts(context.Background(), st, sid, to2, false, true); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(to2, "file-history", sid, "abc@v1")); err != nil || string(b) != "old snapshot" {
		t.Fatalf("%q %v", b, err)
	}
	// memory session
	to3 := t.TempDir()
	if _, err := Session(context.Background(), st, "memory:proj", to3, false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(to3, "proj", "memory", "MEMORY.md")); err != nil || string(b) != "# memory notes for the project" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestRestoreProfileDirs(t *testing.T) {
	st := openStore(t)
	prof := t.TempDir()
	root := filepath.Join(prof, "projects")
	os.MkdirAll(root, 0o755)
	mt := time.Unix(1700000000, 555555500)
	type f struct {
		rel     string // under the profile
		mode    os.FileMode
		content string
	}
	files := []f{
		{filepath.Join("plans", "my-plan.md"), 0o644, "# plan"},
		{filepath.Join("paste-cache", "0123456789abcdef.txt"), 0o600, "pasted"},
		{filepath.Join("tasks", sid, "1.json"), 0o640, `{"id":1}`},
		{filepath.Join("tasks", sid, ".highwatermark"), 0o600, "1"},
		{filepath.Join("uploads", sid, "a.png"), 0o644, "\x89PNG bytes"},
	}
	for _, c := range files {
		p := filepath.Join(prof, c.rel)
		writeFile(t, p, c.content)
		os.Chmod(p, c.mode)
		os.Chtimes(p, mt, mt)
	}
	if _, err := ingest.Sweep(context.Background(), st, root, "p"); err != nil {
		t.Fatal(err)
	}
	check := func(to string, rels ...string) {
		t.Helper()
		for _, c := range files {
			want := false
			for _, r := range rels {
				want = want || r == c.rel
			}
			got := filepath.Join(to, c.rel)
			fi, err := os.Stat(got)
			if !want {
				if err == nil {
					t.Errorf("%s restored but must not be", c.rel)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", c.rel, err)
				continue
			}
			b, _ := os.ReadFile(got)
			if string(b) != c.content || fi.Mode().Perm() != portableMode(c.mode) || !fi.ModTime().Equal(mt) {
				t.Errorf("%s: content %q mode %o mtime %v", c.rel, b, fi.Mode().Perm(), fi.ModTime())
			}
		}
	}
	ctx := context.Background()

	// pseudo-sessions restore without a flag
	to := t.TempDir()
	for _, id := range []string{"plans:p", "paste-cache:p"} {
		if _, err := Session(ctx, st, id, to, false); err != nil {
			t.Fatal(err)
		}
	}
	check(to, files[0].rel, files[1].rel)

	// one plan by its own id; an unknown plan is an error
	to = t.TempDir()
	if _, err := Session(ctx, st, "plans:p/my-plan.md", to, false); err != nil {
		t.Fatal(err)
	}
	check(to, files[0].rel)
	if _, err := Session(ctx, st, "plans:p/nope.md", t.TempDir(), false); err == nil {
		t.Fatal("expected an error for an unknown plan")
	}

	// session-scoped uploads/tasks need the flag
	if _, err := Session(ctx, st, sid, t.TempDir(), false); err == nil {
		t.Fatal("expected no files for the session without the flag")
	}
	to = t.TempDir()
	if _, err := SessionOptions(ctx, st, sid, to, Options{FileHistory: true}); err == nil {
		t.Fatal("file-history alone must not restore uploads/tasks")
	}
	placed, err := SessionOptions(ctx, st, sid, to, Options{ProfileDirs: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 3 {
		t.Fatalf("placed %v", placed)
	}
	check(to, files[2].rel, files[3].rel, files[4].rel)

	// in place puts files back at their original paths
	for _, c := range files {
		os.Remove(filepath.Join(prof, c.rel))
	}
	if _, err := SessionOptions(ctx, st, sid, "", Options{InPlace: true, ProfileDirs: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Session(ctx, st, "plans:p", "", true); err != nil {
		t.Fatal(err)
	}
	check(prof, files[0].rel, files[2].rel, files[3].rel, files[4].rel)
}
