// Command session-index archives Claude Code transcripts into SQLite and
// searches them.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sort"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = ""

func cmdVersion(_ []string, out, _ io.Writer) int {
	v := version
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	fmt.Fprintln(out, "session-index", v)
	return 0
}

// stdin is the hook input source (a var so tests can inject).
var stdin io.Reader = os.Stdin

type command struct {
	run  func(args []string, out, errw io.Writer) int
	help string
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"backfill":        {cmdBackfill, "backfill [--root DIR] [--profile NAME] [--embed] [--embed-deadline 20m]"},
		"ingest":          {cmdIngest, "ingest (--transcript PATH | --stdin-hook) [--profile NAME] [--embed]"},
		"sweep":           {cmdSweep, "sweep [--root DIR --profile NAME | --all-profiles] [--embed]"},
		"embed":           {cmdEmbed, "embed [--batch 128] [--max N] [--summarize-budget 45s]"},
		"search":          {cmdSearch, "search QUERY [--mode auto|vector|fts|hybrid] [--k 10] [--since 7d|24h|YYYY-MM-DD] [--project SUBSTR] [--role user|assistant] [--kind main|subagent|workflow|plan] [--profile NAME] [--db PATH]... [--json]"},
		"show":            {cmdShow, "show SESSION_ID [--around UUID] [--n 20] [--changes]"},
		"files":           {cmdFiles, "files SUBSTRING [--profile NAME] [--limit 200] [--json]"},
		"summarize":       {cmdSummarize, "summarize [--max N] [--budget 60s] [--model NAME] [--parallel P] [--kind tool,change,asset] [--retry-failed [REASON]]"},
		"restore":         {cmdRestore, "restore SESSION_ID|plans:PROFILE|paste-cache:PROFILE|tasks:PROFILE|uploads:PROFILE (--to DIR | --in-place) [--with-file-history] [--with-profile-dirs]"},
		"stats":           {cmdStats, "stats"},
		"doctor":          {cmdDoctor, "doctor [--full]"},
		"install-hooks":   {cmdInstallHooks, "install-hooks [--settings PATH] [--interval-min 10] [--dry-run] [--skill] [--skill-all-profiles]"},
		"repair":          {cmdRepair, "repair"},
		"vacuum":          {cmdVacuum, "vacuum"},
		"rebuild-derived": {cmdRebuildDerived, "rebuild-derived"},
		"rebuild-extras":  {cmdRebuildExtras, "rebuild-extras"},
		"version":         {cmdVersion, "version"},
		"config":          {cmdConfig, "config [init [--force]]"},
		"encrypt":         {cmdEncrypt, "encrypt [--wait 30s] [--recover]"},
		"decrypt":         {cmdDecrypt, "decrypt [--wait 30s] [--recover]"},
		"unlock":          {cmdUnlock, "unlock"},
		"lock":            {cmdLock, "lock"},
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: session-index <command> [flags]")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(w, "  "+commands[n].help)
	}
}

func run(args []string, out, errw io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(errw)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	c, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(errw, "session-index: unknown command %q\n", args[0])
		usage(errw)
		return 2
	}
	return c.run(args[1:], out, errw)
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// newFlags returns a FlagSet that reports errors to errw without exiting.
func newFlags(name string, errw io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errw)
	return fs
}

// parseInterspersed parses flags that may appear before or after positional
// arguments and returns the positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}
