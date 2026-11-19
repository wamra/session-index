import { expect, test } from 'claude-code/testing'

const HITS = JSON.stringify({
  hits: [
    {
      session_id: '11111111-2222-3333-4444-555555555555',
      profile: 'desktop',
      project: '-home-user-dev',
      title: 'Session Indexing',
      score: 0.16,
      chunk: 'Allow the go toolchain through the firewall',
      ts: '2026-11-09T01:32:03.392Z',
      role: 'user',
    },
  ],
})

test('the search tool runs the binary with --json and formats hits', async ($, on) => {
  let seen: readonly string[] = []
  on('process.run', (_, e) => {
    seen = e.argv
    return { value: { exitCode: 0, stdout: HITS, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('tool.register', () => ({ value: {} }))
  on('command.register', () => ({ value: { command: 'sessions' } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })

  const out = await $.tool.call({ tool: 'mcp__session-index-search__search', query: 'firewall', since: '7d', k: 3 })
  expect(seen[0]).toBe('sh')
  expect(seen[1]).toMatch(/\/bin\/session-index$/)
  expect(seen.slice(2)).toEqual(['search', 'firewall', '--json', '--since', '7d', '--k', '3'])
  expect(String((out as { result?: unknown }).result)).toContain('11111111-222')
})

test('an empty query is refused and a failing binary is an error result', async ($, on) => {
  on('process.run', () => ({ value: { exitCode: 2, stdout: '', stderr: 'boom', isStdoutTruncated: false, isStderrTruncated: false } }))
  on('tool.register', () => ({ value: {} }))
  on('command.register', () => ({ value: { command: 'sessions' } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })

  const empty = await $.tool.call({ tool: 'mcp__session-index-search__search', query: ' ' })
  expect((empty as { deny?: string }).deny).toBeDefined()
  const bad = await $.tool.call({ tool: 'mcp__session-index-search__search', query: 'x' })
  expect(JSON.stringify(bad)).toContain('boom')
})

test('without the bundled launcher (sh exits 127) it falls back to session-index on PATH', async ($, on) => {
  const calls: (readonly string[])[] = []
  on('process.run', (_, e) => {
    calls.push(e.argv)
    const exitCode = e.argv[0] === 'sh' ? 127 : 0
    return { value: { exitCode, stdout: exitCode === 0 ? HITS : '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('tool.register', () => ({ value: {} }))
  on('command.register', () => ({ value: { command: 'sessions' } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'desktop', isInteractive: true })

  const out = await $.tool.call({ tool: 'mcp__session-index-search__search', query: 'firewall' })
  expect(calls.map((c) => c[0])).toEqual(['sh', 'session-index'])
  expect(String((out as { result?: unknown }).result)).toContain('11111111-222')
})
