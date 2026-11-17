# Benchmarks (2026-11-13, Apple M1 Ultra, 128 GB RAM, macOS)

Archive: 3 profiles in a cloud-synced folder (`desktop`, `laptop`, `default`), 172,801 records, 30,100 chunks embedded with Ollama `embeddinggemma` (768d), 302 sessions, ~790 MB `archive.sqlite` under `~/.claude/cache/session-archive/`.

## Retrieval quality — 558 labeled queries (bench/labeled_eval, mode auto, K=10)

The labeled query set is derived from private sessions and is not included in the repository; the harness is.

| Category | n | MRR | Recall@5 | p95 (in-process) |
|---|---|---|---|---|
| overall | 558 | 0.672 | 0.799 | 131 ms |
| identifier | 80 | 0.839 | 0.887 | 36 ms |
| multichunk | 80 | 0.843 | 0.963 | 130 ms |
| ambiguous | 80 | 0.773 | 0.912 | 129 ms |
| paraphrase | 318 | 0.561 | 0.708 | 132 ms |

Routes: 79 fts, 479 vector. For reference, the single-profile archive (21k chunks) scored MRR 0.692 / R@5 0.826 / p95 98 ms; adding the other two profiles doubled the corpus and introduced distractor sessions, which is the expected trade for cross-machine search. The labeled set itself is single-profile (`desktop`), so these numbers are a lower bound for a user who knows which profile to filter on.

## Search latency — end-to-end CLI (`session-index search … --json`), includes ~30 ms process start

| Query mix | p50 | p95 |
|---|---|---|
| auto, prose (vector route) | 196 ms | 201 ms |
| auto, identifiers (fts route) | 70 ms | 82 ms |
| hybrid, prose | 383 ms | 470 ms |
| forced fts, prose (trigram OR of up to 24 terms) | 252 ms | 331 ms |
| auto, prose, `--profile laptop` (8,288 chunks, KNN path) | 236 ms | 250 ms |
| auto, prose, `--profile default` (250 chunks, exact scan) | 182 ms | 210 ms |
| auto, prose, plus one peer archive (full duplicate via `--db`) | 270 ms | 274 ms |

Query embedding (Ollama, warm) is 35–60 ms of every vector-route number. KNN over 30k×768 in the WASM SQLite build is ~40 ms. Exact profile scans above ~2,000 chunks were measured at 2.0 s (vec0 rowid fetches), hence the 2,000-chunk threshold.

## Archive operations

| Operation | Measured |
|---|---|
| Hook `ingest --stdin-hook` on a live 5 MB transcript | 64–67 ms (no new lines), 401 ms (new lines) |
| `sweep --all-profiles` with nothing new (1,446 files) | 0.7 s |
| `sweep --all-profiles --embed` first ingest of `laptop` + `default` (253 files, 42.7k lines, 8.5k chunks) | 11 min 37 s |
| Full backfill of `desktop` (1,177 files, 127k lines, 21k chunks) | 8 min 2 s |
| `stats` cold start (wazero cache warm) | 214 ms |
| `doctor` (quick_check + FTS integrity-check) | ~2 s |
| `doctor --full` (adds PRAGMA integrity_check over 790 MB) | 23 s |
| `repair` (orphan sweep + FTS rebuild) | 29.5 s |
| `restore` spot checks | 90/90 (`desktop`) and 37/37 (`laptop`) files SHA-256 identical |

## Scale notes

vec0 KNN is a linear scan: ~22 ms at 15k vectors, ~40 ms at 30k; expect ~130 ms at 100k. The exact profile scan is linear in profile size and should stay under 2,000 chunks. Cross-file search adds one KNN per peer archive.

## Acceptance numbers (2026-11-08, same machine, first backfill)

- Backfill (`backfill --embed`): 1177 files, 127,417 lines/records, 21,299 chunks, all 21,299 embedded, 0 pending, 0 failed; wall time 8m01.9s (target < 10 min).
- Archive after backfill: 215 sessions (190 main, 25 subagent), 87,373 messages, 56,484 tool_fts rows, 745.5 MiB.
- Restore check: 5 random main sessions restored with `restore --to`; all 24 restored files were byte-identical (SHA-256) to the originals.
- Labeled eval (558 queries, mode auto, K=10, `go run ./bench/labeled_eval`): overall MRR 0.682, Recall@5 0.823, p95 99.3 ms (targets: MRR >= 0.70, p95 <= 100 ms; MRR is below target, not tuned). Per category: identifier MRR 0.813, multichunk 0.843, ambiguous 0.779, paraphrase 0.584.
- Hook latency: `ingest --stdin-hook --embed` took 0.033-0.035 s on an already archived transcript and 0.046-0.149 s on the live session transcript (target < 1 s).
