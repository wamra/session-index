package paths

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
)

func TestClassify(t *testing.T) {
	root := filepath.Join("/", "home", "u", ".claude", "projects")
	tests := []struct {
		name string
		path string
		want Info
		ok   bool
	}{
		{
			name: "main session",
			path: filepath.Join(root, "-Users-u-proj", "11111111-2222-3333-4444-555555555555.jsonl"),
			want: Info{SessionID: "11111111-2222-3333-4444-555555555555", Kind: KindMain, ProjectSlug: "-Users-u-proj"},
			ok:   true,
		},
		{
			name: "subagent",
			path: filepath.Join(root, "-Users-u-proj", "11111111-2222", "subagents", "agent-abc.jsonl"),
			want: Info{SessionID: "11111111-2222", Kind: KindSubagent, AgentID: "agent-abc", ProjectSlug: "-Users-u-proj"},
			ok:   true,
		},
		{
			name: "workflow agent",
			path: filepath.Join(root, "-Users-u-proj", "11111111-2222", "wf_123", "agent-x.jsonl"),
			want: Info{SessionID: "11111111-2222", Kind: KindWorkflow, AgentID: "agent-x", ProjectSlug: "-Users-u-proj"},
			ok:   true,
		},
		{
			name: "workflow agent with wf_ prefix in deeper component",
			path: filepath.Join(root, "slug", "sid", "subagents", "wf_be263710-44e", "agent-y.jsonl"),
			want: Info{SessionID: "sid", Kind: KindWorkflow, AgentID: "agent-y", ProjectSlug: "slug"},
			ok:   true,
		},
		{
			name: "non-jsonl main-level file",
			path: filepath.Join(root, "slug", "sid.json"),
		},
		{
			name: "non-jsonl nested file",
			path: filepath.Join(root, "slug", "sid", "subagents", "agent-abc.txt"),
		},
		{
			name: "path outside root",
			path: filepath.Join("/", "elsewhere", "slug", "sid.jsonl"),
		},
		{
			name: "sibling sharing root prefix",
			path: root + "2" + string(filepath.Separator) + "slug" + string(filepath.Separator) + "sid.jsonl",
		},
		{
			name: "file directly under root",
			path: filepath.Join(root, "sid.jsonl"),
		},
		{
			name: "root itself",
			path: root,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Classify(root, tc.path)
			if ok != tc.ok {
				t.Fatalf("Classify ok = %v, want %v (got %+v)", ok, tc.ok, got)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Classify = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	files := []string{
		filepath.Join("slug", "sid.jsonl"),
		filepath.Join("slug", "sid", "subagents", "agent-abc.jsonl"),
		filepath.Join("slug", "sid", "wf_1", "agent-x.jsonl"),
		filepath.Join("slug", "notes.txt"),
		filepath.Join("slug", "sid", "subagents", "agent-abc.meta.json"),
	}
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	if err := Walk(root, func(p string) { got = append(got, p) }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{
		filepath.Join(root, "slug", "sid", "subagents", "agent-abc.jsonl"),
		filepath.Join(root, "slug", "sid", "wf_1", "agent-x.jsonl"),
		filepath.Join(root, "slug", "sid.jsonl"),
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Walk visited %v, want %v", got, want)
	}
}

func TestWalkSkipsUnreadableDir(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "slug", "sid.jsonl")
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Dir(good), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "inner", "x.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("chmod 000 does not make a directory unreadable on Windows")
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	var got []string
	if err := Walk(root, func(p string) { got = append(got, p) }); err != nil {
		t.Fatalf("Walk returned error for unreadable dir: %v", err)
	}
	if !reflect.DeepEqual(got, []string{good}) {
		t.Errorf("Walk visited %v, want [%s]", got, good)
	}
}

func TestWalkFollowsSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir() // lives outside root
	real := filepath.Join(target, "slug", "sid.jsonl")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked-slug")
	if err := os.Symlink(filepath.Join(target, "slug"), link); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := Walk(root, func(p string) { got = append(got, p) }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{filepath.Join(link, "sid.jsonl")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Walk visited %v, want %v (original, unresolved path)", got, want)
	}
	if len(got) == 1 {
		if info, ok := Classify(root, got[0]); !ok || info.ProjectSlug != "linked-slug" || info.Kind != KindMain {
			t.Errorf("Classify(%s) = %+v, %v; want main under linked-slug", got[0], info, ok)
		}
	}
}

func TestWalkSymlinkLoopTerminates(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "slug", "sid", "subagents", "agent-a.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// loop back to an ancestor, and a self-loop
	if err := os.Symlink(filepath.Join(root, "slug"), filepath.Join(root, "slug", "sid", "back")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "slug", "sid"), filepath.Join(root, "slug", "sid", "subagents", "self")); err != nil {
		t.Fatal(err)
	}
	// second path to the same real directory: visited once only
	if err := os.Symlink(filepath.Join(root, "slug", "sid", "subagents"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := Walk(root, func(p string) { got = append(got, p) }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Walk visited %v, want exactly one visit", got)
	}
}

func TestClassifyProfileDir(t *testing.T) {
	prof := filepath.Join(string(filepath.Separator), "home", "u", ".claude")
	cases := []struct {
		kind, rel string
		profile   string
		sid       string
		ok        bool
	}{
		{AssetPlans, "foo.md", "mac", "plans:mac", true},
		{AssetPlans, filepath.Join("sub", "foo.md"), "mac", "plans:mac", true},
		{AssetPasteCache, "0123456789abcdef.txt", "mac", "paste-cache:mac", true},
		{AssetPasteCache, "0123456789abcdef.txt", "", "paste-cache:", true},
		{AssetTasks, filepath.Join("sid-1", "1.json"), "mac", "sid-1", true},
		{AssetTasks, filepath.Join("sid-1", ".highwatermark"), "mac", "sid-1", true},
		{AssetTasks, filepath.Join("sid-1", "deep", "x"), "mac", "sid-1", true},
		{AssetTasks, ".DS_Store", "mac", "tasks:mac", true},
		{AssetUploads, filepath.Join("sid-2", "a.png"), "mac", "sid-2", true},
		{AssetUploads, "stray.png", "mac", "uploads:mac", true},
		{AssetUploads, "", "mac", "", false},
	}
	for _, c := range cases {
		dir := filepath.Join(prof, c.kind)
		info, ok := ClassifyProfileDir(dir, c.kind, c.profile, filepath.Join(dir, c.rel))
		if ok != c.ok || (ok && (info.SessionID != c.sid || info.Kind != c.kind)) {
			t.Errorf("%s/%s: got %+v %v, want sid %q ok %v", c.kind, c.rel, info, ok, c.sid, c.ok)
		}
	}
	if _, ok := ClassifyProfileDir(filepath.Join(prof, "plans"), AssetPlans, "m", filepath.Join(prof, "tasks", "x")); ok {
		t.Error("path outside dir must not classify")
	}
	if len(SweptDirs) != 4 {
		t.Errorf("SweptDirs = %v", SweptDirs)
	}
}
