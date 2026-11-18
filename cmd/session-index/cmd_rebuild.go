package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/ingest"
	"github.com/wamra/session-index/internal/summarize"
)

// cmdRebuildDerived empties the derived tables and re-derives them from
// records.raw. records and files are never touched.
func cmdRebuildDerived(args []string, out, errw io.Writer) int {
	fs := newFlags("rebuild-derived", errw)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	st, err := openStoreWith(dbPath(), true, nil) // the configured size may differ: this command applies it
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer closeWriter(st, errw)
	ctx := context.Background()
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(errw, "rebuild-derived: %v\n", err)
		return 1
	}
	emb, err := newEmbedder(cfg, nil)
	if err != nil {
		fmt.Fprintf(errw, "rebuild-derived: %v\n", err)
		return 1
	}
	if emb.Dims() == 0 { // unknown model size: ask the provider once
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, perr := emb.Embed(pctx, []string{"dimension probe"}, "")
		cancel()
		if perr != nil {
			fmt.Fprintf(errw, "rebuild-derived: cannot detect embedding dimensions (%v); set embed.dims in %s\n", perr, configPath())
			return 1
		}
	}
	lk, err := embed.TryLock(st)
	if err != nil {
		if errors.Is(err, embed.ErrLocked) {
			fmt.Fprintln(errw, "rebuild-derived: embed running, retry")
		} else {
			fmt.Fprintf(errw, "rebuild-derived: %v\n", err)
		}
		return 1
	}
	defer lk.Release()
	n, err := ingest.RederiveDims(ctx, st, emb.Fingerprint(), emb.Dims())
	if err != nil {
		fmt.Fprintf(errw, "rebuild-derived: %v\n", err)
		return 1
	}
	var chunks int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM chunks`).Scan(&chunks); err != nil {
		fmt.Fprintf(errw, "rebuild-derived: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "re-derived %d records into %d chunks (vectors: %d dims, embedder %s); run `session-index embed` to re-embed\n", n, chunks, emb.Dims(), emb.Fingerprint())
	return 0
}

// cmdRebuildExtras re-derives file_changes, tool_outputs and the summaries and
// chunks that hang off them (tool-output summaries that still match are kept).
func cmdRebuildExtras(args []string, out, errw io.Writer) int {
	fs := newFlags("rebuild-extras", errw)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer closeWriter(st, errw)
	lk, err := embed.TryLock(st)
	if err != nil {
		fmt.Fprintf(errw, "rebuild-extras: embed running, retry (%v)\n", err)
		return 1
	}
	defer lk.Release()
	sl, err := summarize.TryLock(st)
	if err != nil {
		fmt.Fprintf(errw, "rebuild-extras: summarize running, retry (%v)\n", err)
		return 1
	}
	defer sl.Release()
	rep, err := ingest.RebuildExtras(context.Background(), st)
	if err != nil {
		fmt.Fprintf(errw, "rebuild-extras: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "rebuild-extras: file_changes %d -> %d, tool_outputs %d -> %d; summaries kept %d, deleted %d; chunks deleted %d; run `session-index embed` to embed re-derived memory and plan chunks\n",
		rep.FileChangesBefore, rep.FileChangesAfter, rep.ToolOutputsBefore, rep.ToolOutputsAfter, rep.SummariesKept, rep.SummariesDeleted, rep.ChunksDeleted)
	return 0
}

// cmdRepair removes derived rows whose record is missing or superseded,
// recomputes affected sessions and rebuilds both FTS indexes. No embedding.
func cmdRepair(args []string, out, errw io.Writer) int {
	fs := newFlags("repair", errw)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer closeWriter(st, errw)
	lk, err := embed.TryLock(st)
	if err != nil {
		if errors.Is(err, embed.ErrLocked) {
			fmt.Fprintln(errw, "repair: embed running, retry")
		} else {
			fmt.Fprintf(errw, "repair: %v\n", err)
		}
		return 1
	}
	defer lk.Release()
	rep, err := ingest.Repair(context.Background(), st)
	if err != nil {
		fmt.Fprintf(errw, "repair: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "repair: deleted %d orphan messages, %d chunks, %d chunk_vec rows, %d tool_fts rows; recomputed %d sessions; rebuilt chunk_fts and tool_fts\n",
		rep.Messages, rep.Chunks, rep.ChunkVec, rep.ToolFTS, rep.Sessions)
	return 0
}

// cmdVacuum rebuilds the own archive file to reclaim free pages. Peer
// archives are never vacuumed.
func cmdVacuum(args []string, out, errw io.Writer) int {
	fs := newFlags("vacuum", errw)
	var dbs repeatFlag
	fs.Var(&dbs, "db", "not accepted: only the own archive is vacuumed")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	if len(dbs) > 0 {
		fmt.Fprintln(errw, "vacuum: --db is not allowed: only the own archive is ever vacuumed")
		return 2
	}
	st, err := openStore()
	if err != nil {
		fmt.Fprintf(errw, "open archive: %v\n", err)
		return 1
	}
	defer st.Close()
	lk, err := embed.TryLock(st)
	if err != nil {
		if errors.Is(err, embed.ErrLocked) {
			fmt.Fprintln(errw, "vacuum: embed running, retry")
		} else {
			fmt.Fprintf(errw, "vacuum: %v\n", err)
		}
		return 1
	}
	defer lk.Release()
	ctx := context.Background()
	// Fold the WAL in first so "before" is the real file size.
	if err := st.Checkpoint(); err != nil {
		fmt.Fprintf(errw, "vacuum: checkpoint: %v\n", err)
		return 1
	}
	before := fileSize(st.Path())
	// One connection runs both statements; VACUUM cannot run in a transaction.
	conn, err := st.DB().Conn(ctx)
	if err != nil {
		fmt.Fprintf(errw, "vacuum: %v\n", err)
		return 1
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		conn.Close()
		fmt.Fprintf(errw, "vacuum: %v\n", err)
		return 1
	}
	var busy, logFrames, ckpt int
	err = conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &ckpt)
	conn.Close()
	if err != nil || busy != 0 {
		fmt.Fprintf(errw, "vacuum: wal checkpoint failed (busy=%d): %v\n", busy, err)
		return 1
	}
	after := fileSize(st.Path())
	fmt.Fprintf(out, "vacuum: %s -> %s, reclaimed %d bytes (%s)\n", humanBytes(before), humanBytes(after), before-after, humanBytes(before-after))
	return 0
}

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return 0
}
