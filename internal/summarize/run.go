package summarize

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wamra/session-index/internal/filelock"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wamra/session-index/internal/ingest"
	"github.com/wamra/session-index/internal/jsonl"
	"github.com/wamra/session-index/internal/provider"
	"github.com/wamra/session-index/internal/store"
	"github.com/wamra/session-index/internal/zst"
)

// MinToolBytes is the smallest inline tool output that gets a summary.
const MinToolBytes = 2048

// Kinds of summary targets.
const (
	KindTool   = "tool"   // inline tool_result block
	KindChange = "change" // file_changes row
	KindAsset  = "asset"  // tool-results file
)

// BreakerThreshold is the number of consecutive item failures with an identical
// reason after which a run stops (a request or configuration problem, not bad
// items). Items without input ("missing") and timeouts (per-item size or
// latency, not configuration) do not count.
const BreakerThreshold = 5

// TimeoutLimit is the number of consecutive timed-out items after which a run
// stops. Timed-out items are never parked: they stay pending.
const TimeoutLimit = 5

// ErrTimeouts is returned by Run after TimeoutLimit consecutive timeouts.
var ErrTimeouts = errors.New("provider timing out; stopping, items stay pending")

// RetryFailed deletes the parked failure rows (summaries.model 'failed:*', or
// only 'failed:<reason>' when reason is non-empty) so their items re-enter
// the queue. It returns how many rows were removed.
func RetryFailed(ctx context.Context, db *sql.DB, reason string) (int64, error) {
	var res sql.Result
	var err error
	if reason == "" {
		res, err = db.ExecContext(ctx, `DELETE FROM summaries WHERE model LIKE 'failed:%'`)
	} else {
		res, err = db.ExecContext(ctx, `DELETE FROM summaries WHERE model = ?`, "failed:"+reason)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// BreakerError is returned by Run when the circuit breaker trips.
type BreakerError struct{ Reason string }

func (e *BreakerError) Error() string {
	return fmt.Sprintf("stopping: %d consecutive identical item failures (%s) — likely a request/config problem", BreakerThreshold, e.Reason)
}

// ErrLocked means another summarize process holds summarize.lock.
var ErrLocked = errors.New("summarize lock held by another process")

// Target is one item to summarize.
type Target struct {
	Kind      string
	RecordID  int64
	Idx       int
	AssetID   int64
	ChangeID  int64
	SessionID string
	TS        string
	Tool      string
	FilePath  string
	system    string
	input     string
}

// Opts configures Run.
type Opts struct {
	Max      int      // stop after about N items (0 = no limit)
	Parallel int      // concurrent requests (default 1)
	Kinds    []string // restrict to these kinds (default all)
	// MaxTokens bounds each reply (0 = provider.DefaultMaxTokens).
	MaxTokens int
	// MaxInputChars clips each input (0 = provider.DefaultMaxInputChars).
	MaxInputChars int
	// MaxPerDay caps the items sent to the provider per UTC day, counted in
	// the archive's meta table across runs (0 = unlimited). When it is
	// reached Run stops with ErrDailyCap and the remaining items stay pending.
	MaxPerDay int
	// Now is the clock for the daily counter (nil = time.Now).
	Now func() time.Time
}

// ErrDailyCap is returned by Run when summarize.max_items_per_day is reached.
type ErrDailyCap struct{ Used, Cap int }

func (e *ErrDailyCap) Error() string {
	return fmt.Sprintf("daily cap reached: %d of %d summaries today (UTC); remaining items stay pending", e.Used, e.Cap)
}

// Stats is the outcome of Run.
type Stats struct{ Done, Failed int }

func (o Opts) has(kind string) bool {
	if len(o.Kinds) == 0 {
		return true
	}
	for _, k := range o.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Lock is a held summarize.lock.
type Lock struct{ l *filelock.Lock }

// TryLock takes summarize.lock (next to the database) without blocking.
func TryLock(st *store.Store) (*Lock, error) {
	l, err := filelock.Try(filepath.Join(filepath.Dir(st.Path()), "summarize.lock"))
	if errors.Is(err, filelock.ErrLocked) {
		return nil, ErrLocked
	}
	if err != nil {
		return nil, err
	}
	return &Lock{l}, nil
}

// Release drops the lock.
func (l *Lock) Release() { l.l.Release() }

// Counts reports done, failed and pending summaries. Pending tool outputs and
// changes are exact; assets counts only text tool-result files.
type Counts struct {
	Done, Failed                             int
	PendingTool, PendingChange, PendingAsset int
}

// Count returns the summary counts.
func Count(ctx context.Context, db *sql.DB) (Counts, error) {
	var c Counts
	for _, q := range []struct {
		dst *int
		sql string
	}{
		{&c.Done, `SELECT count(*) FROM summaries WHERE model NOT LIKE 'failed:%'`},
		{&c.Failed, `SELECT count(*) FROM summaries WHERE model LIKE 'failed:%'`},
		{&c.PendingTool, `SELECT count(*) FROM tool_outputs t JOIN records r ON r.id=t.record_id
			WHERE r.superseded=0 AND t.inline_bytes>=` + fmt.Sprint(MinToolBytes) + ` AND NOT EXISTS (SELECT 1 FROM summaries s WHERE s.record_id=t.record_id AND s.idx=t.idx)`},
		{&c.PendingChange, `SELECT count(*) FROM file_changes fc WHERE NOT EXISTS (SELECT 1 FROM summaries s WHERE s.change_id=fc.id)`},
		{&c.PendingAsset, assetPendingCount},
	} {
		if err := db.QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
			return c, err
		}
	}
	return c, nil
}

const assetTextCond = `a.kind='tool-result' AND a.raw IS NOT NULL AND (lower(a.path) LIKE '%.txt' OR lower(a.path) LIKE '%.md' OR lower(a.path) LIKE '%.json' OR lower(a.path) LIKE '%.js')
	AND NOT EXISTS (SELECT 1 FROM summaries s WHERE s.asset_id=a.id)`
const assetPendingCount = `SELECT count(*) FROM assets a WHERE ` + assetTextCond

// Pending returns up to limit unsummarized targets of one kind, newest first.
func Pending(ctx context.Context, db *sql.DB, kind string, limit int) ([]Target, error) {
	return PendingMax(ctx, db, kind, limit, 0)
}

// PendingMax is Pending with the input clip max_input_chars (0 = 8000).
func PendingMax(ctx context.Context, db *sql.DB, kind string, limit, maxChars int) ([]Target, error) {
	if maxChars <= 0 {
		maxChars = provider.DefaultMaxInputChars
	}
	switch kind {
	case KindTool:
		return pendingTools(ctx, db, limit, maxChars)
	case KindChange:
		return pendingChanges(ctx, db, limit, maxChars)
	case KindAsset:
		return pendingAssets(ctx, db, limit, maxChars)
	}
	return nil, fmt.Errorf("unknown summary kind %q", kind)
}

func pendingTools(ctx context.Context, db *sql.DB, limit, maxChars int) ([]Target, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.record_id, t.idx, COALESCE(r.session_id,''), COALESCE(r.ts,''), r.raw
		FROM tool_outputs t JOIN records r ON r.id=t.record_id
		WHERE r.superseded=0 AND t.inline_bytes>=? AND NOT EXISTS (SELECT 1 FROM summaries s WHERE s.record_id=t.record_id AND s.idx=t.idx)
		ORDER BY t.record_id DESC, t.idx LIMIT ?`, MinToolBytes, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		t := Target{Kind: KindTool, system: ToolPrompt}
		var raw []byte
		if err := rows.Scan(&t.RecordID, &t.Idx, &t.SessionID, &t.TS, &raw); err != nil {
			return nil, err
		}
		if line, err := zst.Decompress(raw); err == nil {
			if rec, err := jsonl.Parse(line); err == nil && rec.Msg != nil && t.Idx < len(rec.Msg.ToolResults) {
				t.input = ClipTo(rec.Msg.ToolResults[t.Idx], maxChars)
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func pendingAssets(ctx context.Context, db *sql.DB, limit, maxChars int) ([]Target, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.id, COALESCE(a.session_id,''), COALESCE(a.mtime,0), a.raw FROM assets a WHERE `+assetTextCond+` ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		t := Target{Kind: KindAsset, system: ToolPrompt}
		var mt int64
		var raw []byte
		if err := rows.Scan(&t.AssetID, &t.SessionID, &mt, &raw); err != nil {
			return nil, err
		}
		t.TS = time.Unix(0, mt).UTC().Format(time.RFC3339)
		if data, err := zst.Decompress(raw); err == nil {
			t.input = ClipTo(strings.ToValidUTF8(string(data), ""), maxChars)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func pendingChanges(ctx context.Context, db *sql.DB, limit, maxChars int) ([]Target, error) {
	rows, err := db.QueryContext(ctx, `SELECT fc.id, fc.record_id, COALESCE(fc.session_id,''), COALESCE(fc.ts,''), fc.tool, fc.file_path,
			COALESCE(fc.tool_use_id,''), fc.result_record_id, fc.result_ok, COALESCE(fc.excerpt,'')
		FROM file_changes fc WHERE NOT EXISTS (SELECT 1 FROM summaries s WHERE s.change_id=fc.id)
		ORDER BY fc.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	type row struct {
		t       Target
		tuid    string
		resRec  sql.NullInt64
		ok      sql.NullInt64
		excerpt string
	}
	var rs []row
	for rows.Next() {
		var r row
		r.t = Target{Kind: KindChange, system: ChangePrompt}
		if err := rows.Scan(&r.t.ChangeID, &r.t.RecordID, &r.t.SessionID, &r.t.TS, &r.t.Tool, &r.t.FilePath, &r.tuid, &r.resRec, &r.ok, &r.excerpt); err != nil {
			rows.Close()
			return nil, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Target
	for _, r := range rs {
		body := r.excerpt
		var raw []byte
		if db.QueryRowContext(ctx, `SELECT raw FROM records WHERE id=?`, r.t.RecordID).Scan(&raw) == nil {
			if full := fullChange(raw, r.tuid, r.t.Tool, r.t.FilePath, maxChars); full != "" {
				body = full
			}
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Tool: %s\nFile: %s\n", r.t.Tool, r.t.FilePath)
		if r.ok.Valid {
			if r.ok.Int64 == 0 {
				sb.WriteString("Outcome: error\n")
			} else {
				sb.WriteString("Outcome: ok\n")
			}
		}
		sb.WriteString("\n")
		sb.WriteString(capRunes(body, maxChars))
		if r.resRec.Valid {
			var rraw []byte
			if db.QueryRowContext(ctx, `SELECT raw FROM records WHERE id=?`, r.resRec.Int64).Scan(&rraw) == nil {
				if res := resultFor(rraw, r.tuid); res != "" {
					sb.WriteString("\n\nTool result:\n" + capRunes(res, 1000))
				}
			}
		}
		r.t.input = sb.String()
		out = append(out, r.t)
	}
	return out, nil
}

func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// resultFor returns the tool_result text answering tool_use id in a user record.
func resultFor(raw []byte, id string) string {
	line, err := zst.Decompress(raw)
	if err != nil {
		return ""
	}
	rec, err := jsonl.Parse(line)
	if err != nil || rec.Msg == nil {
		return ""
	}
	for i, ref := range rec.Msg.ResultRefs {
		if ref.ToolUseID == id && i < len(rec.Msg.ToolResults) {
			return rec.Msg.ToolResults[i]
		}
	}
	return ""
}

// fullChange rebuilds the full edit input (old/new strings, content or
// command) of the tool_use id in an assistant record.
func fullChange(raw []byte, id, tool, filePath string, maxChars int) string {
	line, err := zst.Decompress(raw)
	if err != nil {
		return ""
	}
	rec, err := jsonl.Parse(line)
	if err != nil || rec.Msg == nil {
		return ""
	}
	for _, tu := range rec.Msg.ToolUses {
		if tu.ID != id || tu.Name != tool {
			continue
		}
		var in struct {
			Old       string `json:"old_string"`
			New       string `json:"new_string"`
			Content   string `json:"content"`
			Command   string `json:"command"`
			NewSource string `json:"new_source"`
			Edits     []struct {
				Old string `json:"old_string"`
				New string `json:"new_string"`
			} `json:"edits"`
		}
		if json.Unmarshal(tu.InputJSON, &in) != nil {
			return ""
		}
		half := maxChars / 2
		switch tool {
		case "Edit":
			return "--- old ---\n" + capRunes(in.Old, half) + "\n--- new ---\n" + capRunes(in.New, half)
		case "MultiEdit":
			var sb strings.Builder
			for _, e := range in.Edits {
				sb.WriteString("--- old ---\n" + capRunes(e.Old, 1000) + "\n--- new ---\n" + capRunes(e.New, 1000) + "\n")
			}
			return sb.String()
		case "Write":
			return "--- content ---\n" + in.Content
		case "NotebookEdit":
			return "--- new source ---\n" + in.NewSource
		case "Bash":
			return "--- command ---\n" + in.Command
		}
	}
	return ""
}

type result struct {
	t    Target
	text string
	err  error
}

// Run summarizes pending targets until none are left, Max items are done,
// ctx ends, or the server goes away (provider.ErrDown, after committing
// what finished). It returns the number of summaries written and parked.
func Run(ctx context.Context, st *store.Store, c provider.Summarizer, o Opts) (Stats, error) {
	var stats Stats
	par := o.Parallel
	if par < 1 {
		par = 1
	}
	kinds := []string{KindTool, KindChange, KindAsset}
	var active []string
	for _, k := range kinds {
		if o.has(k) {
			active = append(active, k)
		}
	}
	br := &breaker{}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	for ctx.Err() == nil {
		limit := max(par*3, 6)
		if o.Max > 0 {
			limit = min(limit, o.Max-stats.Done-stats.Failed)
			if limit <= 0 {
				break
			}
		}
		if o.MaxPerDay > 0 {
			used, err := store.UsageToday(ctx, st.DB(), store.UsageSummarize, now())
			if err != nil {
				return stats, err
			}
			if used >= o.MaxPerDay {
				for _, k := range active { // nothing left to do is not a cap hit
					ts, err := PendingMax(ctx, st.DB(), k, 1, o.MaxInputChars)
					if err != nil {
						return stats, err
					}
					if len(ts) > 0 {
						return stats, &ErrDailyCap{Used: used, Cap: o.MaxPerDay}
					}
				}
				break
			}
			limit = min(limit, o.MaxPerDay-used)
		}
		per := (limit + len(active) - 1) / len(active)
		var batch []Target
		for _, k := range active {
			ts, err := PendingMax(ctx, st.DB(), k, per, o.MaxInputChars)
			if err != nil {
				return stats, err
			}
			batch = append(batch, ts...)
		}
		if len(batch) == 0 {
			break
		}
		if (o.Max > 0 || o.MaxPerDay > 0) && len(batch) > limit {
			batch = batch[:limit]
		}
		// Count the items dispatched to the provider before their results are
		// processed (own short transaction), so timeouts, provider-down items
		// and a kill mid-batch still count: the counter is "items sent", an
		// upper bound of the billed requests.
		sent := 0
		for _, t := range batch {
			if strings.TrimSpace(t.input) != "" {
				sent++
			}
		}
		if err := store.AddUsage(context.WithoutCancel(ctx), st.DB(), store.UsageSummarize, sent, now()); err != nil {
			return stats, err
		}
		d, f, err := process(ctx, st, c, batch, par, o.MaxTokens, br)
		stats.Done += d
		stats.Failed += f
		if err != nil {
			return stats, err
		}
	}
	return stats, ctx.Err()
}

// breaker counts consecutive identical item failures across batches.
type breaker struct {
	reason string
	n      int
	tmo    int // consecutive timeouts
}

// failure records an item failure and reports whether the breaker tripped.
func (b *breaker) failure(reason string) bool {
	if reason == "missing" || reason == "timeout" {
		return false
	}
	if reason != b.reason {
		b.reason, b.n = reason, 0
	}
	b.n++
	return b.n >= BreakerThreshold
}

func (b *breaker) success() { b.reason, b.n, b.tmo = "", 0, 0 }

func process(ctx context.Context, st *store.Store, c provider.Summarizer, batch []Target, par, maxTokens int, br *breaker) (done, failed int, err error) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan Target)
	results := make(chan result)
	var wg sync.WaitGroup
	for i := 0; i < par; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if strings.TrimSpace(t.input) == "" {
					results <- result{t: t, err: &ErrItem{Reason: "missing"}}
					continue
				}
				txt, err := generate(wctx, c, t.system, t.input, maxTokens)
				results <- result{t, txt, err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, t := range batch {
			select {
			case jobs <- t:
			case <-wctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	var down error
	tripped := false
	for r := range results {
		if tripped {
			continue // breaker tripped: park nothing further
		}
		var ie *ErrItem
		switch {
		case r.err == nil:
			if e := store1(ctx, st, r.t, r.text, c.Fingerprint()); e != nil {
				if down == nil {
					down = e
					cancel()
				}
				continue
			}
			br.success()
			done++
		case errors.As(r.err, &ie) && ie.Reason == "timeout":
			// per-item latency: leave the item pending, stop after a streak
			br.tmo++
			if br.tmo >= TimeoutLimit {
				down, tripped = ErrTimeouts, true
				cancel()
			}
		case errors.As(r.err, &ie):
			br.tmo = 0
			if e := store1(ctx, st, r.t, "", "failed:"+ie.Reason); e != nil {
				if down == nil {
					down = e
					cancel()
				}
				continue
			}
			failed++
			if br.failure(ie.Reason) {
				down, tripped = &BreakerError{Reason: ie.Reason}, true
				cancel()
			}
		default: // server down or context ended: park nothing
			if down == nil && !errors.Is(r.err, context.Canceled) && !errors.Is(r.err, context.DeadlineExceeded) {
				down = r.err
				cancel()
			}
		}
	}
	if down != nil {
		return done, failed, down
	}
	return done, failed, nil
}

// store1 writes one summary row (text empty = parked failure) and, for a real
// summary, its chunk, in one transaction.
func store1(ctx context.Context, st *store.Store, t Target, text, model string) error {
	tx, err := st.DB().BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	var res sql.Result
	switch t.Kind {
	case KindTool:
		res, err = tx.Exec(`INSERT OR IGNORE INTO summaries(record_id, idx, text, model, created_at) VALUES(?,?,?,?,?)`, t.RecordID, t.Idx, text, model, now)
	case KindAsset:
		res, err = tx.Exec(`INSERT OR IGNORE INTO summaries(asset_id, text, model, created_at) VALUES(?,?,?,?)`, t.AssetID, text, model, now)
	case KindChange:
		res, err = tx.Exec(`INSERT OR IGNORE INTO summaries(change_id, text, model, created_at) VALUES(?,?,?,?)`, t.ChangeID, text, model, now)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 && text != "" {
		bctx := context.Background()
		switch t.Kind {
		case KindTool:
			_, err = ingest.AddChunk(bctx, tx, t.RecordID, t.SessionID, "summary", "", t.TS, 1000+t.Idx, text)
		case KindAsset:
			var rid int64
			if rid, err = ingest.AssetRecord(bctx, tx, t.AssetID, t.SessionID); err == nil {
				_, err = ingest.AddChunk(bctx, tx, rid, t.SessionID, "summary", "", t.TS, 1000, text)
			}
		case KindChange:
			_, err = ingest.AddChunk(bctx, tx, t.RecordID, t.SessionID, "filechange", "", t.TS, 1000, ingest.ChangeChunkText(t.FilePath, t.Tool, t.TS, text))
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
