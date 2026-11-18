# session-index

A lossless, searchable index of your Claude Code sessions.

Claude Code removes old transcripts after `cleanupPeriodDays`, and compaction hides earlier detail from the model. `session-index` keeps every transcript line in one SQLite file, so you can search any past session and restore it.

- **Lossless.** Every JSONL line is stored verbatim. `restore` recreates the transcript files byte for byte. Plans, pasted text, task lists and uploads that Claude Code also deletes are archived too.
- **Searchable.** Full-text search for exact strings and semantic search for prose, routed automatically.
- **Always current.** Claude Code hooks index each session as you work.
- **Compaction-safe.** Messages from before a compaction stay in the index and stay searchable.
- **Multi-machine.** Each machine keeps its own archive; one search covers all of them when the profiles are synced.
- **Local first.** By default nothing leaves your machine. Hosted providers (Anthropic, Bedrock, OpenAI-compatible) are opt-in.
- **Encrypted if you want.** `session-index encrypt` encrypts the whole archive file with a key locked by your passphrase.
- **One binary.** Pure Go, no cgo.

## Install

Building from source needs Go 1.24.1 or newer. Semantic search needs Ollama with `embeddinggemma`, or another provider (see [Configuration](docs/REFERENCE.md#configuration-model-providers)). Without one, search falls back to full-text.

**macOS, Linux:**

```
scripts/install.sh              # builds, installs to ~/.local/bin, adds the hooks and the skill, runs doctor
session-index backfill --embed  # index your existing sessions
```

**Windows (PowerShell):** `scripts\install.ps1`

**Release archive:** `scripts/build-release.sh` writes archives for macOS, Linux and Windows (amd64, arm64) with checksums into `dist/`. Unpack, put `session-index` on your PATH, then run `session-index install-hooks --skill` and `session-index doctor`.

**Claude Code plugin:** the repository is a plugin (hooks and the search skill) and a marketplace that also lists `session-index-search` (model tools `search` and `show`, and a `/sessions` command). The `session-index` binary must be on PATH.

```
/plugin install session-index --marketplace <owner>/<repo>
/plugin install session-index-search --marketplace <owner>/<repo>
```

Use the plugin hooks or `install-hooks`, not both, or each transcript is ingested twice.

## Use

```
session-index search "where did we fix the WAL checkpoint"
session-index search "LockFileEx" --mode fts --since 30d --project myrepo
session-index show SESSION_ID --around UUID --n 20   # read the conversation
session-index files SUBSTRING                        # which sessions changed a file
session-index restore SESSION_ID --to DIR            # recreate a deleted transcript
session-index encrypt                                # encrypt the archive file (asks for a passphrase)
session-index stats
session-index doctor
session-index help                                   # every command and flag
```

Inside Claude Code, the `session-search` skill and the `session-index-search` tools let Claude search the index itself.

## How it works

- Hooks on `Stop`, `SubagentStop` and `SessionEnd` run `session-index ingest`, which appends the new transcript lines to the archive and indexes them. Embedding runs in a detached process.
- The index runs one turn behind: a hook can fire before the last lines of a turn are written. The next hook, or `session-index sweep --embed`, picks them up.
- Transcripts are append-only, and so is the archive. A compaction adds a boundary record and a summary record; nothing earlier is overwritten.
- The database is `~/.claude/cache/session-archive/archive.sqlite` (SQLite with FTS5 and sqlite-vec). Set `SESSION_INDEX_DB` to move it.

## Status

Version 0.1.0. Only macOS is in daily use.

- Linux: the test suite passes in CI; not used beyond that.
- Windows: the test suite passes in CI; not used beyond that, and `install.ps1` has not been run.
- Encryption has been tested on macOS only.
- The plugin install commands and `.goreleaser.yaml` have not been tested.

## Documentation

- [docs/REFERENCE.md](docs/REFERENCE.md): configuration, hosted providers and cost guards, multi-profile archives, restore, summaries, maintenance, retention, encryption, rollback.
- [BENCHMARKS.md](BENCHMARKS.md): search quality and speed.
- [plugins/session-index-search/README.md](plugins/session-index-search/README.md): the in-session search tools.

## License

MIT. See [LICENSE](LICENSE).
