package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wamra/session-index/internal/config"
)

// cmdConfig prints the effective configuration, or with `init` writes a
// commented default config file.
func cmdConfig(args []string, out, errw io.Writer) int {
	fs := newFlags("config", errw)
	force := fs.Bool("force", false, "init: overwrite an existing config file")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	path := configPath()
	switch {
	case len(pos) == 0:
		c, found, err := config.Load(path, os.Getenv)
		if err != nil {
			fmt.Fprintf(errw, "config: %v\n", err)
			return 1
		}
		src := path + " (not found: defaults and environment only)"
		if found {
			src = path
		}
		if err := c.Resolved().Validate(); err != nil {
			fmt.Fprintf(errw, "config: %v\n", err)
			return 1
		}
		out.Write(config.Effective(c, src, os.Getenv))
		return 0
	case len(pos) == 1 && pos[0] == "init":
		if _, err := os.Stat(path); err == nil && !*force {
			fmt.Fprintf(errw, "config init: %s exists (use --force to overwrite)\n", path)
			return 1
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(errw, "config init: %v\n", err)
			return 1
		}
		if err := os.WriteFile(path, config.Init(), 0o644); err != nil {
			fmt.Fprintf(errw, "config init: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "wrote %s\n", path)
		return 0
	}
	fmt.Fprintln(errw, "config: usage: config [init [--force]]")
	return 2
}
