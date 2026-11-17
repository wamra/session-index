package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wamra/session-index/internal/ingest"
	"github.com/wamra/session-index/internal/provider"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/summarize"
)

func cmdSummarize(args []string, out, errw io.Writer) int {
	fs := newFlags("summarize", errw)
	maxN := fs.Int("max", 0, "stop after about N summaries (0 = all pending)")
	budget := fs.Duration("budget", 0, "stop after this long (0 = no limit)")
	model := fs.String("model", "", "summarizer model (default: summarize.model from the config)")
	parallel := fs.Int("parallel", defaultSummarizeParallel, "concurrent requests")
	kinds := fs.String("kind", "", "restrict to tool, change or asset (comma separated; default all)")
	retry := fs.Bool("retry-failed", false, "first delete parked failures (all, or only the reason given as an argument, e.g. timeout) so they are retried")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) > 1 || (len(pos) == 1 && !*retry) {
		fmt.Fprintln(errw, "summarize: usage: summarize [flags] [--retry-failed [REASON]]")
		return 2
	}
	var ks []string
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.TrimSpace(k); k != "" {
			if k != summarize.KindTool && k != summarize.KindChange && k != summarize.KindAsset {
				fmt.Fprintf(errw, "summarize: unknown --kind %q (tool, change, asset)\n", k)
				return 2
			}
			ks = append(ks, k)
		}
	}
	st, err := openStoreStrict()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer closeWriter(st, errw)
	if *retry {
		reason := ""
		if len(pos) == 1 {
			reason = pos[0]
		}
		n, err := summarize.RetryFailed(context.Background(), st.DB(), reason)
		if err != nil {
			fmt.Fprintf(errw, "summarize: retry-failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "retry-failed: removed %d parked failures\n", n)
	}
	if _, err := ingest.BackfillExtras(context.Background(), st); err != nil {
		fmt.Fprintf(errw, "summarize: backfill: %v\n", err)
		return 1
	}
	return runSummarize(st, summarize.Opts{Max: *maxN, Parallel: *parallel, Kinds: ks}, *model, *budget, true, out, errw)
}

// defaultSummarizeParallel is the measured best concurrency (see docs/REFERENCE.md).
const defaultSummarizeParallel = 1

// runSummarize takes summarize.lock, runs the queue and reports. With strict
// false (hook and sweep contexts) an unreachable provider is not an error.
// A non-empty model overrides the configured summarize.model.
func runSummarize(st *store.Store, o summarize.Opts, model string, budget time.Duration, strict bool, out, errw io.Writer) int {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(errw, "summarize: %v\n", err)
		return 1
	}
	if !*cfg.Summarize.Enabled {
		fmt.Fprintln(errw, "summarize: disabled (summarize.enabled is false in the config)")
		return 0
	}
	sum, err := newSummarizer(cfg, model)
	if err != nil {
		fmt.Fprintf(errw, "summarize: %v\n", err)
		return 1
	}
	if m := cfg.Summarize.MaxItemsPerRun; m > 0 && (o.Max == 0 || o.Max > m) {
		o.Max = m
	}
	o.MaxTokens = cfg.Summarize.MaxTokens
	o.MaxInputChars = cfg.Summarize.MaxInputChars
	o.MaxPerDay, _ = cfg.Summarize.DailyCap()
	if len(o.Kinds) == 0 {
		ks, err := summarize.KindsFromConfig(cfg.Summarize.Kinds)
		if err != nil {
			fmt.Fprintf(errw, "summarize: %v\n", err)
			return 1
		}
		o.Kinds = ks
	}
	lk, err := summarize.TryLock(st)
	if err != nil {
		if errors.Is(err, summarize.ErrLocked) {
			fmt.Fprintln(errw, "summarize: another summarize is running")
			return 0
		}
		fmt.Fprintf(errw, "summarize: %v\n", err)
		return 1
	}
	defer lk.Release()
	ctx := context.Background()
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	start := time.Now()
	stats, err := summarize.Run(ctx, st, sum, o)
	el := time.Since(start)
	rate := 0.0
	if el > 0 {
		rate = float64(stats.Done+stats.Failed) / el.Seconds()
	}
	fmt.Fprintf(out, "summarized %d items (%d failed) in %s: %.2f items/s (parallel %d, model %s)\n",
		stats.Done, stats.Failed, el.Round(10*time.Millisecond), rate, max(o.Parallel, 1), sum.Fingerprint())
	var capErr *summarize.ErrDailyCap
	switch {
	case err == nil, errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return 0
	case errors.As(err, &capErr):
		fmt.Fprintf(out, "summarize: %v (summarize.max_items_per_day)\n", capErr)
		return 0
	case errors.Is(err, provider.ErrAuth):
		fmt.Fprintf(errw, "summarize: %s; remaining items stay pending\n", authLine(err, cfg.Summarize.Provider, cfg.Summarize.BaseURL, cfg.Summarize.APIKeyEnv))
		return 1
	case summarize.Down(err):
		fmt.Fprintf(errw, "summarize: %s unreachable at %s (%v); remaining items stay pending\n", cfg.Summarize.Provider, sumEndpoint(cfg.Summarize), err)
		if strict {
			return 1
		}
		return 0
	default:
		fmt.Fprintf(errw, "summarize: %v\n", err)
		return 1
	}
}
