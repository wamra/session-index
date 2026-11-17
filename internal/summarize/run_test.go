package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/ingest"
	"github.com/wamra/session-index/internal/store"
)

const sid = "11111111-1111-1111-1111-111111111111"

func fixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	big := strings.Repeat("compile error line\n", 200) // > 2048
	small := strings.Repeat("x", 100)
	a := fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"%s","timestamp":"2026-03-04T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go build"}},{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/p/main.go","old_string":"foo()","new_string":"bar()"}},{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"ls"}}]}}`, sid) + "\n"
	u := fmt.Sprintf(`{"type":"user","uuid":"u2","sessionId":"%s","timestamp":"2026-03-04T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":%q},{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":"no match"},{"type":"tool_result","tool_use_id":"t3","content":%q}]}}`, sid, big, small) + "\n"
	p := filepath.Join(root, "proj", sid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(a+u), 0o644)
	tr := filepath.Join(root, "proj", sid, "tool-results", "o.txt")
	os.MkdirAll(filepath.Dir(tr), 0o755)
	os.WriteFile(tr, []byte("big file body"), 0o644)
	if _, err := ingest.Sweep(context.Background(), st, root, "p"); err != nil {
		t.Fatal(err)
	}
	return st, root
}

func fake(t *testing.T, status int, calls *int32) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		var req struct {
			Model    string              `json:"model"`
			Messages []map[string]string `json:"messages"`
			Think    bool                `json:"think"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		fmt.Fprintf(w, `{"message":{"content":"summary of %s item"}}`, strings.Fields(req.Messages[1]["content"])[0])
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, DefaultModel)
}

func count(t *testing.T, st *store.Store, q string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestQueueSelectionOnlyLargeOutputs(t *testing.T) {
	st, _ := fixture(t)
	ts, err := Pending(context.Background(), st.DB(), KindTool, 100)
	if err != nil || len(ts) != 1 || ts[0].Idx != 0 {
		t.Fatalf("%+v %v", ts, err)
	}
	if n := count(t, st, `SELECT count(*) FROM tool_outputs`); n != 3 {
		t.Fatalf("tool_outputs %d", n)
	}
	c, _ := Count(context.Background(), st.DB())
	if c.PendingTool != 1 || c.PendingChange != 1 || c.PendingAsset != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestRunStoresSummariesAndChunks(t *testing.T) {
	st, _ := fixture(t)
	var calls int32
	cl := fake(t, 200, &calls)
	stats, err := Run(context.Background(), st, cl, Opts{Parallel: 3})
	if err != nil || stats.Done != 3 || stats.Failed != 0 || calls != 3 {
		t.Fatalf("%+v %v calls=%d", stats, err, calls)
	}
	if n := count(t, st, `SELECT count(*) FROM summaries WHERE record_id IS NOT NULL AND idx=0`); n != 1 {
		t.Fatalf("tool summary rows %d", n)
	}
	// tool summary chunk hangs off the record, kind summary
	if n := count(t, st, `SELECT count(*) FROM chunks c JOIN summaries s ON s.record_id=c.record_id AND s.idx=0 WHERE c.kind='summary' AND c.session_id='`+sid+`' AND c.embedded_at IS NULL`); n != 1 {
		t.Fatalf("summary chunk %d", n)
	}
	// change summary: chunk text format and FK columns
	var txt string
	if err := st.DB().QueryRow(`SELECT text FROM chunks WHERE kind='filechange'`).Scan(&txt); err != nil || !strings.HasPrefix(txt, "/p/main.go (Edit 2026-03-04): summary of") {
		t.Fatalf("%q %v", txt, err)
	}
	var recID, resID, ok int64
	if err := st.DB().QueryRow(`SELECT fc.record_id, fc.result_record_id, fc.result_ok FROM file_changes fc JOIN summaries s ON s.change_id=fc.id`).Scan(&recID, &resID, &ok); err != nil || recID == 0 || resID == 0 || resID == recID || ok != 0 {
		t.Fatalf("%d %d %d %v", recID, resID, ok, err)
	}
	var cr int64
	st.DB().QueryRow(`SELECT record_id FROM chunks WHERE kind='filechange'`).Scan(&cr)
	if cr != recID {
		t.Fatalf("chunk record %d != change record %d", cr, recID)
	}
	// asset summary chunk
	if n := count(t, st, `SELECT count(*) FROM summaries s JOIN assets a ON a.id=s.asset_id`); n != 1 {
		t.Fatalf("asset summaries %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("foreign_key_check rows %d", n)
	}
	// nothing pending afterwards; second run makes no calls
	stats, err = Run(context.Background(), st, cl, Opts{})
	if err != nil || stats.Done != 0 || calls != 3 {
		t.Fatalf("%+v %v %d", stats, err, calls)
	}
	// the FTS index sees the summary
	if n := count(t, st, `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH '"summary of"'`); n != 3 {
		t.Fatalf("chunk_fts %d", n)
	}
}

func TestFailedMarkerNotRetriedAndMax(t *testing.T) {
	st, _ := fixture(t)
	var calls int32
	cl := fake(t, 400, &calls)
	stats, err := Run(context.Background(), st, cl, Opts{Max: 2})
	if err != nil || stats.Failed != 2 || stats.Done != 0 {
		t.Fatalf("%+v %v", stats, err)
	}
	stats, err = Run(context.Background(), st, cl, Opts{})
	if err != nil || stats.Failed != 1 || calls != 3 { // only the third item is still pending
		t.Fatalf("%+v %v calls=%d", stats, err, calls)
	}
	if n := count(t, st, `SELECT count(*) FROM summaries WHERE text='' AND model LIKE 'failed:%'`); n != 3 {
		t.Fatalf("failed markers %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE kind IN ('summary','filechange')`); n != 0 {
		t.Fatalf("failed rows must not create chunks: %d", n)
	}
	stats, _ = Run(context.Background(), st, cl, Opts{})
	if stats.Failed != 0 || calls != 3 {
		t.Fatalf("retried: %+v calls=%d", stats, calls)
	}
}

func TestOllamaDownStopsWithoutParking(t *testing.T) {
	st, _ := fixture(t)
	var calls int32
	cl := fake(t, 503, &calls)
	stats, err := Run(context.Background(), st, cl, Opts{})
	if !errors.Is(err, embed.ErrOllamaDown) || stats.Done+stats.Failed != 0 {
		t.Fatalf("%+v %v", stats, err)
	}
	if n := count(t, st, `SELECT count(*) FROM summaries`); n != 0 {
		t.Fatalf("parked %d", n)
	}
}

func TestLockExclusion(t *testing.T) {
	st, _ := fixture(t)
	l1, err := TryLock(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(st); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v", err)
	}
	l1.Release()
	l2, err := TryLock(st)
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

func TestSummariesSurviveRederiveAndRepair(t *testing.T) {
	st, _ := fixture(t)
	var calls int32
	if _, err := Run(context.Background(), st, fake(t, 200, &calls), Opts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Rederive(context.Background(), st, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Repair(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if n := count(t, st, `SELECT count(*) FROM summaries`); n != 3 {
		t.Fatalf("summaries %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM chunks WHERE kind IN ('summary','filechange')`); n != 3 {
		t.Fatalf("summary chunks %d", n)
	}
	if n := count(t, st, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
		t.Fatalf("fk rows %d", n)
	}
}

// manyToolOutputs returns a store with n large tool outputs awaiting summaries.
func manyToolOutputs(t *testing.T, n int) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	big := strings.Repeat("compile error line\n", 200)
	var uses, results []string
	for i := 0; i < n; i++ {
		uses = append(uses, fmt.Sprintf(`{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"cmd%d"}}`, i, i))
		results = append(results, fmt.Sprintf(`{"type":"tool_result","tool_use_id":"t%d","content":%q}`, i, big))
	}
	a := fmt.Sprintf(`{"type":"assistant","uuid":"a1","sessionId":"%s","timestamp":"2026-03-04T00:00:01Z","message":{"role":"assistant","content":[%s]}}`, sid, strings.Join(uses, ",")) + "\n"
	u := fmt.Sprintf(`{"type":"user","uuid":"u2","sessionId":"%s","timestamp":"2026-03-04T00:00:02Z","message":{"role":"user","content":[%s]}}`, sid, strings.Join(results, ",")) + "\n"
	p := filepath.Join(root, "proj", sid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(a+u), 0o644)
	if _, err := ingest.Sweep(context.Background(), st, root, "p"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCircuitBreakerStopsAfterFiveIdenticalItemFailures(t *testing.T) {
	st := manyToolOutputs(t, 12)
	var calls int32
	c := fake(t, 400, &calls)
	stats, err := Run(context.Background(), st, c, Opts{Kinds: []string{KindTool}})
	var be *BreakerError
	if !errors.As(err, &be) || be.Reason != "http 400" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "stopping: 5 consecutive identical item failures (http 400)") || !strings.Contains(err.Error(), "likely a request/config problem") {
		t.Fatalf("message %q", err)
	}
	if stats.Failed != BreakerThreshold || count(t, st, `SELECT count(*) FROM summaries WHERE model='failed:http 400'`) != BreakerThreshold {
		t.Fatalf("stats %+v parked %d", stats, count(t, st, `SELECT count(*) FROM summaries WHERE model LIKE 'failed:%'`))
	}
	if got := atomic.LoadInt32(&calls); got > BreakerThreshold+1 { // at most one request is already in flight when it trips
		t.Fatalf("%d requests for a run that should stop after %d (+1 in flight)", got, BreakerThreshold)
	}
}

func TestTimeoutsAreNeverParkedAndStopTheRunAfterFive(t *testing.T) {
	st := manyToolOutputs(t, 6)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, DefaultModel)
	c.HTTP.Timeout = 40 * time.Millisecond
	stats, err := Run(context.Background(), st, c, Opts{Kinds: []string{KindTool}})
	if !errors.Is(err, ErrTimeouts) || !strings.Contains(err.Error(), "provider timing out; stopping, items stay pending") {
		t.Fatalf("err = %v", err)
	}
	if stats.Done != 0 || stats.Failed != 0 || count(t, st, `SELECT count(*) FROM summaries`) != 0 {
		t.Fatalf("stats %+v, parked %d: timeouts must park nothing", stats, count(t, st, `SELECT count(*) FROM summaries`))
	}
	if got := atomic.LoadInt32(&calls); got < TimeoutLimit || got > TimeoutLimit+1 {
		t.Fatalf("%d requests", got)
	}
	// every item is still pending: a healthy provider summarizes all 6
	var n int32
	stats, err = Run(context.Background(), st, fake(t, 200, &n), Opts{Kinds: []string{KindTool}})
	if err != nil || stats.Done != 6 {
		t.Fatalf("after recovery: %+v %v", stats, err)
	}
}

func TestRetryFailedReenqueuesParkedItems(t *testing.T) {
	st := manyToolOutputs(t, 4)
	var n int32
	stats, err := Run(context.Background(), st, fake(t, 400, &n), Opts{Kinds: []string{KindTool}})
	if err != nil || stats.Failed != 4 {
		t.Fatalf("%+v %v", stats, err)
	}
	if again, _ := Run(context.Background(), st, fake(t, 200, &n), Opts{Kinds: []string{KindTool}}); again.Done != 0 {
		t.Fatal("parked items were retried without --retry-failed")
	}
	if k, err := RetryFailed(context.Background(), st.DB(), "http 500"); err != nil || k != 0 {
		t.Fatalf("other reason removed %d %v", k, err)
	}
	if k, err := RetryFailed(context.Background(), st.DB(), "http 400"); err != nil || k != 4 {
		t.Fatalf("removed %d %v", k, err)
	}
	stats, err = Run(context.Background(), st, fake(t, 200, &n), Opts{Kinds: []string{KindTool}})
	if err != nil || stats.Done != 4 {
		t.Fatalf("%+v %v", stats, err)
	}
	if k, _ := RetryFailed(context.Background(), st.DB(), ""); k != 0 {
		t.Fatalf("removed %d successes", k)
	}
}

func TestDailyCapStopsRunAndPersistsAcrossRuns(t *testing.T) {
	st, _ := fixture(t)
	var calls int32
	cl := fake(t, 200, &calls)
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	o := Opts{MaxPerDay: 2, Now: func() time.Time { return now }}
	stats, err := Run(context.Background(), st, cl, o)
	var capErr *ErrDailyCap
	if !errors.As(err, &capErr) || capErr.Used != 2 || capErr.Cap != 2 || stats.Done != 2 || calls != 2 {
		t.Fatalf("%+v err=%v calls=%d", stats, err, calls)
	}
	if used, _ := store.UsageToday(context.Background(), st.DB(), store.UsageSummarize, now); used != 2 {
		t.Fatalf("usage %d", used)
	}
	// the third item stays pending, not parked
	if c, _ := Count(context.Background(), st.DB()); c.Done != 2 || c.Failed != 0 || c.PendingTool+c.PendingChange+c.PendingAsset != 1 {
		t.Fatalf("%+v", c)
	}
	// same day, new run: no calls at all
	if _, err := Run(context.Background(), st, cl, o); !errors.As(err, &capErr) || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	// next UTC day: the counter resets and the last item is done; finishing
	// exactly at the cap with nothing pending is not a cap hit
	now = now.Add(24 * time.Hour)
	stats, err = Run(context.Background(), st, cl, o)
	if err != nil || stats.Done != 1 || calls != 3 {
		t.Fatalf("%+v err=%v calls=%d", stats, err, calls)
	}
	o.MaxPerDay = 1
	if stats, err = Run(context.Background(), st, cl, o); err != nil || stats.Done != 0 {
		t.Fatalf("nothing pending, cap used up: %+v %v", stats, err)
	}
}

func TestMaxInputCharsAndKinds(t *testing.T) {
	st, _ := fixture(t)
	ts, err := PendingMax(context.Background(), st.DB(), KindTool, 10, 1000)
	if err != nil || len(ts) != 1 {
		t.Fatalf("%v %v", ts, err)
	}
	if n := len([]rune(ts[0].input)); n != 1000+len("\n[...]\n") {
		t.Fatalf("clipped input has %d runes", n)
	}
	if ts[0].input[:10] != "compile er" || !strings.HasSuffix(ts[0].input, "line\n") {
		t.Fatalf("head/tail lost: %q", ts[0].input[:20])
	}
	if got := ClipTo(strings.Repeat("a", 9000), 0); len(got) != 8000+len("\n[...]\n") {
		t.Fatalf("default clip %d", len(got))
	}
	if Clip(strings.Repeat("a", 8000)) != strings.Repeat("a", 8000) {
		t.Fatal("text at the limit must be kept whole")
	}
	var calls int32
	ks, err := KindsFromConfig([]string{"asset", "file_change"})
	if err != nil || len(ks) != 2 {
		t.Fatal(ks, err)
	}
	stats, err := Run(context.Background(), st, fake(t, 200, &calls), Opts{Kinds: ks})
	if err != nil || stats.Done != 2 || calls != 2 {
		t.Fatalf("%+v %v %d", stats, err, calls)
	}
	if c, _ := Count(context.Background(), st.DB()); c.PendingTool != 1 {
		t.Fatalf("tool outputs must stay pending: %+v", c)
	}
	if _, err := KindsFromConfig([]string{"bogus"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestUsageCountsDispatchedItemsEvenWhenProviderIsDown(t *testing.T) {
	st, _ := fixture(t)
	var calls int32
	now := time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
	_, err := Run(context.Background(), st, fake(t, 503, &calls), Opts{MaxPerDay: 100, Now: func() time.Time { return now }})
	if !Down(err) {
		t.Fatalf("err %v", err)
	}
	used, _ := store.UsageToday(context.Background(), st.DB(), store.UsageSummarize, now)
	if used != 3 || calls < 1 {
		t.Fatalf("usage %d calls %d: the dispatched batch must count although nothing succeeded", used, calls)
	}
	if c, _ := Count(context.Background(), st.DB()); c.Done != 0 || c.Failed != 0 {
		t.Fatalf("down items must stay pending: %+v", c)
	}
}
