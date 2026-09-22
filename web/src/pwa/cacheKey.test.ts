import { describe, expect, it } from 'vitest'
import { AGENT_RE, SNAPSHOT_RE, subjectFromAuth, userScopedCacheKey } from './cacheKey'

// jwt mints an unsigned token whose payload carries the given claims (only the
// `sub` is read, and only as a cache namespace — never for authorization).
function jwt(claims: Record<string, unknown>): string {
  const payload = btoa(JSON.stringify(claims)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return `header.${payload}.sig`
}

describe('subjectFromAuth', () => {
  it('extracts the sub claim from a Bearer header', () => {
    expect(subjectFromAuth(`Bearer ${jwt({ sub: 'user_123' })}`)).toBe('user_123')
  })
  it('returns anon for a missing header', () => {
    expect(subjectFromAuth(null)).toBe('anon')
  })
  it('returns anon for a malformed or empty token', () => {
    expect(subjectFromAuth('Bearer not-a-jwt')).toBe('anon')
    expect(subjectFromAuth('Bearer ')).toBe('anon')
    expect(subjectFromAuth('')).toBe('anon')
  })
  it('returns anon when sub is absent or empty', () => {
    expect(subjectFromAuth(`Bearer ${jwt({ foo: 'bar' })}`)).toBe('anon')
    expect(subjectFromAuth(`Bearer ${jwt({ sub: '' })}`)).toBe('anon')
  })
  it('is case-insensitive about the Bearer scheme', () => {
    expect(subjectFromAuth(`bearer ${jwt({ sub: 'u1' })}`)).toBe('u1')
  })
})

describe('userScopedCacheKey (pwa.md §3.2 per-user isolation)', () => {
  it('appends the user discriminator to the cache key', () => {
    const key = userScopedCacheKey('https://x.test/api/v1/daily-plans/current', `Bearer ${jwt({ sub: 'u1' })}`)
    expect(key).toBe('https://x.test/api/v1/daily-plans/current?_u=u1')
  })
  it('namespaces different users apart on a shared device', () => {
    const url = 'https://x.test/api/v1/goals'
    expect(userScopedCacheKey(url, `Bearer ${jwt({ sub: 'alice' })}`))
      .not.toBe(userScopedCacheKey(url, `Bearer ${jwt({ sub: 'bob' })}`))
  })
  it('preserves existing query params and falls back to anon', () => {
    const key = userScopedCacheKey('https://x.test/api/v1/goals?a=1', null)
    expect(key).toContain('a=1')
    expect(key).toContain('_u=anon')
  })
})

describe('route predicates (pwa.md §3.2)', () => {
  it('matches the agent transport endpoints for explicit NetworkOnly exclusion', () => {
    expect(AGENT_RE.test('/api/v1/agent/commands')).toBe(true)
    expect(AGENT_RE.test('/api/v1/agent/resume')).toBe(true)
    expect(AGENT_RE.test('/api/v1/agent/resume-state')).toBe(true)
    expect(AGENT_RE.test('/api/v1/goals')).toBe(false)
  })
  it('matches exactly the three per-user snapshot routes', () => {
    expect(SNAPSHOT_RE.test('/api/v1/daily-plans/current')).toBe(true)
    expect(SNAPSHOT_RE.test('/api/v1/goals')).toBe(true)
    expect(SNAPSHOT_RE.test('/api/v1/goals/goal_1/task-tree')).toBe(true)
    // Everything else stays NetworkOnly.
    expect(SNAPSHOT_RE.test('/api/v1/goals/goal_1')).toBe(false)
    expect(SNAPSHOT_RE.test('/api/v1/daily-plans')).toBe(false)
    expect(SNAPSHOT_RE.test('/api/v1/agent/commands')).toBe(false)
    expect(SNAPSHOT_RE.test('/api/v1/conversations')).toBe(false)
  })
})
