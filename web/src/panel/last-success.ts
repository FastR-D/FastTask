import type { NativeDevice, NativeHealth } from './native'
import type { PanelPairing } from './pairing'

type SuccessStorage = Pick<Storage, 'getItem' | 'setItem'>

function successTimestamp(value: unknown): string {
  // An explicit offset prevents old or malformed records adopting the browser's timezone.
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) return ''
  const time = Date.parse(value)
  return Number.isFinite(time) ? new Date(time).toISOString() : ''
}

/** verifiedPairing must come from matchingPanelPairing for the current account. */
export function nativePanelLastSuccess(health: NativeHealth | null, device: NativeDevice | null, verifiedPairing: PanelPairing | undefined): string {
  const config = health?.config
  if (!config || !device || !verifiedPairing || !config.has_token
    || config.pairing_id !== verifiedPairing.id || config.device_name !== verifiedPairing.device_name
    || config.device_name !== device.name || !config.address
    || config.address.toUpperCase() !== device.address.toUpperCase()) return ''
  // Neither a successful poll nor a queued refresh is a screen acknowledgement.
  return successTimestamp(health?.monitor?.last_synced_at)
}

function browserSuccessKey(origin: string, userId: string, deviceId: string): string {
  if (!userId || !deviceId || deviceId.length > 256 || /[\x00-\x1f\x7f]/.test(deviceId)) return ''
  try { return `fasttask_panel_success_v1:${encodeURIComponent(new URL(origin).origin)}:${encodeURIComponent(userId)}:${encodeURIComponent(deviceId)}` }
  catch { return '' }
}

export function loadBrowserPanelSuccess(origin: string, userId: string, deviceId: string, storage?: SuccessStorage): string {
  const key = browserSuccessKey(origin, userId, deviceId)
  if (!key) return ''
  try { return successTimestamp((storage ?? localStorage).getItem(key)) }
  catch { return '' }
}

/** Call only after sendImage resolves with the screen's completion acknowledgement. */
export function rememberBrowserPanelSuccess(origin: string, userId: string, deviceId: string, acknowledgedAt: string, storage?: SuccessStorage): string {
  const key = browserSuccessKey(origin, userId, deviceId), timestamp = successTimestamp(acknowledgedAt)
  if (!key || !timestamp) return ''
  try { (storage ?? localStorage).setItem(key, timestamp) }
  catch { /* Keep showing this session's confirmed success if storage is unavailable. */ }
  return timestamp
}

export function formatPanelLastSuccess(timestamp: string, timezone: string): string {
  const valid = successTimestamp(timestamp)
  if (!valid) return ''
  const options: Intl.DateTimeFormatOptions = { timeZone: timezone || 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' }
  let formatter: Intl.DateTimeFormat
  try { formatter = new Intl.DateTimeFormat('en-GB', options) }
  catch { formatter = new Intl.DateTimeFormat('en-GB', { ...options, timeZone: 'Asia/Shanghai' }) }
  const parts = formatter.formatToParts(new Date(valid))
  const value = (type: string) => parts.find(part => part.type === type)?.value
  return `${value('year')}-${value('month')}-${value('day')} ${value('hour')}:${value('minute')}:${value('second')}`
}
