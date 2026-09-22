import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { clearSession, ensureFreshAccessToken, hasRefreshToken, login, logout, token } from './api'

// jsonResponse builds the minimal Response shape execute()/refreshAccess() read.
function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    headers: new Headers(),
  } as unknown as Response
}

// fakeJwt mints an unsigned token whose payload carries the given exp (seconds),
// matching the server's RegisteredClaims.ExpiresAt -> `exp` claim.
function fakeJwt(expSeconds: number): string {
  const payload = btoa(JSON.stringify({ exp: expSeconds }))
    .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return `eyJhbGciOiJIUzI1NiJ9.${payload}.sig`
}

const nowSec = () => Math.floor(Date.now() / 1000)

// cacheNames backs a fake Cache Storage so clearSession's cache wipe is observable
// (jsdom does not implement the Cache API).
let cacheNames: string[] = []

describe('api session storage (pwa.md §4 / frontend.md §5)', () => {
  beforeEach(() => {
    localStorage.clear()
    token.clear()
    cacheNames = ['workbox-precache', 'ft-api-user']
    vi.stubGlobal('caches', {
      keys: async () => [...cacheNames],
      delete: async (key: string) => { cacheNames = cacheNames.filter(k => k !== key); return true },
    })
  })
  afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks() })

  it('login keeps the access token in memory and the refresh token in localStorage', async () => {
    const access = fakeJwt(nowSec() + 3600)
    vi.stubGlobal('fetch', vi.fn(async (url: string) =>
      url.endsWith('/auth/login')
        ? jsonResponse({ access_token: access, refresh_token: 'rt-1', user: {} })
        : jsonResponse({}, 404)))
    await login('admin', 'pw')
    expect(token.get()).toBe(access)
    expect(localStorage.getItem('fasttask_refresh')).toBe('rt-1')
    // The access token must never be persisted (memory only).
    expect(localStorage.getItem('fasttask_access')).toBeNull()
    expect(sessionStorage.getItem('fasttask_access')).toBeNull()
    expect(hasRefreshToken()).toBe(true)
  })

  it('clearSession wipes the access token, refresh token and every cache', async () => {
    token.set(fakeJwt(nowSec() + 3600))
    localStorage.setItem('fasttask_refresh', 'rt')
    await clearSession()
    expect(token.get()).toBeNull()
    expect(hasRefreshToken()).toBe(false)
    expect(cacheNames).toEqual([])
  })

  it('ensureFreshAccessToken reuses a valid in-memory token without a network call', async () => {
    const access = fakeJwt(nowSec() + 3600)
    token.set(access)
    localStorage.setItem('fasttask_refresh', 'rt')
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    await expect(ensureFreshAccessToken()).resolves.toBe(access)
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('ensureFreshAccessToken restores from the refresh token on cold start, single-flight and rotating', async () => {
    // Cold start: no access token in memory, but a persisted refresh token.
    localStorage.setItem('fasttask_refresh', 'rt-old')
    const newAccess = fakeJwt(nowSec() + 3600)
    let refreshCalls = 0
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      if (url.endsWith('/auth/refresh')) {
        refreshCalls++
        return jsonResponse({ access_token: newAccess, refresh_token: 'rt-new' })
      }
      return jsonResponse({}, 404)
    }))
    const [a, b] = await Promise.all([ensureFreshAccessToken(), ensureFreshAccessToken()])
    expect(a).toBe(newAccess)
    expect(b).toBe(newAccess)
    expect(refreshCalls).toBe(1) // concurrent callers share one rotation
    expect(localStorage.getItem('fasttask_refresh')).toBe('rt-new')
  })

  it('ensureFreshAccessToken returns null when the refresh token is rejected (reuse/revoked)', async () => {
    localStorage.setItem('fasttask_refresh', 'rt-bad')
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({}, 401)))
    await expect(ensureFreshAccessToken()).resolves.toBeNull()
    expect(token.get()).toBeNull()
    expect(hasRefreshToken()).toBe(false)
    expect(cacheNames).toEqual([])
  })

  it('retains the session and offline snapshots during a refresh network failure', async () => {
    localStorage.setItem('fasttask_refresh', 'rt-offline')
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    await expect(ensureFreshAccessToken()).resolves.toBeNull()
    expect(localStorage.getItem('fasttask_refresh')).toBe('rt-offline')
    expect(cacheNames).toEqual(['workbox-precache', 'ft-api-user'])
  })

  it('logout clears the local session even when the server call fails', async () => {
    token.set(fakeJwt(nowSec() + 3600))
    localStorage.setItem('fasttask_refresh', 'rt')
    vi.stubGlobal('fetch', vi.fn(async () => jsonResponse({}, 500)))
    await logout()
    expect(token.get()).toBeNull()
    expect(hasRefreshToken()).toBe(false)
    expect(cacheNames).toEqual([])
  })
})
