package main

import (
	"context"
	"fmt"
	"github.com/wamra/session-index/internal/config"
	"io"
	"os"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/summarize"
)

func cmdEmbed(args []string, out, errw io.Writer) int {
	fs := newFlags("embed", errw)
	batch := fs.Int("batch", 128, "chunks per embedding request")
	max := fs.Int("max", 0, "stop after about N chunks (0 = all pending)")
	sumBudget := fs.Duration("summarize-budget", envDuration("SESSION_INDEX_SUMMARIZE_BUDGET"), "after draining, summarize pending items for up to this long, then embed the new summaries (0 = off)")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	st, err := openStoreStrict()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer closeWriter(st, errw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := *batch
	if *max > 0 {
		if b <= 0 || b > *max {
			b = *max // bounds the overshoot to one batch
		}
		start, err := embed.ChunksPending(ctx, st)
		if err != nil {
			fmt.Fprintf(errw, "embed: %v\n", err)
			return 1
		}
		go func() {
			t := time.NewTicker(50 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if p, err := embed.ChunksPending(ctx, st); err == nil && start-p >= *max {
						cancel()
						return
					}
				}
			}
		}()
	}
	code, locked := drainLocked(ctx, st, b, true, out, errw)
	if locked {
		return 0
	}
	if *sumBudget > 0 && code == 0 {
		// Hook-spawned: tolerate an unreachable Ollama, then embed what was written.
		runSummarize(st, summarize.Opts{Parallel: defaultSummarizeParallel}, "", *sumBudget, false, out, errw)
		drainLocked(context.Background(), st, b, false, out, errw)
	}
	if n, err := embed.ChunksPending(context.Background(), st); err == nil && code == 0 {
		fmt.Fprintf(out, "%d chunks pending\n", n)
	}
	return code
}

// envDuration parses a duration from an environment variable (0 when unset or invalid).
func envDuration(key string) time.Duration {
	d, _ := time.ParseDuration(config.WithLegacy(os.Getenv)(key))
	return d
}
