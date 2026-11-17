# session-index-search

A Claude Code mod that puts the `session-index` binary inside the session. It gives the model two tools and gives you one slash command.

- `search` tool: searches the lossless archive of every Claude Code transcript across all synced machines and profiles. Filters: mode, since, profile, project, role, kind, k.
- `show` tool: reads messages from one archived session, optionally a window around a message uuid.
- `/sessions <query>`: prints search hits. `/sessions show <id>` reads a session. `/sessions` alone prints archive stats.

Setup: install the `session-index` binary and make sure it is on PATH, or set the plugin option `binary` to its full path. Semantic search needs Ollama with the embeddinggemma model; otherwise prose queries fall back to full-text search.

The mod only reads. Restoring transcripts is left to `session-index restore --to DIR`.

Install from a marketplace: `/plugin install session-index-search --marketplace <owner>/<repo>` (the repository that holds this plugin), or pass a local folder path instead.
