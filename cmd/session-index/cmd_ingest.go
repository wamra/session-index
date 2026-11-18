package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/hooks"
	"github.com/wamra/session-index/internal/ingest"
	"github.com/wamra/session-index/internal/paths"
	"github.com/wamra/session-index/internal/store"
)

const stdinTimeout = 300 * time.Millisecond

func cmdIngest(args []string, out, errw io.Writer) (code int) {
	fs := newFlags("ingest", errw)
	transcript := fs.String("transcript", "", "transcript .jsonl path")
	hook := fs.Bool("stdin-hook", false, "read Claude Code hook JSON from stdin")
	emb := fs.Bool("embed", false, "start a detached `embed` after ingest")
	root := fs.String("root", defaultRoot(), "transcripts root")
	profileFlag := fs.String("profile", "", "profile name (default: this machine's profile)")
	if _, err := parseInterspersed(fs, args); err != nil {
		for _, a := range args {
			if a == "--stdin-hook" || a == "-stdin-hook" {
				return 0 // a hook must never fail the host
			}
		}
		return 2
	}
	if *hook {
		// A bug in hook mode must never block Claude Code: log and exit 0.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(errw, "session-index: panic in hook mode: %v\n%s", r, debug.Stack())
				code = 0
			}
		}()
	}
	if (*transcript == "") == !*hook {
		fmt.Fprintln(errw, "ingest: exactly one of --transcript or --stdin-hook is required")
		return 2
	}

	var path, sid, event string
	if *hook {
		type res struct {
			in  hooks.HookInput
			err error
		}
		ch := make(chan res, 1)
		go func() {
			in, err := hooks.ParseStdin(stdin)
			ch <- res{in, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				fmt.Fprintf(errw, "session-index: %v\n", r.err)
				return 0 // a hook must never fail the host
			}
			path, sid, event = r.in.TranscriptPath, r.in.SessionID, r.in.HookEventName
		case <-time.After(stdinTimeout):
			fmt.Fprintln(errw, "session-index: timed out waiting for hook stdin; skipping")
			return 0
		}
	} else {
		var err error
		if path, err = filepath.Abs(*transcript); err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		sid = ""
	}

	// The projects root: the configured one if the transcript lives under it,
	// otherwise <path>/../.. (layout <root>/<slug>/<sid>.jsonl).
	r := *root
	if _, ok := paths.Classify(r, path); !ok {
		r = filepath.Dir(filepath.Dir(path))
	}
	if info, ok := paths.Classify(r, path); ok && sid == "" {
		sid = info.SessionID
	}

	profile := *profileFlag
	if profile == "" {
		profile, _ = ownProfile()
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		if *hook {
			hookLockedNotice(out, event, err)
		}
		return hookExit(*hook)
	}
	defer closeWriter(st, errw)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	files := []string{path}
	if sid != "" {
		sub := filepath.Join(filepath.Dir(path), sid)
		if fi, err := os.Stat(sub); err == nil && fi.IsDir() {
			if err := paths.Walk(sub, func(p string) { files = append(files, p) }); err != nil {
				fmt.Fprintf(errw, "session-index: walk %s: %v\n", sub, err)
			}
		}
	}
	var lines, chunks, failed int
	for _, p := range files {
		res, err := ingest.File(ctx, st, r, p, profile)
		if err != nil {
			fmt.Fprintf(errw, "session-index: %v\n", err)
			failed++
			continue
		}
		lines += res.NewLines
		chunks += res.NewChunks
	}
	assets := ingestAssets(ctx, st, r, path, sid, profile, errw)
	if !*hook {
		fmt.Fprintf(out, "ingested %d files: %d new lines, %d new chunks, %d failed; %d assets archived\n", len(files), lines, chunks, failed, assets)
	}
	if *emb {
		if err := spawnEmbed(st); err != nil {
			fmt.Fprintf(errw, "session-index: start embed: %v\n", err)
		}
	}
	if failed > 0 && !*hook {
		return 1
	}
	return 0
}

// ingestAssets archives the non-transcript files of the session subtree, the
// slug's memory/*.md files, the session's file-history snapshots, uploads and
// tasks, and the profile's plans and paste-cache, then resolves tool-result
// pointers. Files of the profile folders that are dataless cloud placeholders
// are skipped, so the hook never waits for a download. It returns how many
// assets were stored.
func ingestAssets(ctx context.Context, st *store.Store, root, path, sid, profile string, errw io.Writer) int {
	n := 0
	add := func(res ingest.AssetResult, err error, p string) {
		if err != nil {
			fmt.Fprintf(errw, "session-index: asset %s: %v\n", p, err)
		} else if res.Archived {
			n++
		}
	}
	slugDir := filepath.Dir(path)
	var dirs []string
	if sid != "" {
		dirs = append(dirs, filepath.Join(slugDir, sid))
	}
	dirs = append(dirs, filepath.Join(slugDir, "memory"))
	for _, d := range dirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			continue
		}
		var list []string
		paths.WalkAssets(d, func(p string) { list = append(list, p) })
		for _, p := range list {
			res, err := ingest.Asset(ctx, st, root, p, profile)
			add(res, err, p)
		}
	}
	if sid != "" {
		fh := filepath.Join(filepath.Dir(filepath.Clean(root)), "file-history")
		d := filepath.Join(fh, sid)
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			var list []string
			paths.WalkAssets(d, func(p string) { list = append(list, p) })
			for _, p := range list {
				res, err := ingest.FileHistory(ctx, st, fh, p, profile)
				add(res, err, p)
			}
		}
	}
	profDir := filepath.Dir(filepath.Clean(root))
	for _, kind := range paths.SweptDirs {
		kindDir := filepath.Join(profDir, kind)
		d := kindDir
		if kind == paths.AssetUploads || kind == paths.AssetTasks {
			if sid == "" {
				continue
			}
			d = filepath.Join(kindDir, sid)
		}
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			continue
		}
		var list []string
		paths.WalkAssets(d, func(p string) { list = append(list, p) })
		for _, p := range list {
			if isDataless(p) {
				continue
			}
			res, err := ingest.ProfileDirAsset(ctx, st, kindDir, kind, p, profile)
			add(res, err, p)
		}
	}
	if _, err := ingest.ResolvePointers(ctx, st.DB()); err != nil {
		fmt.Fprintf(errw, "session-index: resolve tool-result pointers: %v\n", err)
	}
	return n
}

func hookExit(hook bool) int {
	if hook {
		return 0
	}
	return 1
}

// selfPath resolves the running binary (a var so tests can substitute a fake).
var selfPath = binaryPath

// spawnEmbed starts `<self> embed` detached, logging to <db dir>/embed.log.
var spawnEmbed = func(st *store.Store) error {
	self, err := selfPath()
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(filepath.Dir(st.Path()), "embed.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(self, "embed")
	// The detached child summarizes for a bounded time after draining embeddings.
	cmd.Env = append(os.Environ(), "SESSION_INDEX_SUMMARIZE_BUDGET=45s")
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = lf, lf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func cmdSweep(args []string, out, errw io.Writer) int {
	fs := newFlags("sweep", errw)
	root := fs.String("root", defaultRoot(), "transcripts root")
	profileFlag := fs.String("profile", "", "profile name for --root (default: this machine's profile)")
	all := fs.Bool("all-profiles", false, "sweep <profiles>/*/projects, each labelled with its directory name")
	emb := fs.Bool("embed", false, "embed pending chunks after the sweep")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	rootSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "root" || f.Name == "profile" {
			rootSet = true
		}
	})
	if *all && rootSet {
		fmt.Fprintln(errw, "sweep: --all-profiles cannot be combined with --root or --profile")
		return 2
	}
	type target struct{ name, root string }
	var targets []target
	if *all {
		_, pdir := ownProfile()
		if pdir == "" {
			fmt.Fprintln(errw, "sweep: --all-profiles needs ~/.claude to resolve into a profiles directory")
			return 1
		}
		own, _ := ownProfile()
		for _, p := range paths.ProfileRoots(pdir) {
			r := p.Path
			// The own profile is archived under its ~/.claude/projects path
			// spelling; keep using it so files.path stays one row per file.
			if p.Name == own && sameDir(r, defaultRoot()) {
				r = defaultRoot()
			}
			targets = append(targets, target{p.Name, r})
		}
		if len(targets) == 0 {
			fmt.Fprintf(errw, "sweep: no <profile>/projects directories under %s\n", pdir)
			return 1
		}
	} else {
		name := *profileFlag
		if name == "" {
			name, _ = ownProfile()
		}
		targets = []target{{name, *root}}
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer closeWriter(st, errw)
	ctx := context.Background()
	var total ingest.Summary
	for _, t := range targets {
		sum, err := ingest.Sweep(ctx, st, t.root, t.name)
		if err != nil {
			fmt.Fprintf(errw, "sweep %s: %v\n", t.name, err)
			return 1
		}
		if *all {
			fmt.Fprintf(out, "profile %s: swept %d files: %d new lines, %d new chunks, %d skipped, %d failed; %d assets (%d archived, %d too large, %d ignored, %d failed)\n",
				t.name, sum.Files, sum.NewLines, sum.NewChunks, sum.Skipped, sum.Failed, sum.Assets, sum.AssetsArchived, sum.AssetsSkipped, sum.AssetsIgnored, sum.AssetsFailed)
		}
		total.Files += sum.Files
		total.NewLines += sum.NewLines
		total.NewChunks += sum.NewChunks
		total.Skipped += sum.Skipped
		total.Failed += sum.Failed
		total.Assets += sum.Assets
		total.AssetsArchived += sum.AssetsArchived
		total.AssetsSkipped += sum.AssetsSkipped
		total.AssetsIgnored += sum.AssetsIgnored
		total.AssetsFailed += sum.AssetsFailed
	}
	fmt.Fprintf(out, "swept %d files: %d new lines, %d new chunks, %d skipped, %d failed; %d assets (%d archived, %d too large, %d ignored, %d failed)\n",
		total.Files, total.NewLines, total.NewChunks, total.Skipped, total.Failed, total.Assets, total.AssetsArchived, total.AssetsSkipped, total.AssetsIgnored, total.AssetsFailed)
	if *emb {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		return drainReport(dctx, st, 128, false, out, errw)
	}
	return 0
}

func cmdBackfill(args []string, out, errw io.Writer) int {
	fs := newFlags("backfill", errw)
	root := fs.String("root", defaultRoot(), "transcripts root")
	emb := fs.Bool("embed", false, "embed pending chunks after archiving")
	deadline := fs.Duration("embed-deadline", 20*time.Minute, "stop embedding after this long")
	profile := fs.String("profile", "", "profile name (default: this machine's profile)")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	if *profile == "" {
		*profile, _ = ownProfile()
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer closeWriter(st, errw)
	ctx := context.Background()
	sum, err := ingest.Sweep(ctx, st, *root, *profile) // archiving is never cut off by the embed deadline
	if err != nil {
		fmt.Fprintf(errw, "backfill: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "archived %d files: %d new lines, %d new chunks, %d skipped, %d failed; %d assets (%d archived, %d too large, %d ignored, %d failed)\n",
		sum.Files, sum.NewLines, sum.NewChunks, sum.Skipped, sum.Failed, sum.Assets, sum.AssetsArchived, sum.AssetsSkipped, sum.AssetsIgnored, sum.AssetsFailed)
	code := 0
	if *emb {
		dctx, cancel := context.WithTimeout(ctx, *deadline)
		code = drainReport(dctx, st, 128, false, out, errw)
		cancel()
	}
	if n, err := embed.ChunksPending(ctx, st); err == nil {
		fmt.Fprintf(out, "%d chunks pending embedding\n", n)
	}
	return code
}

// sameDir reports whether a and b resolve to the same directory.
func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
