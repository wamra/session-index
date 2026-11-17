package paths

import (
	"os"
	"path/filepath"
	"sort"
)

// ArchiveRel is the archive database location relative to a profile dir.
var ArchiveRel = filepath.Join("cache", "session-archive", "archive.sqlite")

// Peer is another profile's archive database.
type Peer struct {
	Name string // profile name
	Path string // archive.sqlite path
}

// Profiles locates the own profile from claudeDir (normally ~/.claude): the
// symlink is resolved, the own profile name is the basename of the resolved
// directory and the profiles directory is its parent.
func Profiles(claudeDir string) (own, profilesDir string) {
	real, err := filepath.EvalSymlinks(claudeDir)
	if err != nil {
		real = filepath.Clean(claudeDir)
	}
	return filepath.Base(real), filepath.Dir(real)
}

// ProfileRoots lists <profilesDir>/*/projects directories (sorted by profile
// name) as Peer{Name: profile, Path: projects dir}.
func ProfileRoots(profilesDir string) []Peer {
	return glob(profilesDir, "projects", true, "")
}

// DiscoverPeers lists <profilesDir>/*/cache/session-archive/archive.sqlite
// files, excluding the profile named own.
func DiscoverPeers(profilesDir, own string) []Peer {
	return glob(profilesDir, ArchiveRel, false, own)
}

// ProfileDirs lists the profile directories under profilesDir.
func ProfileDirs(profilesDir string) []Peer {
	return glob(profilesDir, "", true, "")
}

func glob(profilesDir, rel string, wantDir bool, skip string) []Peer {
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return nil
	}
	var out []Peer
	for _, e := range entries {
		if e.Name() == skip || e.Name()[0] == '.' {
			continue
		}
		p := filepath.Join(profilesDir, e.Name(), rel)
		if fi, err := os.Stat(p); err != nil || fi.IsDir() != wantDir {
			continue
		}
		out = append(out, Peer{Name: e.Name(), Path: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
