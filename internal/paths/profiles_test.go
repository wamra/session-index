package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeProfiles(t *testing.T) (profiles string) {
	t.Helper()
	profiles = t.TempDir()
	for _, n := range []string{"desktop", "laptop", "default"} {
		os.MkdirAll(filepath.Join(profiles, n, "projects"), 0o755)
	}
	touch(t, filepath.Join(profiles, "desktop", ArchiveRel))
	touch(t, filepath.Join(profiles, "laptop", ArchiveRel))
	// default has no archive; a stray file at top level is ignored.
	touch(t, filepath.Join(profiles, "README.txt"))
	return profiles
}

func TestProfilesResolvesSymlink(t *testing.T) {
	profiles := fakeProfiles(t)
	home := t.TempDir()
	link := filepath.Join(home, ".claude")
	if err := os.Symlink(filepath.Join(profiles, "desktop"), link); err != nil {
		t.Fatal(err)
	}
	own, dir := Profiles(link)
	real, _ := filepath.EvalSymlinks(profiles)
	if own != "desktop" || dir != real {
		t.Fatalf("got %q %q, want desktop %q", own, dir, real)
	}
}

func TestDiscoverPeersFromFakeProfilesTree(t *testing.T) {
	profiles := fakeProfiles(t)
	got := DiscoverPeers(profiles, "desktop")
	if len(got) != 1 || got[0].Name != "laptop" || got[0].Path != filepath.Join(profiles, "laptop", ArchiveRel) {
		t.Fatalf("peers = %+v", got)
	}
	all := DiscoverPeers(profiles, "")
	if len(all) != 2 {
		t.Fatalf("all = %+v", all)
	}
	roots := ProfileRoots(profiles)
	if len(roots) != 3 || roots[0].Name != "default" || roots[2].Name != "laptop" {
		t.Fatalf("roots = %+v", roots)
	}
	if got := DiscoverPeers(filepath.Join(profiles, "missing"), ""); got != nil {
		t.Fatalf("missing dir: %+v", got)
	}
}
