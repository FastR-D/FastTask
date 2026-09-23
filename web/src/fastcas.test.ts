import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

function response(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}
const access = () => `header.${btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + 3600 }))}.signature`

describe('optional FastCAS callback session boundaries', () => {
  beforeEach(() => { vi.resetModules(); localStorage.clear(); sessionStorage.clear() })
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

  it('consumes the callback once when React mounts concurrently and preserves the local session after binding', async () => {
    const api = await import('./api')
    const original = access()
    api.token.set(original)
    localStorage.setItem('fasttask_refresh', 'local-refresh')
    const fetch = vi.fn(async () => response({ linked: true }))
    vi.stubGlobal('fetch', fetch)
    const [a, b] = await Promise.all([api.completeFastCAS(), api.completeFastCAS()])
    expect(a.linked && b.linked).toBe(true)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(api.token.get()).toBe(original)
    expect(localStorage.getItem('fasttask_refresh')).toBe('local-refresh')
  })

  it('a rejected optional callback neither refreshes nor clears the working local login', async () => {
    const api = await import('./api')
    const original = access()
    api.token.set(original)
    localStorage.setItem('fasttask_refresh', 'local-refresh')
    const fetch = vi.fn(async () => response({ detail: 'expired transaction' }, 401))
    vi.stubGlobal('fetch', fetch)
    await expect(api.completeFastCAS()).rejects.toThrow('expired transaction')
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(api.token.get()).toBe(original)
    expect(localStorage.getItem('fasttask_refresh')).toBe('local-refresh')
  })

  it('stores only the project session returned by a successful CAS login', async () => {
    const api = await import('./api')
    const projectAccess = access()
    vi.stubGlobal('fetch', vi.fn(async () => response({ linked: false, access_token: projectAccess, refresh_token: 'project-refresh' })))
    await api.completeFastCAS()
    expect(api.token.get()).toBe(projectAccess)
    expect(localStorage.getItem('fasttask_refresh')).toBe('project-refresh')
    expect(localStorage.getItem('fasttask_access')).toBeNull()
    expect(sessionStorage.getItem('fasttask_access')).toBeNull()
  })
})
