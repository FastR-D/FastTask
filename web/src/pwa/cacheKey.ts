// Pure helpers for the service worker's per-user offline caching (pwa.md §3.2).
// They live outside sw.ts so they can be unit-tested in jsdom: sw.ts itself runs
// only in the service-worker pipeline and cannot be exercised there.

// AGENT_RE matches the agent transport endpoints. These carry SSE streams and
// must be NetworkOnly — a stream must never enter a Workbox cache (§3.2).
export const AGENT_RE = /^\/api\/v1\/agent\//

// SNAPSHOT_RE matches the per-user read snapshots with network-first reads and
// offline cache fallback: today's plan, the goal list, and a goal's task tree (§3.2, §5).
export const SNAPSHOT_RE = /^\/api\/v1\/(daily-plans\/current|goals(?:\/[^/]+\/task-tree)?)$/

// subjectFromAuth reads the JWT `sub` claim (the user id) from an Authorization
// header WITHOUT verifying the signature — it is only a cache namespace, never an
// authorization decision (the server still authenticates every request). Returns
// 'anon' when the header is absent or undecodable so unauthenticated traffic
// shares one harmless bucket.
export function subjectFromAuth(authHeader: string | null): string {
  if (!authHeader) return 'anon'
  const token = authHeader.replace(/^Bearer\s+/i, '').trim()
  const part = token.split('.')[1]
  if (!part) return 'anon'
  try {
    const b64 = part.replace(/-/g, '+').replace(/_/g, '/')
    const padded = b64 + '='.repeat((4 - (b64.length % 4)) % 4)
    const claims = JSON.parse(atob(padded)) as { sub?: unknown }
    return typeof claims.sub === 'string' && claims.sub.length > 0 ? claims.sub : 'anon'
  } catch {
    return 'anon'
  }
}

// userScopedCacheKey appends a per-user discriminator to a request URL so two
// users on a shared device never read each other's stale snapshots (§3.2: "必须
// 按用户隔离缓存键"). The discriminator is added to the CACHE KEY only; Workbox
// still fetches the original URL, so the server never sees the `_u` parameter.
export function userScopedCacheKey(requestUrl: string, authHeader: string | null): string {
  const url = new URL(requestUrl)
  url.searchParams.set('_u', subjectFromAuth(authHeader))
  return url.toString()
}
