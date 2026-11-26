# session-index-search

A Claude Code mod that puts the `session-index` binary inside the session. It gives the model two tools and gives you one slash command.

- `search` tool: searches the lossless archive of every Claude Code transcript across all synced machines and profiles. Filters: mode, since, profile, project, role, kind, k.
- `show` tool: reads messages from one archived session, optionally a window around a message uuid.
- `/sessions <query>`: prints search hits. `/sessions show <id>` reads a session. `/sessions` alone prints archive stats.

This directory is the source of the mod that ships inside the `session-index` plugin (see the repository README). The build (`scripts/build-plugin.sh`) renames its tool prefix to `mcp__session-index__`, so installing the plugin is the supported way to get it; this folder is not a separate marketplace entry.

The plugin bundles the `session-index` binary and runs it through a launcher. The launcher falls back to `session-index` on PATH; set the plugin option `binary` to a command or full path to use a different executable. Semantic search needs Ollama with the embeddinggemma model (or another configured provider); otherwise prose queries fall back to full-text search.

The mod only reads. Archived text it returns is marked as untrusted data. Restoring transcripts is left to `session-index restore --to DIR`.
