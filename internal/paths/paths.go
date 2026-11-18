// Package paths classifies Claude Code transcript files by their location
// under a projects root such as ~/.claude/projects.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

const ext = ".jsonl"

// Kind values reported in Info.Kind.
const (
	KindMain     = "main"
	KindSubagent = "subagent"
	KindWorkflow = "workflow"
)

// Info describes where a transcript file sits in the projects tree.
// Kind is one of KindMain, KindSubagent or KindWorkflow. AgentID is the file
// stem for subagent and workflow files and empty for main sessions.
type Info struct {
	SessionID   string
	Kind        string
	AgentID     string
	ProjectSlug string
	// Profile is the profile the transcript belongs to. Classify leaves it
	// empty; ingest callers fill it in.
	Profile string
}

// Classify reports the transcript metadata for path under root. It returns
// false for non-.jsonl files, paths outside root, and layouts it does not
// recognise.
//
// Recognised layouts, relative to root:
//
//	<slug>/<sid>.jsonl                          main
//	<slug>/<sid>/subagents/<agent>.jsonl        subagent
//	<slug>/<sid>/.../wf_*/.../<agent>.jsonl     workflow
func Classify(root, path string) (Info, bool) {
	if filepath.Ext(path) != ext {
		return Info{}, false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Info{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	stem := strings.TrimSuffix(parts[len(parts)-1], ext)

	switch {
	case len(parts) < 2 || len(parts) == 3:
		return Info{}, false
	case len(parts) == 2:
		return Info{SessionID: stem, Kind: KindMain, ProjectSlug: parts[0]}, true
	}

	info := Info{SessionID: parts[1], ProjectSlug: parts[0], AgentID: stem}
	for _, dir := range parts[2 : len(parts)-1] {
		if strings.HasPrefix(dir, "wf_") {
			info.Kind = KindWorkflow
			return info, true
		}
	}
	if parts[2] == "subagents" && len(parts) == 4 {
		info.Kind = KindSubagent
		return info, true
	}
	return Info{}, false
}

// Walk calls fn with the path of every *.jsonl file under root, at any depth.
// Symlinked directories are followed, and each real directory is visited at
// most once, so symlink loops terminate and do not duplicate files. Paths
// passed to fn are the original paths under root, not resolved targets.
// Unreadable directories are skipped without failing the walk. An error is
// returned only if root itself cannot be resolved.
func Walk(root string, fn func(path string)) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	seen := map[string]bool{realRoot: true}
	walkDir(root, seen, isJSONL, fn)
	return nil
}

func isJSONL(p string) bool { return filepath.Ext(p) == ext }

// WalkAssets calls fn with the path of every regular file that is not a
// *.jsonl transcript under root, with the same symlink and error rules as Walk.
func WalkAssets(root string, fn func(path string)) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	seen := map[string]bool{realRoot: true}
	walkDir(root, seen, func(p string) bool { return !isJSONL(p) }, fn)
	return nil
}

func walkDir(dir string, seen map[string]bool, match func(string) bool, fn func(path string)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // unreadable directory: skip
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		info, err := os.Stat(p) // follows symlinks
		if err != nil {
			continue // dangling symlink or unreadable entry
		}
		if info.IsDir() {
			real, err := filepath.EvalSymlinks(p)
			if err != nil || seen[real] {
				continue
			}
			seen[real] = true
			walkDir(p, seen, match, fn)
			continue
		}
		if info.Mode().IsRegular() && match(p) {
			fn(p)
		}
	}
}

// Asset kinds reported in AssetInfo.Kind.
const (
	AssetToolResult = "tool-result"
	AssetMeta       = "meta"
	AssetWorkflow   = "workflow"
	AssetMemory     = "memory"
	AssetOther      = "other"
)

// AssetInfo describes where a non-transcript file sits in the projects tree.
type AssetInfo struct {
	// SessionID is the owning session, "memory:<slug>" for files under
	// <slug>/memory, or empty for files directly under <slug>.
	SessionID   string
	Kind        string
	ProjectSlug string
}

// ClassifyAsset reports the asset metadata for a non-.jsonl path under root.
// It returns false for .jsonl files, paths outside root and files that sit
// directly in root.
//
//	<slug>/memory/<name>.md          memory (other files there: other)
//	<slug>/<sid>/tool-results/...    tool-result
//	<slug>/<sid>/**/<x>.meta.json    meta
//	<slug>/<sid>/**/workflows|wf_*   workflow
//	<slug>/<sid>/...                 other
//	<slug>/<file>                    other, no session
func ClassifyAsset(root, path string) (AssetInfo, bool) {
	if isJSONL(path) {
		return AssetInfo{}, false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return AssetInfo{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	name := parts[len(parts)-1]
	switch {
	case len(parts) < 2:
		return AssetInfo{}, false
	case len(parts) == 2:
		return AssetInfo{Kind: AssetOther, ProjectSlug: parts[0]}, true
	case parts[1] == "memory":
		info := AssetInfo{SessionID: "memory:" + parts[0], Kind: AssetOther, ProjectSlug: parts[0]}
		if len(parts) == 3 && strings.HasSuffix(name, ".md") {
			info.Kind = AssetMemory
		}
		return info, true
	}
	info := AssetInfo{SessionID: parts[1], Kind: AssetOther, ProjectSlug: parts[0]}
	switch {
	case parts[2] == "tool-results":
		info.Kind = AssetToolResult
	case strings.HasSuffix(name, ".meta.json"):
		info.Kind = AssetMeta
	default:
		for _, d := range parts[2 : len(parts)-1] {
			if d == "workflows" || strings.HasPrefix(d, "wf_") {
				info.Kind = AssetWorkflow
			}
		}
	}
	return info, true
}

// AssetFileHistory is the kind of Claude Code's per-edit snapshots under
// <profile>/file-history/<sid>/.
const AssetFileHistory = "file-history"

// ClassifyFileHistory classifies path under a file-history directory dir
// (<profile>/file-history): the first component below dir is the session id.
func ClassifyFileHistory(dir, path string) (AssetInfo, bool) {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return AssetInfo{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return AssetInfo{}, false
	}
	return AssetInfo{SessionID: parts[0], Kind: AssetFileHistory}, true
}

// Kinds of the other profile folders Claude Code's retention sweep deletes.
// Each kind is the directory name under <profile>.
const (
	AssetPlans      = "plans"
	AssetPasteCache = "paste-cache"
	AssetTasks      = "tasks"
	AssetUploads    = "uploads"
)

// SweptDirs lists the profile folders archived next to file-history, in
// addition to projects/.
var SweptDirs = []string{AssetPlans, AssetPasteCache, AssetTasks, AssetUploads}

// ClassifyProfileDir classifies path under dir (<profile>/<kind>). For the
// session-scoped kinds (uploads, tasks) the first component below dir is the
// session id when the path has at least two components. Every other file
// belongs to the pseudo-session "<kind>:<profile>", like "memory:<slug>".
func ClassifyProfileDir(dir, kind, profile, path string) (AssetInfo, bool) {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return AssetInfo{}, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if (kind == AssetUploads || kind == AssetTasks) && len(parts) >= 2 {
		return AssetInfo{SessionID: parts[0], Kind: kind}, true
	}
	return AssetInfo{SessionID: kind + ":" + profile, Kind: kind}, true
}

// IsProfileDirSession reports whether id is a pseudo-session id of one of the
// folders in SweptDirs ("<kind>:<profile>") or a per-file id below it.
func IsProfileDirSession(id string) bool {
	for _, k := range SweptDirs {
		if strings.HasPrefix(id, k+":") {
			return true
		}
	}
	return false
}

// ProfileDirRel returns path below the nearest enclosing directory named
// kind, with forward slashes; the base name when there is no such directory.
func ProfileDirRel(path, kind string) string {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == kind {
			return strings.Join(parts[i+1:], "/")
		}
	}
	return parts[len(parts)-1]
}

// DocSession is the per-file session id of a searchable file in a profile
// folder: "<kind>:<profile>/<path below the folder>". sessionID is the
// pseudo-session id from ClassifyProfileDir.
func DocSession(sessionID, kind, path string) string {
	return sessionID + "/" + ProfileDirRel(path, kind)
}

// SplitDocSession splits a per-file id into the pseudo-session id and the
// path below the folder. Any other id comes back unchanged with rel "".
func SplitDocSession(id string) (sessionID, rel string) {
	if IsProfileDirSession(id) {
		if i := strings.IndexByte(id, '/'); i >= 0 {
			return id[:i], id[i+1:]
		}
	}
	return id, ""
}
