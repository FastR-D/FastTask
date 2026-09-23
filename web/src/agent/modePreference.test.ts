import { afterEach, describe, expect, it } from 'vitest'
import { forcedModeOf, readModePreference, writeModePreference } from './modePreference'

// The stored host preference (doc/harness.md §3.2, §15).
//
// What §3.2 forbids is caching the PROBE answer, because a browser upgrade changes what this machine can
// do and a stored verdict would keep being wrong. A preference is the opposite: an instruction from the
// user, which is exactly what settings are for. These tests hold that line — 'auto' is represented by the
// absence of a setting, and anything this module did not write reads back as 'auto' rather than being
// guessed at.

const KEY = 'fasttask.harness.mode'

afterEach(() => {
  localStorage.clear()
})

describe('the host preference', () => {
  it('defaults to auto, which means "probe and tell me what you picked"', () => {
    expect(readModePreference()).toBe('auto')
    expect(localStorage.getItem(KEY)).toBeNull()
  })

  it('round-trips an explicit choice', () => {
    writeModePreference('wasm')
    expect(localStorage.getItem(KEY)).toBe('wasm')
    expect(readModePreference()).toBe('wasm')

    writeModePreference('sidecar')
    expect(readModePreference()).toBe('sidecar')
  })

  it('represents auto as no setting at all', () => {
    writeModePreference('sidecar')
    writeModePreference('auto')
    expect(localStorage.getItem(KEY)).toBeNull()
    expect(readModePreference()).toBe('auto')
  })

  it('treats a value it did not write as no preference', () => {
    localStorage.setItem(KEY, 'carrier-pigeon')
    expect(readModePreference()).toBe('auto')
    localStorage.setItem(KEY, '')
    expect(readModePreference()).toBe('auto')
  })

  it('ignores a value that is not one of the three', () => {
    writeModePreference('turbo' as never)
    expect(localStorage.getItem(KEY)).toBeNull()
  })

  it('translates a preference into what the probe takes, where auto means probe', () => {
    expect(forcedModeOf('auto')).toBeNull()
    expect(forcedModeOf('wasm')).toBe('wasm')
    expect(forcedModeOf('sidecar')).toBe('sidecar')
  })

  it('keeps working when storage is refused, since the preference is a convenience', () => {
    const throwing = {
      getItem() {
        throw new Error('storage is blocked')
      },
      setItem() {
        throw new Error('storage is blocked')
      },
      removeItem() {
        throw new Error('storage is blocked')
      },
    } as unknown as Storage
    expect(readModePreference(throwing)).toBe('auto')
    expect(() => writeModePreference('wasm', throwing)).not.toThrow()
    // A null storage — the shape safeStorage returns when access threw — behaves the same way.
    expect(readModePreference(null)).toBe('auto')
    expect(() => writeModePreference('sidecar', null)).not.toThrow()
  })
})
