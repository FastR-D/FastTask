import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// Build artifacts (doc/harness.md §9.1, §14.8).
//
// libfx ships a 4.5 MB interactive terminal (fx-term.wasm) and four 6–7 MB native addons. None of them
// belong in a browser bundle, and nothing at runtime would notice if they leaked — the bundle would just
// be 30 MB heavier and the PWA precache would blow its budget. These assertions are what makes the
// omission structural rather than lucky.
//
// The dist checks need `npm run build` to have run; CI does that before the tests. When dist is absent
// the source-level invariants are still checked, so the suite is meaningful on a fresh checkout too.

const dist = resolve(process.cwd(), 'dist')
const src = resolve(process.cwd(), 'src')
const hasDist = existsSync(dist)

function walk(directory: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(directory)) {
    const path = join(directory, entry)
    if (statSync(path).isDirectory()) out.push(...walk(path))
    else out.push(path)
  }
  return out
}

describe('browser artifacts', () => {
  it('never imports the libfx entry that drags in fx-term.wasm (§9.1)', () => {
    // The package root resolves default asset URLs for BOTH wasm files at module scope, so importing it
    // makes the bundler emit the terminal. harness/runtime.ts imports libfx/wasm instead.
    const offenders = walk(src)
      .filter(path => /\.(ts|tsx)$/.test(path) && !path.endsWith('.d.ts') && !path.endsWith('.test.ts'))
      .filter(path => /from\s+['"]libfx['"]/.test(readFileSync(path, 'utf8')))
    expect(offenders.map(path => path.replace(src, 'src'))).toEqual([])
  })

  it('self-hosts the runtime instead of pointing at a CDN (§9.1, ADR-0005 §3.3)', () => {
    const backend = readFileSync(join(src, 'harness', 'backend.ts'), 'utf8')
    expect(backend).toContain("WASM_ASSET_PATH = '/fx-core.wasm'")

    // Nothing may fetch an asset from a third-party CDN: the runtime has to work with no egress but our
    // own server. Test files are excluded because they quote these hostnames to forbid them.
    const cdns = ['unpkg.com', 'jsdelivr.net', 'cdnjs.cloudflare.com']
    const offenders = walk(src)
      .filter(path => /\.(ts|tsx)$/.test(path) && !path.endsWith('.test.ts') && !path.endsWith('.test.tsx'))
      .filter(path => {
        const text = readFileSync(path, 'utf8')
        return cdns.some(cdn => text.includes(cdn))
      })
    expect(offenders.map(path => path.replace(src, 'src'))).toEqual([])
  })

  it.skipIf(!hasDist)('contains the runtime and its precompressed siblings (§9.1)', () => {
    const files = walk(dist).map(path => path.replace(dist, ''))
    expect(files).toContain('/fx-core.wasm')
    expect(files).toContain('/fx-core.wasm.br')
    expect(files).toContain('/fx-core.wasm.gz')
    const raw = statSync(join(dist, 'fx-core.wasm')).size
    const brotli = statSync(join(dist, 'fx-core.wasm.br')).size
    expect(brotli).toBeLessThan(raw / 2)
    // A wasm module starts with its magic number; a corrupted copy would compile to nothing.
    const magic = readFileSync(join(dist, 'fx-core.wasm')).subarray(0, 4)
    expect(Array.from(magic)).toEqual([0x00, 0x61, 0x73, 0x6d])
  })

  it.skipIf(!hasDist)('carries no interactive terminal and no native addon (§9.1, §14.8)', () => {
    const offenders = walk(dist).filter(path => path.includes('fx-term') || path.endsWith('.node'))
    expect(offenders.map(path => path.replace(dist, 'dist'))).toEqual([])
  })

  it.skipIf(!hasDist)('keeps the precache manifest inside its own budget (doc/pwa.md)', () => {
    // maximumFileSizeToCacheInBytes is 4 MB; the runtime is 2 MB, so it fits with room for the shell.
    // What must not happen is the terminal (4.5 MB) or an addon (6 MB+) landing in the manifest.
    const manifest = walk(dist).find(path => path.endsWith('sw.js'))
    expect(manifest).toBeDefined()
    const text = readFileSync(manifest!, 'utf8')
    expect(text).toContain('fx-core.wasm')
    expect(text).not.toContain('fx-term')
    expect(text).not.toContain('.node')
    expect(text).not.toContain('fx-core.wasm.br')
  })
})
