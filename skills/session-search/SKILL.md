---
name: session-search
description: Search the user's archived Claude Code sessions across every machine and profile. Use when the user asks "find the session where...", "search my Claude history", "where did I work on X", or wants to restore a deleted session.
---

# session-search

`session-index` keeps a lossless SQLite archive of Claude Code transcripts and searches all synced profile archives (this machine and its peers, for example a laptop) at once.

## Search

Run:

```
session-index search "<query>" [--mode fts] [--profile NAME] [--since 30d] [--json]
```

- Default mode (`auto`) routes identifier-shaped queries (paths, flags, snake_case, CONSTANTS) to full-text search and everything else to vector search.
- Use `--mode fts` for exact tokens (error strings, file names, ids).
- `--profile NAME` keeps only sessions from one profile (for example `laptop`).
- `--since 30d` (also `24h`, `YYYY-MM-DD`) limits by date; `--k N` sets how many sessions to return; `--project SUBSTR`, `--role user|assistant`, `--kind main|subagent|workflow|plan` filter further.
- `--json` prints machine-readable hits (session_id, profile, project, title, score, chunk, ts).
- The vector route needs Ollama running with the `embeddinggemma` model. Prose queries fall back to FTS and report `route: fts-fallback` when Ollama is down or the model is missing; identifier-shaped queries use FTS first and fall back to the vector route only when FTS finds nothing. Use `--mode fts` explicitly to skip the embedding attempt.

Each hit line shows: score, date, profile, short session id, project, title, snippet.

Plan files (`~/.claude/plans/*.md`) are indexed too. A plan hit has the session id `plans:<profile>` and a snippet that starts with `[plan]` and the plan's file name; `--kind plan` returns only plans.

## After finding hits

1. Read the hits and summarise the most relevant sessions to the user (date, profile, project, title, why it matches).
2. Offer `session-index show SESSION_ID` (prefix of a session id is accepted; `--around UUID` and `--n N` select a window of messages) to read the conversation.
3. Offer `session-index restore SESSION_ID --to DIR` to recreate the transcript files of a deleted or missing session under DIR.

For a plan hit, read `~/.claude/plans/<file name>` when the file still exists; otherwise `session-index restore plans:<profile> --to DIR` writes every archived plan of that profile under `DIR/plans/`. `show` has nothing to print for a plan.

`show` and `restore` read this machine's archive. Sessions from other profiles appear there after `session-index sweep --all-profiles` has been run on this machine; if `show` reports no messages for a hit from another profile, run that sweep first.

## Safety

Never run `session-index restore --in-place` without asking the user first: it writes into the live `~/.claude/projects` tree. Prefer `--to DIR`.
