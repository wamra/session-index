package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wamra/session-index/internal/hooks"
	"github.com/wamra/session-index/internal/launchd"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/skills"
)

func cmdInstallHooks(args []string, out, errw io.Writer) int {
	fs := newFlags("install-hooks", errw)
	settings := fs.String("settings", settingsPathDefault(), "Claude Code settings.json")
	useLaunchd := fs.Bool("launchd", false, "also write the optional launchd sweep plist (off by default; see docs/REFERENCE.md)")
	interval := fs.Int("interval-min", 10, "launchd sweep interval in minutes (only with --launchd)")
	dry := fs.Bool("dry-run", false, "print the resulting settings JSON; write nothing")
	skill := fs.Bool("skill", false, "also install the session-search skill into this profile's skills dir")
	skillAll := fs.Bool("skill-all-profiles", false, "also install the session-search skill into every profile's skills dir")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	bin, err := binaryPath()
	if err != nil {
		fmt.Fprintf(errw, "install-hooks: %v\n", err)
		return 1
	}
	if *dry {
		b, changed, err := hooks.Render(*settings, bin)
		if err != nil {
			fmt.Fprintf(errw, "install-hooks: %v\n", err)
			return 1
		}
		fmt.Fprint(out, string(b))
		msg := "dry run: settings would change: %v"
		if *useLaunchd {
			msg += "; launchd plist would be written to " + plistPathDefault()
		}
		fmt.Fprintf(errw, msg+"\n", changed)
		if *skill || *skillAll {
			for _, d := range skillDirs(*skillAll) {
				fmt.Fprintf(errw, "dry run: skill would be written to %s\n", skillFile(d))
			}
		}
		return 0
	}
	changed, err := hooks.Install(*settings, bin, false)
	if err != nil {
		fmt.Fprintf(errw, "install-hooks: %v\n", err)
		return 1
	}
	if changed {
		fmt.Fprintf(out, "hooks added to %s\n", *settings)
	} else {
		fmt.Fprintf(out, "hooks already present in %s\n", *settings)
	}
	if *skill || *skillAll {
		for _, d := range skillDirs(*skillAll) {
			if err := installSkill(d); err != nil {
				fmt.Fprintf(errw, "install-hooks: skill: %v\n", err)
				return 1
			}
			fmt.Fprintf(out, "skill installed: %s\n", skillFile(d))
		}
	}
	if !*useLaunchd {
		return 0
	}
	pp := plistPathDefault()
	if err := launchd.Write(pp, bin, *interval); err != nil {
		fmt.Fprintf(errw, "install-hooks: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "wrote %s\nto load it run:\n  launchctl bootstrap gui/$(id -u) %s\n", pp, filepath.Clean(pp))
	return 0
}

func skillFile(profileDir string) string {
	return filepath.Join(profileDir, "skills", "session-search", "SKILL.md")
}

// skillDirs lists the profile directories that receive the skill: the own
// ~/.claude (resolved) and, with all, every directory under the profiles dir.
func skillDirs(all bool) []string {
	own := claudeDir()
	if r, err := filepath.EvalSymlinks(own); err == nil {
		own = r
	}
	dirs := []string{own}
	if all {
		if _, pdir := ownProfile(); pdir != "" {
			for _, p := range paths.ProfileDirs(pdir) {
				if p.Path != own {
					dirs = append(dirs, p.Path)
				}
			}
		}
	}
	return dirs
}

func installSkill(profileDir string) error {
	f := skillFile(profileDir)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	return os.WriteFile(f, skills.SessionSearch, 0o644)
}
