// Command labeled_eval measures routed (mode=auto) search quality and latency
// against a labeled-query benchmark: MRR, Recall@5 and p95 latency.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wamra/session-index/internal/embed"
	"github.com/wamra/session-index/internal/search"
	"github.com/wamra/session-index/internal/store"
)

type query struct {
	Q   string `json:"q"`
	Cat string `json:"cat"`
	Sid string `json:"sid"`
}

type agg struct {
	n         int
	rr, rec5  float64
	latencies []time.Duration
}

func (a *agg) add(rank int, d time.Duration) {
	a.n++
	if rank > 0 {
		a.rr += 1 / float64(rank)
		if rank <= 5 {
			a.rec5++
		}
	}
	a.latencies = append(a.latencies, d)
}

func (a *agg) p95() time.Duration {
	s := append([]time.Duration(nil), a.latencies...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s) == 0 {
		return 0
	}
	i := int(float64(len(s))*0.95+0.999999) - 1
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func (a *agg) print(name string) {
	fmt.Printf("%-14s n=%-4d MRR=%.3f R@5=%.3f p95=%.1fms\n", name, a.n,
		a.rr/float64(a.n), a.rec5/float64(a.n), float64(a.p95().Microseconds())/1000)
}

func main() {
	qpath := flag.String("queries", os.Getenv("SESSION_INDEX_LABELED_QUERIES"), "labeled queries json (default: $SESSION_INDEX_LABELED_QUERIES)")
	home, _ := os.UserHomeDir()
	dbp := flag.String("db", filepath.Join(home, ".claude", "cache", "session-archive", "archive.sqlite"), "archive db")
	url := flag.String("ollama", "http://localhost:11434", "ollama url")
	model := flag.String("model", "embeddinggemma", "embedding model")
	before := flag.String("before", "", "drop hits whose session started on/after this RFC3339 timestamp (string-compared with sessions.started)")
	noTool := flag.Bool("no-tool-fts", false, "skip the tool_fts leg")
	flag.Parse()
	if *qpath == "" {
		fmt.Fprintln(os.Stderr, "labeled_eval: --queries or SESSION_INDEX_LABELED_QUERIES is required")
		os.Exit(2)
	}

	raw, err := os.ReadFile(*qpath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var qs []query
	if err := json.Unmarshal(raw, &qs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st, err := store.Open(*dbp)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer st.Close()
	emb := embed.NewClient(*url, *model)
	ctx := context.Background()

	started := map[string]string{}
	if *before != "" {
		rows, err := st.DB().Query("select session_id, coalesce(started,'') from sessions")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for rows.Next() {
			var id, s string
			rows.Scan(&id, &s)
			started[id] = s
		}
		rows.Close()
	}
	if *before != "" {
		var ns, nc int
		st.DB().QueryRow("select count(*) from sessions where started >= ?", *before).Scan(&ns)
		st.DB().QueryRow("select count(*) from chunks where session_id in (select session_id from sessions where started >= ?)", *before).Scan(&nc)
		fmt.Printf("excluded: %d sessions, %d chunks (started >= %s)\n", ns, nc, *before)
	}
	k := 10
	if *before != "" {
		k = 20
	}
	all := &agg{}
	byCat := map[string]*agg{}
	routes := map[string]int{}
	var idTop, toolOnly, toolOnlyCorrect int
	for _, q := range qs {
		t0 := time.Now()
		hits, route, err := search.Run(ctx, st, emb, q.Q, search.Opts{Mode: "auto", K: k, NoToolFTS: *noTool})
		d := time.Since(t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query %q: %v\n", q.Q, err)
		}
		routes[route]++
		if *before != "" {
			kept := hits[:0:0]
			for _, h := range hits {
				if s := started[h.SessionID]; s != "" && s >= *before {
					continue
				}
				kept = append(kept, h)
			}
			if len(kept) > 10 {
				kept = kept[:10]
			}
			hits = kept
		}
		rank := 0
		for i, h := range hits {
			if h.SessionID == q.Sid {
				rank = i + 1
				break
			}
		}
		if q.Cat == "identifier" && !*noTool && len(hits) > 0 && route == "fts" {
			idTop++
			ph, _, _ := search.Run(ctx, st, emb, q.Q, search.Opts{Mode: "auto", K: 100, NoToolFTS: true})
			found := false
			for _, h := range ph {
				if h.ChunkID == hits[0].ChunkID {
					found = true
					break
				}
			}
			if !found {
				toolOnly++
				if rank == 1 {
					toolOnlyCorrect++
				}
			}
		}
		all.add(rank, d)
		if byCat[q.Cat] == nil {
			byCat[q.Cat] = &agg{}
		}
		byCat[q.Cat].add(rank, d)
	}
	all.print("OVERALL")
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		byCat[c].print(c)
	}
	fmt.Println("routes:", routes)
	if !*noTool {
		fmt.Printf("identifier fts-routed queries with hits: %d; top-1 chunk not in no-tool chunk_fts results (tool_fts-only): %d (of which top-1 correct: %d)\n", idTop, toolOnly, toolOnlyCorrect)
	}
}
