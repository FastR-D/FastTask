import { describe, expect, it } from 'vitest'
import { localDateInTimezone } from './date'

describe('localDateInTimezone', () => {
  it('uses the account day across UTC date boundaries', () => {
    const instant = new Date('2026-09-22T00:30:00Z')
    expect(localDateInTimezone(instant, 'America/Los_Angeles')).toBe('2026-09-21')
    expect(localDateInTimezone(instant, 'Asia/Singapore')).toBe('2026-09-22')
  })
})
