# session-index reference

Technical reference. For what the tool is and how to install it, see the [README](../README.md).

## Manual install

```
go build -o ~/.local/bin/session-index ./cmd/session-index
session-index doctor            # DB path, quick_check, embed and summarize providers (health, key env set/unset), hooks
session-index backfill --embed  # archive ~/.claude/projects, then embed until --embed-deadline (default 20m)
session-index install-hooks --dry-run   # print the resulting settings.json
session-index install-hooks     # writes only ~/.claude/settings.json
```

## Platforms

Only macOS is in daily use. On Linux and Windows the test suite passes in CI (GitHub-hosted amd64 runners), but neither has been used with a real Claude Code profile, and `install.ps1` has not been run. On Windows, file locking uses LockFileEx, detached embedding uses DETACHED_PROCESS, and restore uses hard links. The archive uses a WAL there as elsewhere, through a patched driver (see the design note). The cloud-sync warning recognises only the macOS CloudStorage path.

## Catch-up

Catch-up: run `session-index sweep --embed` manually (hooks cover live sessions). Run it from an interactive Terminal that already has consent to read `~/.claude`.

## Optional: launchd sweep

`install-hooks --launchd [--interval-min 10]` additionally writes `~/Library/LaunchAgents/session-index.sweep.plist` and prints the bootstrap command (`launchctl bootstrap gui/$(id -u) <plist>`; if it reports "already bootstrapped", run `launchctl kickstart -k gui/$(id -u)/session-index.sweep`). It is off by default.

TCC caveat: a background process reading `~/.claude` when it resolves into `~/Library/CloudStorage` (OneDrive) blocks on a macOS TCC consent prompt that nobody sees, so the sweep hangs. If you enable launchd, grant Full Disk Access to the `session-index` binary first.

## Database location

WARNING: when `~/.claude` resolves into a cloud-synced tree (for example `~/Library/CloudStorage/...`), the archive database lives in that synced tree. A live WAL database under a sync client churns (the whole file re-uploads on change) and risks conflict copies or torn uploads; `flock` on File Provider volumes is not guaranteed. `session-index doctor` prints a warning when the resolved database path is under `/Users/*/Library/CloudStorage/` and runs `PRAGMA quick_check` (a failure exits 1). Set `SESSION_INDEX_DB` or `paths.db` in `config.json` to a path outside the synced tree to relocate it.

Before lowering Claude Code's `cleanupPeriodDays`, take a copy of the database outside the synced tree, because the archive then holds the only copy of old transcripts:

```
sqlite3 archive.sqlite ".backup /path/outside/sync/snapshot.sqlite"
# or: sqlite3 archive.sqlite "VACUUM INTO '/path/outside/sync/snapshot.sqlite'"
```

These commands work on a plaintext archive only; for an encrypted one see [Encryption](#encryption).

Vector search is a brute-force scan (22 ms p50 at about 15k vectors, linear in corpus size) and the 100 ms p95 target passes only barely at the current size; expect it to grow with the corpus.

## What gets written where

- Database: `~/.claude/cache/session-archive/archive.sqlite`.
- Only after `encrypt`: `archive.key` next to the database, the unlocked key in `~/.config/session-index/keys/`, and the kept copy `archive.sqlite.plaintext-<UTC timestamp>`.
- `~/.claude/settings.json`: three async hook entries (`Stop`, `SubagentStop`, `SessionEnd`) running `<path of the session-index binary> ingest --stdin-hook --embed` (timeout 10 s). Existing hooks are kept. A one-time backup is written to `~/.claude/settings.json.bak-session-index`.
- Only with `install-hooks --launchd`: `~/Library/LaunchAgents/session-index.sweep.plist`, which runs `session-index sweep --embed` at load and every 600 s (log at `~/.claude/cache/session-archive/sweep.log`).
- `~/.claude/projects` is only read, never modified (`restore --in-place` is the only command that writes there, and only when asked).

## Rollback

1. Remove the three hook entries whose command contains `session-index` from `~/.claude/settings.json` (Stop, SubagentStop, SessionEnd). Restoring `~/.claude/settings.json.bak-session-index` also works but reverts every settings change made since the first install.
2. Only if you enabled launchd: `launchctl bootout gui/$(id -u)/session-index.sweep`, then `rm ~/Library/LaunchAgents/session-index.sweep.plist`.
3. Optionally delete the database and the binary.

## Configuration

Embeddings and summaries go through a provider. With no configuration the behavior is the historical one: Ollama at `http://localhost:11434`, `embeddinggemma` (768 dims) for embeddings, `gemma4:e4b` for summaries. Supported providers: `ollama`, `openai` (any OpenAI-compatible server: LM Studio, vLLM, OpenAI, Voyage), `anthropic` (summaries only) and `bedrock` (embeddings and summaries); see Hosted providers below.

- `session-index config` prints the effective configuration (defaults, file and environment applied). API keys are never read into it: only the NAME of the key variable is shown, with `set`/`unset`.
- `session-index config init [--force]` writes a default `~/.config/session-index/config.json` (JSON with a top-level `_comment`). Override the path with `SESSION_INDEX_CONFIG`. A missing file means defaults; unknown keys are an error.
- Everything is configured in this one file; the only thing outside it is the file's own location (`SESSION_INDEX_CONFIG`). Environment variables stay as overrides and win over the file, and so do flags.
- Precedence, lowest first: defaults, config file, `SESSION_INDEX_OLLAMA` (alias for both base URLs while the provider is `ollama`), then `SESSION_INDEX_EMBED_{PROVIDER,MODEL,BASE_URL}` and `SESSION_INDEX_SUMMARIZE_{PROVIDER,MODEL,BASE_URL}`.

```json
{
  "paths": {"db": "~/.claude/cache/session-archive/archive.sqlite", "claude_dir": "~/.claude", "root": "~/.claude/projects",
            "key_dir": "~/.config/session-index/keys", "peers": []},
  "search": {"default_k": 10, "rrf_k": 60, "embed_timeout_ms": 3000, "max_terms": 24},
  "ingest": {"backoff_seconds": 30},
  "embed": {"provider": "ollama|openai|bedrock", "base_url": "...", "model": "...", "dims": 768,
            "doc_prefix": "...", "query_prefix": "...", "api_key_env": "NAME_OF_ENV_VAR"},
  "summarize": {"provider": "ollama|openai|anthropic|bedrock", "base_url": "...", "model": "...", "api_key_env": "...",
                "max_tokens": 120, "temperature": 0.2, "enabled": true, "max_items_per_run": 0}
}
```

`paths` (every key optional, `~` is the home directory; the values shown are the defaults): `db` is the archive file (`SESSION_INDEX_DB`); `claude_dir` is the Claude Code profile folder, from which the profile name, the default `db` and `root`, and `settings.json` derive; `root` is the transcripts folder (`SESSION_INDEX_ROOT`, `--root`); the asset folders `file-history`, `plans`, `paste-cache`, `tasks` and `uploads` are read from the parent of `root`; `key_dir` holds the unlocked keys (`SESSION_INDEX_KEY_DIR`); `peers` lists peer archives to search and replaces discovery (`SESSION_INDEX_PEERS`, colon-separated). Only the `paths` section is read by the commands that archive, and it is read without checking the other sections, so a mistake in `embed` never moves the archive. A config file that is not valid JSON stops `embed`, `summarize`, `search` and `doctor`; archiving carries on in the default location only when an archive already exists there, and says so. `search` and `ingest` hold the knobs that change no stored data. The sizes that shape stored rows (chunk length, tool-output cap, excerpt length, asset text cap) are fixed in the code, because changing them would mix old and new rows until a rebuild. A binary older than a key you add rejects the file (unknown keys are an error), so update every machine before the key reaches a file they share.

`api_key_env` names an environment variable; `Authorization: Bearer <value>` is sent only when that variable is set and non-empty. `summarize.enabled=false` turns summaries off; `max_items_per_run` > 0 caps one `summarize` run. Prefixes and `dims` default per known model (`embeddinggemma`: `title: none | text: ` / `task: search result | query: `, 768; `nomic-embed-text`: `search_document: ` / `search_query: `, 768; `all-minilm`: none, 384; `bge-*`: query prefix `Represent this sentence for searching relevant passages: `; `text-embedding-3-small`: none, 1536); config values override them. For any other model, leave `dims` out and it is detected from the first response.

Ollama (the default, written out):

```json
{"embed": {"provider": "ollama", "model": "embeddinggemma"},
 "summarize": {"provider": "ollama", "model": "gemma4:e4b"}}
```

LM Studio (OpenAI-compatible; replace `<port>` with the port its server listens on):

```json
{"embed": {"provider": "openai", "base_url": "http://localhost:<port>", "model": "text-embedding-nomic-embed-text-v1.5"},
 "summarize": {"provider": "openai", "base_url": "http://localhost:<port>", "model": "<a chat model loaded in LM Studio>"}}
```

OpenAI:

```json
{"embed": {"provider": "openai", "model": "text-embedding-3-small", "api_key_env": "OPENAI_API_KEY"},
 "summarize": {"provider": "openai", "model": "<a chat model>", "api_key_env": "OPENAI_API_KEY"}}
```

(`base_url` defaults to `https://api.openai.com`; a trailing `/v1` is accepted.)

Switching embedders. The vector size is fixed when the archive is created (`chunk_vec` is created with the configured or detected dims; `meta.vec_dims` stores it) and `meta.embedder_fingerprint` is `<provider>|<model>|<dims>|<doc_prefix>|v1`. An existing archive keeps its size: a configuration that wants another size fails with an error naming both values. To switch (new model, new provider or new dims): change the configuration, run `session-index rebuild-derived` (it recreates `chunk_vec` at the new size and stores the new fingerprint inside its transaction), then `session-index embed` to re-embed every chunk. Archives created before providers existed carry the fingerprint `embeddinggemma|title: none | text: |v1`; it is accepted as `ollama|embeddinggemma|768|title: none | text: |v1` and rewritten once, on the first writer open. Summaries record `<provider>:<model>` in `summaries.model` (older rows keep the bare model name). Failure classes: unreachable, 5xx, 404, 408 and 429 leave the work pending; 401 and 403 (`authentication failed`, naming the key variable and whether it is set) stop `embed`, `summarize` and `search` with exit 1 and park nothing; 400, 413 and other 4xx park only the offending item, and `summarize` stops after 5 consecutive identical item failures (a request or configuration problem); a request timeout never parks an item (it stays pending) and 5 consecutive timeouts stop the run; `summarize --retry-failed [REASON]` deletes parked `failed:*` rows (all, or only `failed:REASON`) so they are retried; wrong dimensions or malformed responses stop the drain. Archiving commands (ingest, sweep, backfill, hooks, restore, files, show, stats) never fail on an invalid config or a dims mismatch for an existing archive (a NEW archive is refused under an invalid config); `embed`, `summarize`, `search` and `doctor` check them (`search` in auto/hybrid mode falls back to FTS, route `fts-fallback`, on a dims mismatch; `--mode vector` fails) (`doctor` reports a bad config as a FAILED line). `config init` leaves `doc_prefix`/`query_prefix` out for known models so changing the model picks up the right defaults.

## Hosted providers

Everything below is opt-in: with no configuration nothing leaves the machine and local providers have no caps.

Keys come from environment variables only. `api_key_env` names the variable (the config file never holds a key); for `anthropic` it defaults to `ANTHROPIC_API_KEY`. Bedrock takes no key: it uses the default AWS credential chain (environment variables, shared config and credentials files with an optional `profile`, SSO) in an optional `region`; `session-index doctor` shows the key state or the Bedrock region and profile and runs a health probe (Anthropic: `GET /v1/models`; Bedrock: a 1-text embed or a 1-token Converse call).

What leaves the machine: for summaries, the text of the tool output, tool-result file or file change being summarized (clipped to `summarize.max_input_chars`) and a fixed instruction; for embeddings, the text of every chunk as indexed (messages, tool text, summaries; each capped to 2,000 characters), and the text of each search query in `search` (vector and hybrid modes). Nothing else is sent. How Anthropic, AWS and Voyage use or retain that data is governed by your own agreements with them; this tool makes no claim about it.

Anthropic summaries (Messages API, `POST {base_url}/v1/messages` with `x-api-key` and `anthropic-version: 2023-06-01`; `base_url` defaults to `https://api.anthropic.com`; the model defaults to `claude-haiku-4-5`). Anthropic has no embeddings endpoint, so `embed.provider: "anthropic"` is a configuration error:

```json
{"summarize": {"provider": "anthropic", "model": "claude-haiku-4-5", "api_key_env": "ANTHROPIC_API_KEY"}}
```

Embeddings with Voyage (OpenAI-compatible `/v1/embeddings`; `voyage-3` is 1024 dims, `voyage-3-lite` 512), or another hosted OpenAI-compatible endpoint:

```json
{"embed": {"provider": "openai", "base_url": "https://api.voyageai.com", "model": "voyage-3", "api_key_env": "VOYAGE_API_KEY"}}
```

AWS Bedrock (`github.com/aws/aws-sdk-go-v2`; the only AWS dependency, isolated in `internal/provider/bedrock.go`). Embeddings through InvokeModel: Titan Text Embeddings V2 `amazon.titan-embed-text-v2:0` (one text per request; `dims` 256, 512 or 1024, default 1024; `normalize: true`) or Cohere `cohere.embed-english-v3` / `cohere.embed-multilingual-v3` (1024 dims, up to 96 texts per request, `truncate: END`; the document and query "prefixes" are the Cohere `input_type` values `search_document` and `search_query` and are not prepended to the text). Summaries through the Converse API with any Claude model id enabled in your account (`anthropic.claude-*` or `us.anthropic.claude-*`; no default is assumed, set `model`):

```json
{"embed": {"provider": "bedrock", "model": "amazon.titan-embed-text-v2:0", "dims": 1024, "region": "us-east-1", "profile": "default"},
 "summarize": {"provider": "bedrock", "model": "<a Claude model id>", "region": "us-east-1"}}
```

Fingerprints: `bedrock|<model>|<dims>|<input_type or prefix>|v1` (for example `bedrock|cohere.embed-english-v3|1024|search_document|v1`, `bedrock|amazon.titan-embed-text-v2:0|1024||v1`); Voyage through `openai` keeps the `openai|<model>|<dims>|<doc_prefix>|v1` form. Summaries record `anthropic:<model>` or `bedrock:<model>`.

Error mapping (all hosted drivers): 401/403 and Bedrock `AccessDenied`, `UnrecognizedClient`, `ExpiredToken` or a failed credential chain are `authentication failed` (stops the run, parks nothing); 429 and Bedrock `Throttling` are retried with backoff, then count as the provider being down (work stays pending); 5xx, Anthropic 529 `overloaded_error`, Bedrock `ModelNotReady`, `ServiceUnavailable` and `InternalServer` are the provider being down; Anthropic 400 and a Bedrock `ValidationException` about the input size park only that item (`failed:...`); other Bedrock `ValidationException`s (bad model id, dims) stop the run. Retries (the AWS SDK's own retryer is switched off, so only this schedule applies): a throttled request is retried up to 3 times after 1 s, 4 s and 16 s (so at most 4 requests), or after the server's `retry-after` (at most 60 s) when it sends one; this applies to `openai`, `anthropic` and `bedrock` (the local `ollama` driver is not retried), and 4xx item errors are never retried.

Cost guards (they apply to every provider; they default to off for local providers):

- `summarize.max_items_per_day` and `embed.max_items_per_day`: items sent to the provider per UTC day, counted in the archive (`meta` keys `usage:summarize:YYYY-MM-DD` and `usage:embed:YYYY-MM-DD`) across all runs and hooks. The counters are "items sent": an item is counted when its request is dispatched (own short transaction, before the result is processed), including items that then time out, hit a provider that is down or are throttled, and items interrupted by a kill; an item that stays pending is counted again each time it is re-sent, so the count is an upper bound of billed requests. When a cap is reached the run stops, the remaining items stay pending (nothing is parked), the message `daily cap reached: N of M ...` is printed and the exit code is 0; the next UTC day continues. Unset (0) means 2000 for hosted providers (`anthropic`, `bedrock`, and `openai` with a `base_url` that is not localhost, a private network address or `*.local`) and unlimited for local ones; a negative value means unlimited. A first full embedding of a large archive on a hosted provider therefore takes several days unless you raise `embed.max_items_per_day`.
- `summarize.max_input_chars` (default 8000, which is the existing clip: first 6,000 and last 2,000 characters; any other value keeps the first three quarters and the last quarter).
- `summarize.kinds`: a subset of `tool_output`, `file_change`, `asset` (default all); the `summarize --kind tool,change,asset` flag, when given, overrides it.
- `session-index config` shows the effective daily caps; `session-index doctor` shows today's usage against them (`summarize usage today (UTC 2026-11-15): 12 / 2000 (hosted default)`).

## Multi-profile archives

Each Claude profile (`<profiles>/<name>/`, for example `desktop`, `laptop`, `default`) keeps its own complete `cache/session-archive/archive.sqlite` (raw lines plus index) in the synced tree. A machine's own profile name is the basename of the resolved `~/.claude`; the profiles directory is its parent (when `~/.claude` is a plain directory the name is `default` and peer discovery is off).

- Indexer: the machine that runs the sweep. `session-index sweep --all-profiles --embed` walks `<profiles>/*/projects`, labels every file and session with its profile directory name and writes them into that machine's archive; `sweep --root DIR --profile NAME` does one root. Hooks and `backfill` use the own profile (`--profile NAME` overrides). `files.profile` and `sessions.profile` are added by an additive migration; existing rows are backfilled with the own profile name.
- Readers: `session-index search` also opens every other `<profiles>/*/cache/session-archive/archive.sqlite` read-only. Add archives with `--db PATH` (repeatable) or replace discovery with `SESSION_INDEX_PEERS` (colon-separated paths). A peer that cannot be opened (sync in progress, malformed) is reported on stderr and skipped. The query is embedded once; vector hits are merged by distance, FTS hits by reciprocal rank, hybrid by RRF over all legs, and hits are deduplicated by session id (best hit wins). A peer whose `meta.embedder_fingerprint` differs from the own one is searched with FTS only (warning on stderr). `--profile NAME` keeps only that profile's sessions (applied after ranking, with a larger candidate pool). Text output shows the profile after the date; `--json` has a `profile` field.
- Every writer (`ingest`, `sweep`, `backfill`, `embed`, `rebuild-derived`) runs `PRAGMA wal_checkpoint(TRUNCATE)` before closing so the synced `archive.sqlite` is complete without its `-wal` (skipped silently if another writer holds the database).
- A machine that only searches needs only the `session-index` binary; the embedding provider (by default Ollama with `embeddinggemma`) is optional (needed only for vector queries, otherwise use `--mode fts`).
- Skill: `skills/session-search/SKILL.md` is embedded in the binary. `install-hooks --skill` copies it to `<profiles>/<own>/skills/session-search/SKILL.md`; `--skill-all-profiles` copies it into every profile directory (creating `skills/session-search/`).

## Assets, restore fidelity, tool outputs and summaries

- Assets: every regular non-JSONL file under `<profile>/projects/` is archived zstd-compressed with sha256, size, mtime and mode in `assets` (kinds: `tool-result`, `meta`, `workflow`, `memory`, `other`; latest version only, re-archived when size, mtime or content changes; files over 64 MB are skipped whole with a stderr note; `.DS_Store` and `.restore-*` temp files are ignored). An unreadable file (for example a OneDrive file that is not downloaded) is recorded on its `assets` row (`raw` NULL, `failed_at`, `fail_reason`, `next_retry_at`, same 30 s to 1 h backoff as transcript files), listed by `doctor`, and retried by later sweeps; the sweep line counts too large, ignored (`.DS_Store`) and failed files separately. Text assets (`.txt .md .json .js`) are indexed in `asset_fts` (first 256 KB). `<slug>/memory/*.md` is also chunked and embedded as kind `memory` under a synthetic session `memory:<slug>` (title `memory`; each chunk text starts with the file name), so `search` finds it (`[memory]` marker). Each profile's `file-history/<sid>/**` snapshots are archived as kind `file-history`. The other folders Claude Code's retention sweep deletes are archived under the kind of their directory name: `plans/**`, `paste-cache/**`, `tasks/**` and `uploads/**` (kinds `plans`, `paste-cache`, `tasks`, `uploads`). Under `uploads/<sid>/` and `tasks/<id>/` the first component is the `session_id`; every other file (all of `plans/` and `paste-cache/`, and files directly in `uploads/` or `tasks/`) belongs to the pseudo-session `<kind>:<profile>`, like `memory:<slug>`. `.DS_Store`, `.restore-*` and the `.lock` files in `tasks/` are ignored; `.highwatermark` is archived. These kinds are stored and text files are indexed in `asset_fts`. Text files under `plans/` are also chunked and embedded as kind `plan`. Each plan has its own synthetic session `plans:<profile>/<path below plans/>` (title: the plan's first-line heading, or the file name; each chunk text starts with the file name, followed by the heading when the chunk does not contain it), so `search` lists every matching plan as its own hit (`[plan]` marker; `search --kind plan` returns only plans) and `show 'plans:<profile>/<name>'` prints the archived file. The asset row keeps the pseudo-session `plans:<profile>`. A changed plan replaces its chunks, and a plan deleted from disk keeps them. A sweep gives plans that lack their own session row one, from the stored bytes (run `embed` or `sweep --embed` afterwards). `paste-cache`, `tasks` and `uploads` get no chunks or embeddings, and none of the four kinds gets summaries. Sweeps and the hook (session subtree, slug memory dir, session file-history, the session's `uploads/<sid>` and `tasks/<sid>`, and all of `plans/` and `paste-cache/`) archive assets. The hook skips dataless files (macOS cloud placeholders, `SF_DATALESS`) in these folders so it never waits for a download; the sweep does not skip them. `chunks.record_id` is a foreign key, so asset chunks hang off an anchor record (file `asset:anchor`, one record per asset, no raw line); `doctor` counts include those anchor rows.
- Restore: `restore SID --to DIR` also writes the session's assets in the same tree and sets original mode (`files.mode` / `assets.mode`, default 0600) and mtime (`os.Chtimes`); it still refuses existing targets and verifies asset sha256 before writing. `restore memory:<slug>` restores a slug's memory files. File-history snapshots are written (under `file-history/<sid>/`) only with `--with-file-history`. A session's `uploads/<sid>/` and `tasks/<sid>/` files are written (under `uploads/<sid>/` and `tasks/<sid>/`) only with `--with-profile-dirs`, which implies `--with-file-history`. `restore plans:<profile>`, `paste-cache:<profile>`, `tasks:<profile>` and `uploads:<profile>` restore those pseudo-sessions' files (under `<kind>/…`) with no flag; `restore 'plans:<profile>/<name>'` restores one plan. These ids are taken as given, with no prefix matching. `--in-place` writes every one of these back to its original path. Files directly under a slug directory (for example `bridge-pointer.json`) have no session and are archived but not restorable by session id.
- `tool_outputs(record_id, idx, tool_use_id, inline_bytes, saved_path, asset_id)` has one row per `tool_result` block; a `Full output saved to: <path>` pointer (or any absolute path under `tool-results/`) is resolved to the archived asset (`asset_id`). Only Claude Code's message form `Output too large (...). Full output saved to: <path>` at the start of a line counts as a pointer (first match only); a tool-results path merely quoted in Read or grep output does not. `records.raw` always holds the complete tool_result text; `tool_fts` is only an 8 KB index.
- `file_changes` rows (derived from `Edit`, `MultiEdit`, `Write`, `NotebookEdit` and Bash `sed -i`, `>`, `>>`, `tee`) carry `record_id`, `tool_use_id`, `result_record_id`, `result_ok`, `file_path` (trigram `file_fts`). `added_chars`/`removed_chars` are the character counts of the new/old strings. `session-index files SUBSTRING [--profile NAME] [--json]` lists changes newest first; `show SID --changes` lists a session's changes grouped by file.
- Summaries: `session-index summarize [--max N] [--budget 60s] [--model NAME] [--parallel P] [--kind tool,change,asset]` generates one summary with the configured summarizer (default: local Ollama `gemma4:e4b`; see Configuration) for each inline tool output of at least 2,048 bytes, each `file_changes` row and each text tool-result file. Non-text tool-result files (pdf, images) get a `failed:binary` summary row so the pending count is honest. Rows live in `summaries` (failures are parked as empty text with model `failed:<reason>` and are not retried), each summary is also a `chunks` row (kind `summary` or `filechange`, text `<path> (<tool> <date>): <summary>` for changes) that the normal `embed` drain vectorizes and FTS-indexes. It takes `summarize.lock`, is resumable, and stops with exit 1 when Ollama is down (exit 0 in hook context). The hook-spawned `embed` child runs summarize for 45 s after draining (live sessions get summaries incrementally); the full backlog (about 11,100 tool outputs, 159 files and about 5,000 file changes, at 1-3 s each) is run manually. Search shows `[summary]`, `[file]` and `[memory]` markers; `--json` has `kind` `summary`, `filechange` or `memory`. `show` prints a `[tool output summary]` line under tool results that have one.
- `doctor` prints asset/file-change/tool-output counts, summaries done/failed/pending, runs `PRAGMA foreign_key_check`, lists unresolved tool-result pointers (expected only for files deleted before archiving) and orphan summaries or summary/filechange chunks; `doctor --full` also verifies 100 random records (byte_len) and 20 random assets (size and sha256) and compares `tool_outputs` with the number of tool_result blocks.
- `session-index rebuild-extras` (embed and summarize locks held) re-derives `file_changes`, `file_fts` and `tool_outputs` from `records.raw` after an extraction fix: it deletes the change summaries and their `filechange` chunks, keeps tool-output summaries whose `(record_id, idx)` still exists, re-derives memory and plan chunks and re-runs the backfill; run `embed` afterwards.
- `rebuild-derived` and `repair` leave `assets` and `summaries` alone: rebuild re-creates file changes, tool outputs, memory and plan chunks and the summary chunks and re-links summaries to the new change ids; repair only removes derived rows and summaries whose record, change or asset is gone.

## Maintenance

- `session-index repair` removes derived rows whose record is missing or superseded, recomputes affected sessions and rebuilds every FTS5 index (`chunk_fts`, `tool_fts`, `asset_fts`, `file_fts`, and any added later; the list is read from `sqlite_master`; no embedding); `session-index doctor [--full]` runs the integrity checks that tell you when it is needed.
- FTS integrity false positives: `PRAGMA integrity_check` can report `malformed inverted index for FTS5 table ...` while another process is writing (for example a hook-spawned `embed`) and then report `ok` minutes later with no repair. Reproduced with the ncruces WASM build (`internal/store/fts_integrity_test.go`): bulk DELETEs by an UNINDEXED column, by rowid, of everything, followed by `optimize` or `automerge`, never corrupt a trigram FTS5 table on a single connection; but a long-lived second connection checking while a writer upserts (DELETE then INSERT per row, as asset indexing does) is flagged on almost every run, and stays flagged until an FTS5 command refreshes it, while fresh connections are never flagged and every connection is ok once the writer is done. The data was not corrupt: it is a stale per-connection FTS5 view. `doctor` therefore runs `PRAGMA integrity_check` (with `--full`) first and then the FTS5 `integrity-check` of every FTS table on one connection (the FTS commands cache FTS state, so they must not precede the PRAGMA) and, only if anything fails, repeats everything once on a brand-new connection pool before printing FAILED. The delete statements were left unchanged.
- `session-index vacuum` runs `VACUUM` on the local archive (never a peer), then a WAL checkpoint, and prints the size before, after and bytes reclaimed; it needs free disk space about the size of the archive and refuses to run while an embed holds the lock.

## Retention

Because transcripts are archived losslessly (raw lines are stored in `records.raw`), Claude Code's `cleanupPeriodDays` can now be lowered; `session-index restore SESSION_ID --to DIR` recreates deleted transcripts.

## Encryption

Off by default. `session-index encrypt` encrypts the whole archive file (raw lines, FTS indexes, vectors, and its journal or WAL) with the SQLite driver's Adiantum VFS (`vfs/adiantum`, 4 KiB blocks). The file keeps its name, `archive.sqlite`.

- Key: 32 random bytes. `archive.key`, next to the archive, holds the key locked with your passphrase (Argon2id: time 3, memory 64 MiB, 4 threads; XChaCha20-Poly1305) and syncs with the archive. The unlocked key is cached per machine in `~/.config/session-index/keys/<key id>.key` (mode 0600), so hooks and search run without a prompt. `SESSION_INDEX_KEY_DIR` moves that directory; keep it out of any synced tree.
- `encrypt [--wait 30s] [--recover]` asks for a new passphrase twice (at least 8 characters; one line from stdin when stdin is not a terminal), writes the key file, waits up to `--wait` for sole access to the archive, copies it with `VACUUM INTO`, verifies the copy (`PRAGMA integrity_check`, same tables, same row counts) and swaps the files. Embeddings and summaries are copied, not rebuilt. The plaintext file stays as `archive.sqlite.plaintext-<UTC timestamp>`: nothing deletes it, and `doctor` warns while it exists. It needs free disk space about the size of the archive. A 1.3 GB archive took about 20 s.
- `decrypt [--wait 30s] [--recover]` is the reverse. The encrypted file stays as `archive.sqlite.encrypted-<UTC timestamp>` and the key file stays, so a later `encrypt` reuses it (it asks for the same passphrase).
- `unlock` asks for the passphrase once and caches the key for the own archive and for every peer archive that has a key file. Run it once on each other machine. `lock` removes the cached keys from this machine.
- An archive that does not exist yet is created encrypted when `archive.key` is already next to its path.
- While `encrypt` or `decrypt` runs, `archive.sqlite.migrating` exists and every other open of the archive fails. A run that was interrupted is undone by the next `encrypt` or `decrypt`, or by `--recover` alone.
- On a machine where the archive is locked, commands fail with `archive is encrypted and locked on this machine (run: session-index unlock)`. Hooks exit 0 and archive nothing; the `Stop` hook also prints a `systemMessage` that says so. After `unlock`, `session-index sweep` picks up what was missed.
- `doctor` prints `encryption: off` or `encryption: on (key <id>, unlocked on this machine)` (or `LOCKED on this machine`).
- Before encrypting, every `session-index` binary that opens the archive (the hooks on each machine, a `session-archive` link) must be a version that has these commands. Older versions cannot open an encrypted archive.
- What it protects: the file at rest (the copy at the sync provider, backups, other accounts on the machine). What it does not: software running as you on an unlocked machine, which can read the cached key. Adiantum is deterministic and not authenticated: a modified file is not detected, and someone who sees successive versions of the file can tell which blocks changed.
- There is no recovery without the passphrase. A new machine needs both `archive.key` and the passphrase, so back up the key file with the archive.
- The `sqlite3` command and other SQLite tools cannot read an encrypted archive. For a snapshot, copy `archive.sqlite` and `archive.key` while no session is writing.
- Tested on macOS only.

## Renamed from session-archive

The tool was called `session-archive`. Existing setups keep working: `SESSION_ARCHIVE_*` environment variables are still read when the `SESSION_INDEX_*` name is unset, `~/.config/session-archive/config.json` is used when the new config file is absent, and hook entries that call `session-archive ingest` are still recognised. The database folder keeps its name, `cache/session-archive/`, so every machine's profile stays discoverable. Keep a `session-archive` link to the new binary until old hook entries are rewritten.

## Design note: SQLite driver

Chosen: `github.com/ncruces/go-sqlite3` v0.21.3 (pure Go, WASM SQLite, no cgo) with `github.com/asg017/sqlite-vec-go-bindings/ncruces` v0.1.6.

- Benchmark: 15,273 x 768 float32 vectors in `chunk_vec`, 100 KNN k=50 queries: p50 = 22.1 ms, p95 = 23.6 ms (threshold 40 ms).
- `mattn/go-sqlite3` + cgo bindings was not run: ncruces p50 is below the 40 ms switch threshold, so no fallback comparison number exists.
- Version pin: the binding's embedded wasm only links against ncruces v0.19.0 to v0.21.3 and v0.23.3 (tested: v0.16.3 and v0.18.0 fail on `go_busy_timeout` signature, v0.22.0 on atomics, v0.25.2 on `go_final`; ncruces v0.37.0 no longer has `sqlite3.Binary`, so the binding does not compile). v0.21.3 is the newest tag in the contiguous passing range.
- Windows: v0.21.3 has three WAL defects there. (1) It maps a WAL-index region at an offset Windows rejects once a WAL grows past about 4,000 pages ("disk I/O error"; ncruces/go-sqlite3 issue 330, fixed upstream in v0.30.1, which the binding does not link against). (2) It fills every missing region slot with a view of the requested region, so a second connection can read a wrong index ("database disk image is malformed"). (3) Its timed file lock can be granted after it timed out and is then never released, so every writer waits out its busy timeout ("database is locked"; removed upstream in v0.25.0, issue 254). `go.mod` replaces the driver with `github.com/wamra/go-sqlite3`, branch `wamra/v0.21.3-windows-wal`: v0.21.3 plus the three fixes. The archive is opened with `journal_mode=WAL` on every platform.
- The binding's wasm uses atomics, so `internal/store` sets `sqlite3.RuntimeConfig` with `experimental.CoreFeaturesThreads`.

## Development

- `go build ./cmd/session-index` build the binary
- `go vet ./... && go test ./...` run checks and tests
- `SESSION_INDEX_BENCH_VECS=/path/to/vectors.f32 go test ./bench/ -run TestKNN -v` run the KNN micro-benchmark (raw little-endian float32 rows of 768 values; skipped when the variable is unset)
- `go run ./bench/labeled_eval --queries FILE.json` run the labeled retrieval eval against your own archive (or set `SESSION_INDEX_LABELED_QUERIES`, which also enables the router category test in `internal/search`). The labeled query set is not part of the repository: it is derived from private sessions.
