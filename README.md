# session-index

A lossless, searchable index of your Claude Code sessions.

Claude Code removes old transcripts after `cleanupPeriodDays`, and compaction hides earlier detail from the model. `session-index` keeps every transcript line in one SQLite file, so you can search any past session and restore it.

- **Lossless.** Every JSONL line is stored verbatim. `restore` recreates the transcript files byte for byte. Plans, pasted text, task lists and uploads that Claude Code also deletes are archived too.
- **Searchable.** Full-text search for exact strings and semantic search for prose, routed automatically. Plan files are searchable too.
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

**Claude Code plugin:** the `plugin` branch is one installable plugin: the archive hooks, the `session-search` skill, the `search` and `show` tools and a `/sessions` command, with the `session-index` binary for macOS, Linux and Windows (amd64, arm64) bundled, so no separate install is needed. The repository's marketplace lists it:

```
/plugin marketplace add wamra/session-index
/plugin install session-index@session-index
```

Use the plugin hooks or `install-hooks`, not both, or each transcript is ingested twice. If a bundled binary cannot run on your system, the launcher falls back to a `session-index` on PATH, or to the plugin option `binary`; hooks never fail a Claude Code session.

CI publishes the branch when a version tag is pushed (`git tag v0.1.1 && git push --tags`) or the `ci` workflow is run by hand. `scripts/build-plugin.sh OUT` builds the same tree locally and `scripts/check-plugin.sh OUT` runs the checks CI runs before publishing.

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

## Privacy and security

**What is stored.** Every line of every transcript under `~/.claude/projects`, verbatim, in one SQLite file. That includes your prompts, the model's replies, tool calls and their output, file contents the model read or wrote, and anything pasted. Also archived: the other files Claude Code keeps under `projects/` (tool results, memory), plans, pasted text, task lists, uploads and file-history snapshots. Search indexes (full-text and vectors) and optional summaries are derived from the same text and stored beside it. Nothing is filtered or redacted: **a secret that appeared in a session (a key, a token, a password, a customer record) is stored as it appeared.** The archive is as sensitive as the sessions themselves.

**Where.** `~/.claude/cache/session-archive/archive.sqlite` with its `-wal` and `-shm` files, plus `embed.log` and a `wazero-cache/` folder (compiled SQLite) in the same folder. `SESSION_INDEX_DB` or `paths.db` in `~/.config/session-index/config.json` moves it. If `~/.claude` lives in a cloud-synced folder, so does the archive; see the warning in [docs/REFERENCE.md](docs/REFERENCE.md#database-location).

**File modes (macOS, Linux).** A new archive folder is created `0700`, and the database, `-wal`, `-shm` and `embed.log` are `0600`; opening an older archive tightens its database files to `0600`. An archive folder that already exists keeps its mode, so check it yourself. The key file `archive.key` is `0600`, and the cached unlocked key (`~/.config/session-index/keys/`) is `0600` in a `0700` folder. On Windows no mode bits are set; the access rules of your user profile apply.

**What leaves the machine.** By default nothing: embeddings and summaries use Ollama at `http://localhost:11434`, and the only network code in the program is the provider clients. Text is sent off the machine only if you configure a hosted provider (`openai` with a non-local `base_url`, `anthropic` or `bedrock`) in the config file or environment; then the text of indexed chunks and of the items being summarized, and your search queries in vector mode, go to that provider (details and cost guards in the reference, "Hosted providers"). If you point the Ollama or OpenAI-compatible URL at another machine, text goes there. The program sends no telemetry and makes no update checks.

**Encryption.** Off by default; the archive is plaintext SQLite. `session-index encrypt` encrypts the whole archive file at rest with a key locked by your passphrase (Adiantum, Argon2id); `session-index decrypt` reverses it. It protects the file in backups and sync copies, not against software running as you while the key is cached, and a modified file is not detected. `encrypt` leaves the old plaintext file next to the archive as `archive.sqlite.plaintext-<timestamp>`: delete it yourself once you have checked the encrypted archive. Tested on macOS only. Details: [docs/REFERENCE.md](docs/REFERENCE.md#encryption).

**Deleting data.** There is no purge or forget command yet. To remove the archive on this machine, uninstall the hooks (or the plugin) first, otherwise the next session or sweep re-archives what is still in `~/.claude/projects`, then delete:

- the archive folder: `~/.claude/cache/session-archive/` (or the folder that holds your `SESSION_INDEX_DB` / `paths.db`). This removes the database, `-wal`, `-shm`, `embed.log`, `wazero-cache/`, `archive.key` and any `archive.sqlite.plaintext-*` copy.
- the cached keys: `~/.config/session-index/keys/` (or your `SESSION_INDEX_KEY_DIR`).
- optionally the configuration: `~/.config/session-index/config.json`.

This does not remove the original transcripts in `~/.claude/projects`, copies the sync service keeps (version history, trash), archives of the same profile on other machines, or text a hosted provider already received. To forget a single session there is no supported way; deleting the transcript file does not remove its archived lines.

Report vulnerabilities as described in [SECURITY.md](SECURITY.md). Third-party licences: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Archived text is untrusted. The skill and the search tools tell the model to treat transcript text as data, never as instructions, because a past session can contain text written to steer a model.

## Status

Version 0.1.0. Only macOS is in daily use.

- Linux: the test suite passes in CI; not used beyond that.
- Windows: the test suite passes in CI; not used beyond that, and `install.ps1` has not been run.
- Encryption has been tested on macOS only.
- The plugin install commands above and `.goreleaser.yaml` have not been tested.

## Documentation

- [docs/REFERENCE.md](docs/REFERENCE.md): configuration, hosted providers and cost guards, multi-profile archives, restore, summaries, maintenance, retention, encryption, rollback.
- [BENCHMARKS.md](BENCHMARKS.md): search quality and speed.
- [plugins/session-index-search/README.md](plugins/session-index-search/README.md): the in-session search tools.

## License

MIT. See [LICENSE](LICENSE).
