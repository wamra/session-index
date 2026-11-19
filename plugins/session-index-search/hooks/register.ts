import type { EngineInterface, Register } from 'claude-code'

const SEARCH = 'mcp__session-index-search__search'
const SHOW = 'mcp__session-index-search__show'
const MAX_TEXT = 12000
const DEFAULT_BIN = 'session-index'

type Hit = {
  session_id: string
  profile?: string
  project?: string
  title?: string
  score?: number
  chunk?: string
  ts?: string
  role?: string
  kind?: string
}

type SearchInput = {
  query: string
  mode?: 'auto' | 'vector' | 'fts' | 'hybrid'
  since?: string
  profile?: string
  project?: string
  role?: 'user' | 'assistant'
  kind?: 'main' | 'subagent' | 'workflow' | 'plan'
  k?: number
}

type ShowInput = { session_id: string; around?: string; n?: number }

const clip = (s: string, n: number) => (s.length > n ? `${s.slice(0, n)}\n[cut: ${s.length - n} more characters]` : s)

const one = (s: string | undefined, n: number) => (s ?? '').replace(/\s+/g, ' ').trim().slice(0, n)

function searchArgv(i: SearchInput): string[] {
  const argv = ['search', i.query, '--json']
  if (i.mode) argv.push('--mode', i.mode)
  if (i.since) argv.push('--since', i.since)
  if (i.profile) argv.push('--profile', i.profile)
  if (i.project) argv.push('--project', i.project)
  if (i.role) argv.push('--role', i.role)
  if (i.kind) argv.push('--kind', i.kind)
  argv.push('--k', String(Math.min(Math.max(i.k ?? 8, 1), 25)))
  return argv
}

function formatHits(stdout: string): string {
  let parsed: { hits?: Hit[]; route?: string }
  try {
    parsed = JSON.parse(stdout)
  } catch {
    return clip(stdout.trim() || 'No output from session-index.', MAX_TEXT)
  }
  const hits = parsed.hits ?? []
  if (hits.length === 0) return 'No matching sessions.'
  const lines = hits.map((h, n) =>
    [
      `${n + 1}. ${h.session_id.includes(':') ? h.session_id : h.session_id.slice(0, 12)}  ${(h.ts ?? '').slice(0, 10)}  ${h.profile ?? ''}  ${one(h.project, 40)}`,
      `   title: ${one(h.title, 80) || '(untitled)'}  role: ${h.role ?? '?'}  score: ${(h.score ?? 0).toFixed(3)}`,
      `   ${one(h.chunk, 300)}`,
    ].join('\n'),
  )
  const route = parsed.route ? `route: ${parsed.route}\n` : ''
  return `${route}${lines.join('\n')}\n\nRead one with the show tool (session_id prefix is enough; for a plan pass the whole plans:... id). Restore transcripts only with 'session-index restore SESSION_ID --to DIR', never --in-place without asking.`
}

// Without a "binary" option the plugin runs its bundled launcher (bin/ next to plugin.json) and
// falls back to session-index on PATH when the launcher is absent (sh exits 127).
async function run($: EngineInterface, bin: string, argv: string[]) {
  try {
    let r = await $.process.run(bin === DEFAULT_BIN ? ['sh', `${$.plugin.root}/bin/session-index`, ...argv] : [bin, ...argv], { timeoutMs: 90_000 })
    if (bin === DEFAULT_BIN && r.exitCode === 127) r = await $.process.run([bin, ...argv], { timeoutMs: 90_000 })
    if (r.exitCode !== 0) return { ok: false as const, text: clip(`session-index exited ${r.exitCode}: ${r.stderr.trim() || r.stdout.trim()}`, MAX_TEXT) }
    return { ok: true as const, text: r.stdout }
  } catch (err) {
    return { ok: false as const, text: `Could not run '${bin}': ${String(err)}. Set the session-index-search "binary" option to the executable's full path.` }
  }
}

export const register: Register = (on, options) => {
  const bin = String((options as { binary?: string } | undefined)?.binary || 'session-index')

  on('session.start', async ($, e, next) => {
    await $.tool.register({
      name: 'search',
      description:
        "Search the user's archived Claude Code sessions on every machine and profile (lossless archive of past transcripts, including compacted-away detail). Use for 'find the session where...', 'where did we discuss X', or recalling earlier decisions. Default mode routes identifiers and file names to full-text search and prose to semantic search. Use mode 'fts' for exact strings.",
      inputSchema: {
        type: 'object',
        properties: {
          query: { type: 'string', description: 'What to look for.' },
          mode: { type: 'string', enum: ['auto', 'vector', 'fts', 'hybrid'], description: "Default 'auto'." },
          since: { type: 'string', description: "Limit by age: '7d', '24h' or 'YYYY-MM-DD'." },
          profile: { type: 'string', description: "Only this machine/profile, for example 'laptop'." },
          project: { type: 'string', description: 'Only projects whose name contains this text.' },
          role: { type: 'string', enum: ['user', 'assistant'] },
          kind: { type: 'string', enum: ['main', 'subagent', 'workflow', 'plan'] },
          k: { type: 'number', description: 'Sessions to return, 1 to 25 (default 8).' },
        },
        required: ['query'],
      },
      isDeferred: false,
    })
    await $.tool.register({
      name: 'show',
      description:
        'Read messages from one archived session found by the search tool. Pass a session id prefix; optionally a message uuid to center a window on (around) and the window size (n). For a plan hit pass its whole plans:... id: the archived plan file is returned.',
      inputSchema: {
        type: 'object',
        properties: {
          session_id: { type: 'string', description: 'Session id or unique prefix.' },
          around: { type: 'string', description: 'Message uuid to center the window on.' },
          n: { type: 'number', description: 'Messages to show (default 20, max 100).' },
        },
        required: ['session_id'],
      },
      isDeferred: false,
    })
    await $.command.register({
      name: 'sessions',
      description: 'Search archived sessions: /sessions <query>, /sessions show <id>, or /sessions alone for archive stats',
      argumentHint: '[query | show <id>]',
    })
    return next(e)
  })

  on('tool.call', { tool: SEARCH }, async ($, e) => {
    const i = e as unknown as SearchInput
    if (typeof i.query !== 'string' || i.query.trim() === '') return { deny: 'query is required.' }
    const r = await run($, bin, searchArgv(i))
    return { result: r.ok ? formatHits(r.text) : r.text, ...(r.ok ? {} : { isError: true as const }) } as never
  }).catch(() => ({ deny: 'session-index-search failed unexpectedly; try the session-index command in Bash.' }))

  on('tool.call', { tool: SHOW }, async ($, e) => {
    const i = e as unknown as ShowInput
    if (typeof i.session_id !== 'string' || !/^([0-9a-fA-F-]{4,}|plans:\S+)$/.test(i.session_id)) return { deny: 'session_id must be a session id prefix (hex and dashes) or a plans:... id from a plan hit.' }
    const argv = ['show', i.session_id, '--n', String(Math.min(Math.max(i.n ?? 20, 1), 100))]
    if (i.around) argv.push('--around', i.around)
    const r = await run($, bin, argv)
    return { result: clip(r.text.trim() || 'No messages.', MAX_TEXT), ...(r.ok ? {} : { isError: true as const }) } as never
  }).catch(() => ({ deny: 'session-index-search failed unexpectedly; try the session-index command in Bash.' }))

  on('command.run', { command: 'sessions' }, async ($, e) => {
    const args = e.args.trim()
    if (args === '') {
      const r = await run($, bin, ['stats'])
      return { text: clip(r.text.trim() || 'No output.', MAX_TEXT) }
    }
    const m = /^show\s+(\S+)$/.exec(args)
    if (m) {
      const r = await run($, bin, ['show', m[1], '--n', '20'])
      return { text: clip(r.text.trim() || 'No messages.', MAX_TEXT) }
    }
    const r = await run($, bin, searchArgv({ query: args }))
    return { text: r.ok ? formatHits(r.text) : r.text }
  })
}
