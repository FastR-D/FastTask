const API = '/api/v1'
const REFRESH_KEY = 'fasttask_refresh'
// Refresh this far ahead of the real expiry so a token never lapses mid-request.
const EXPIRY_SKEW_MS = 60_000

// pwa.md §4 / frontend.md §5 storage model:
//   - access token: memory only, never persisted. An installed PWA re-derives it
//     from the refresh token on cold start (see ensureFreshAccessToken).
//   - refresh token: localStorage, so the session survives a cold start. This
//     widens the XSS exposure window versus sessionStorage; the trade-off is
//     accepted because the CSP (server.go) forbids inline and external script
//     sources and refresh tokens rotate with reuse detection. If the CSP is ever
//     relaxed, revisit this decision.
let accessToken: string | null = null
let accessExpiry = 0 // epoch ms of the access token's exp claim; 0 = unknown/expired

// Single-flight guard so concurrent 401 retries and proactive authHeaders calls
// rotate the refresh token exactly once (frontend.md §5: shared with refreshInFlight).
let refreshInFlight: Promise<boolean> | null = null

export class ApiError extends Error {
  constructor(public status: number, message: string) { super(message) }
}

// decodeExpiry reads the JWT `exp` claim (seconds) without verifying the
// signature — it is only a hint for when to proactively refresh. Any decode
// failure yields 0, which safely forces a refresh attempt.
function decodeExpiry(jwt: string): number {
  try {
    const part = jwt.split('.')[1]
    if (!part) return 0
    const b64 = part.replace(/-/g, '+').replace(/_/g, '/')
    const padded = b64 + '='.repeat((4 - (b64.length % 4)) % 4)
    const claims = JSON.parse(atob(padded))
    return typeof claims.exp === 'number' ? claims.exp * 1000 : 0
  } catch {
    return 0
  }
}

function storeAccess(value: string) {
  accessToken = value
  accessExpiry = decodeExpiry(value)
}

function readRefresh(): string | null {
  try { return localStorage.getItem(REFRESH_KEY) } catch { return null }
}

function writeRefresh(value: string) {
  try { localStorage.setItem(REFRESH_KEY, value) } catch { /* storage unavailable */ }
}

export const token = {
  get: () => accessToken,
  set: storeAccess,
  clear: () => { void clearSession() },
}

// hasRefreshToken reports whether a persisted session exists, so the app can
// attempt a silent restore on cold start instead of flashing the login screen.
export function hasRefreshToken(): boolean {
  return readRefresh() !== null
}

// clearSession drops the in-memory access token, the persisted refresh token and
// every Cache Storage entry. pwa.md §3.2/§4 require this on logout, on a failed
// refresh (which is how the server signals refresh-token reuse / session-family
// revocation), so a shared device never shows the next user the previous user's
// cached data.
export async function clearSession(): Promise<void> {
  accessToken = null
  accessExpiry = 0
  try { localStorage.removeItem(REFRESH_KEY) } catch { /* ignore */ }
  if (typeof caches !== 'undefined') {
    try {
      const keys = await caches.keys()
      await Promise.all(keys.map(key => caches.delete(key)))
    } catch { /* Cache API unavailable */ }
  }
}

// refreshAccess rotates the refresh token. Returns false when there is no session
// or the server rejects the token (expired, reused, revoked).
async function refreshAccess(): Promise<boolean> {
  const refreshToken = readRefresh()
  if (!refreshToken) return false
  try {
    const refreshed = await fetch(`${API}/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Idempotency-Key': `browser-${refreshToken.slice(-12)}` },
      body: JSON.stringify({ refresh_token: refreshToken }),
    })
    if (!refreshed.ok) return false
    const credentials = await refreshed.json()
    storeAccess(credentials.access_token)
    writeRefresh(credentials.refresh_token)
    return true
  } catch {
    return false
  }
}

// singleFlightRefresh dedupes concurrent refreshes through refreshInFlight.
function singleFlightRefresh(): Promise<boolean> {
  refreshInFlight ??= refreshAccess().finally(() => { refreshInFlight = null })
  return refreshInFlight
}

// ensureFreshAccessToken proactively guarantees a valid access token and returns
// it, for requests that do NOT flow through execute()'s reactive 401 retry —
// namely the three agent endpoints driven by assistant-ui (frontend.md §5: the
// only place a token can be refreshed before assistant-ui issues its request).
// Returns null when there is no session to refresh.
export async function ensureFreshAccessToken(): Promise<string | null> {
  if (accessToken && Date.now() < accessExpiry - EXPIRY_SKEW_MS) return accessToken
  if (await singleFlightRefresh()) return accessToken
  return null
}

async function execute<T>(path: string, init: RequestInit, retry: boolean): Promise<{ data: T; etag: string | null }> {
  const headers = new Headers(init.headers)
  if (!(init.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  const access = token.get()
  if (access) headers.set('Authorization', `Bearer ${access}`)
  const response = await fetch(`${API}${path}`, { ...init, headers })
  if (response.status === 401 && retry && path !== '/auth/login' && path !== '/auth/refresh') {
    if (await singleFlightRefresh()) return execute<T>(path, init, false)
    await clearSession()
  }
  if (!response.ok) {
    const problem = await response.json().catch(() => ({ detail: response.statusText }))
    throw new ApiError(response.status, problem.detail || problem.title || '请求失败')
  }
  const data = response.status === 204 ? undefined : await response.json()
  return { data: data as T, etag: response.headers.get('ETag') }
}

export function request<T>(path: string, init: RequestInit = {}) { return execute<T>(path, init, true) }

export function idem() { return crypto.randomUUID() }

export async function login(identifier: string, password: string) {
  const { data } = await request<{access_token:string;refresh_token:string;user:unknown}>('/auth/login', { method: 'POST', body: JSON.stringify({ identifier, password }) })
  storeAccess(data.access_token)
  writeRefresh(data.refresh_token)
  return data
}

export async function logout() {
  try { await request('/auth/logout', { method:'POST' }) } catch { /* Local logout must still complete. */ }
  await clearSession()
}
