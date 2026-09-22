import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// The harness boundary (doc/harness.md §3.1).
//
// Rule 1: libfx types must not appear outside web/src/harness/. The UI renders from server state, so a
// component that learned about turn events or HostTool shapes would be a second source of truth for what
// a run is doing — and the reason §3.1 asks for lint or test coverage rather than review.
// Rule 2: a host persists nothing. A checkpoint in localStorage would break cross-device continuation,
// which is the point of storing it server-side (§6.1).

const root = resolve(process.cwd(), 'src')

function walk(directory: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(directory)) {
    const path = join(directory, entry)
    if (statSync(path).isDirectory()) out.push(...walk(path))
    else if (/\.(ts|tsx)$/.test(path) && !path.endsWith('.d.ts')) out.push(path)
  }
  return out
}

// code strips line and block comments, so a rule is checked against what a file DOES. The harness files
// quote these APIs in comments precisely to say they are forbidden.
function code(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .split('\n')
    .filter(line => !line.trim().startsWith('//') && !line.trim().startsWith('*'))
    .join('\n')
}

const sources = walk(root).map(path => ({
  path,
  relative: relative(root, path),
  text: readFileSync(path, 'utf8'),
  code: code(readFileSync(path, 'utf8')),
}))
const outsideHarness = sources.filter(file => !file.relative.startsWith('harness/'))
const insideHarness = sources.filter(file => file.relative.startsWith('harness/') && !file.relative.endsWith('.test.ts'))

describe('harness boundary', () => {
  it('keeps every libfx import inside web/src/harness/ (§3.1 rule 1)', () => {
    const leaking = outsideHarness.filter(file => /from\s+['"]libfx['"]|import\(\s*['"]libfx['"]\s*\)/.test(file.code))
    expect(leaking.map(file => file.relative)).toEqual([])
  })

  it('keeps libfx vocabulary out of the UI layer (§3.1 rule 1)', () => {
    // Names that only exist in libfx. A UI file mentioning one has almost certainly imported a type it
    // should not know about, or worse, started branching on harness internals.
    const vocabulary = ['createFxAgent', 'HostTool', 'FxTurn', 'FxAgent', 'supportsJspi', 'getBackendInfo', 'stopReason']
    const leaking = outsideHarness
      .map(file => ({ file, hits: vocabulary.filter(word => file.code.includes(word)) }))
      .filter(entry => entry.hits.length > 0)
    expect(leaking.map(entry => `${entry.file.relative}: ${entry.hits.join(', ')}`)).toEqual([])
  })

  it('exports no libfx type from the harness entry point', () => {
    const entry = sources.find(file => file.relative === 'harness/index.ts')
    expect(entry).toBeDefined()
    // A re-export of a libfx type would put it back in the UI's reach.
    expect(entry!.code).not.toMatch(/export\s+type\s+\{[^}]*\b(Fx|Host)[A-Za-z]*\b/)
    expect(entry!.code).not.toMatch(/from\s+['"]libfx['"]/)
  })

  it('persists nothing on the host (§3.1 rule 2)', () => {
    const offenders = insideHarness
      .map(file => ({
        file,
        hits: ['localStorage', 'sessionStorage', 'indexedDB', 'IDBDatabase', 'document.cookie'].filter(word => file.code.includes(word)),
      }))
      .filter(entry => entry.hits.length > 0)
    expect(offenders.map(entry => `${entry.file.relative}: ${entry.hits.join(', ')}`)).toEqual([])
  })

  it('renders nothing from the host (§3.1 rule 3)', () => {
    // The host consumes turn events for diagnostics and cancellation only; the UI renders from the
    // server's stream. A harness file that reaches for the DOM is a leak in the other direction.
    const offenders = insideHarness.filter(file =>
      /document\.(createElement|querySelector|body)|ReactDOM|\.innerHTML/.test(file.code),
    )
    expect(offenders.map(file => file.relative)).toEqual([])
  })
})
