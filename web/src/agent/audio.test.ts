import { describe, expect, it } from 'vitest'
import { extensionForMime, negotiateRecordingMime } from './audio'

describe('negotiateRecordingMime (pwa.md §2 #3, §6)', () => {
  it('prefers webm/opus when supported (desktop Chrome/Firefox)', () => {
    expect(negotiateRecordingMime(m => m === 'audio/webm;codecs=opus'))
      .toEqual({ mime: 'audio/webm;codecs=opus', ext: 'webm' })
  })

  it('falls back to mp4/aac on iOS Safari, which has no webm', () => {
    const supported = new Set(['audio/mp4;codecs=mp4a.40.2', 'audio/mp4'])
    expect(negotiateRecordingMime(m => supported.has(m)))
      .toEqual({ mime: 'audio/mp4;codecs=mp4a.40.2', ext: 'm4a' })
  })

  it('returns null when no candidate is supported', () => {
    expect(negotiateRecordingMime(() => false)).toBeNull()
  })

  it('picks the first supported candidate in preference order', () => {
    const supported = new Set(['audio/webm', 'audio/ogg;codecs=opus'])
    expect(negotiateRecordingMime(m => supported.has(m))?.mime).toBe('audio/webm')
  })
})

describe('extensionForMime', () => {
  it('maps containers to extensions, ignoring codec parameters', () => {
    expect(extensionForMime('audio/webm;codecs=opus')).toBe('webm')
    expect(extensionForMime('audio/mp4;codecs=mp4a.40.2')).toBe('m4a')
    expect(extensionForMime('audio/ogg')).toBe('ogg')
    expect(extensionForMime('audio/mpeg')).toBe('mp3')
    expect(extensionForMime('audio/wav')).toBe('wav')
  })
  it('defaults to webm for an unknown container', () => {
    expect(extensionForMime('audio/unknown')).toBe('webm')
  })
})
