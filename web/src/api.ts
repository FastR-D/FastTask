const API = '/api/v1'
let refreshInFlight: Promise<boolean> | null = null
const REFRESH_KEY = 'fasttask_refresh'

export class ApiError extends Error {
  constructor(public status: number, message: string) { super(message) }
}

export const token = {
	get: () => sessionStorage.getItem('fasttask_access'),
	set: (value: string) => sessionStorage.setItem('fasttask_access', value),
  clear: () => { sessionStorage.removeItem('fasttask_access'); sessionStorage.removeItem(REFRESH_KEY) },
}

async function execute<T>(path: string, init: RequestInit, retry: boolean): Promise<{ data: T; etag: string | null }> {
  const headers = new Headers(init.headers)
  if (!(init.body instanceof FormData)) headers.set('Content-Type', 'application/json')
  const access = token.get()
  if (access) headers.set('Authorization', `Bearer ${access}`)
	const response = await fetch(`${API}${path}`, { ...init, headers })
	if (response.status === 401 && retry && path !== '/auth/login' && path !== '/auth/refresh') {
		refreshInFlight ??= refreshAccess().finally(() => { refreshInFlight = null })
		if (await refreshInFlight) return execute<T>(path, init, false)
		token.clear()
	}
  if (!response.ok) {
    const problem = await response.json().catch(() => ({ detail: response.statusText }))
    throw new ApiError(response.status, problem.detail || problem.title || '请求失败')
  }
  const data = response.status === 204 ? undefined : await response.json()
  return { data: data as T, etag: response.headers.get('ETag') }
}

async function refreshAccess() {
	const refreshToken = sessionStorage.getItem(REFRESH_KEY)
	if (!refreshToken) return false
	const refreshed = await fetch(`${API}/auth/refresh`, { method:'POST', headers:{'Content-Type':'application/json','Idempotency-Key':`browser-${refreshToken.slice(-12)}`}, body:JSON.stringify({refresh_token:refreshToken}) })
	if (!refreshed.ok) return false
	const credentials = await refreshed.json()
	token.set(credentials.access_token)
		sessionStorage.setItem(REFRESH_KEY, credentials.refresh_token)
	return true
}

export function request<T>(path: string, init: RequestInit = {}) { return execute<T>(path, init, true) }

export function idem() { return crypto.randomUUID() }

export async function login(identifier: string, password: string) {
  const { data } = await request<{access_token:string;refresh_token:string;user:unknown}>('/auth/login', { method: 'POST', body: JSON.stringify({ identifier, password }) })
  token.set(data.access_token)
  sessionStorage.setItem(REFRESH_KEY, data.refresh_token)
  return data
}

export async function logout() {
	try { await request('/auth/logout', { method:'POST' }) } catch { /* Local logout must still complete. */ } finally { token.clear() }
}
