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
