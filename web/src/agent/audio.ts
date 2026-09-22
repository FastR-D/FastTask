// Recording-format negotiation (pwa.md §2 #3, §6; frontend.md §4.2). The old code
// hardcoded audio/webm + voice.webm, which breaks on iOS Safari — its
// MediaRecorder only produces mp4/aac. We negotiate a container the browser
// actually supports and send the real MIME + a matching extension to the backend.

export type RecordingFormat = { mime: string; ext: string }

// Preference order: webm/opus (desktop Chrome/Firefox), then mp4/aac (iOS Safari),
// then ogg/opus, then bare containers as a last resort.
const CANDIDATES: RecordingFormat[] = [
  { mime: 'audio/webm;codecs=opus', ext: 'webm' },
  { mime: 'audio/webm', ext: 'webm' },
  { mime: 'audio/mp4;codecs=mp4a.40.2', ext: 'm4a' },
  { mime: 'audio/mp4', ext: 'm4a' },
  { mime: 'audio/ogg;codecs=opus', ext: 'ogg' },
]

// negotiateRecordingMime returns the first candidate the browser supports, or null
// when none is supported (the caller surfaces "unsupported" rather than recording
// a container the backend cannot transcribe). It takes the isSupported predicate so
// it is pure and unit-testable without a real MediaRecorder.
export function negotiateRecordingMime(isSupported: (mime: string) => boolean): RecordingFormat | null {
  for (const candidate of CANDIDATES) {
    if (isSupported(candidate.mime)) return candidate
  }
  return null
}

// extensionForMime derives a filename extension from a (possibly parameterized)
// MIME type, for the uploaded blob's filename.
export function extensionForMime(mime: string): string {
  const base = mime.split(';')[0].trim().toLowerCase()
  switch (base) {
    case 'audio/webm':
      return 'webm'
    case 'audio/mp4':
      return 'm4a'
    case 'audio/ogg':
      return 'ogg'
    case 'audio/mpeg':
      return 'mp3'
    case 'audio/wav':
    case 'audio/wave':
    case 'audio/x-wav':
      return 'wav'
    default:
      return 'webm'
  }
}
