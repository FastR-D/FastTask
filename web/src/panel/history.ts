export type RememberedPanelDevice = {
  transport: 'native' | 'browser'
  name: string
  address?: string
  id?: string
  last_used_at: string
}
type DeviceInput = Omit<RememberedPanelDevice, 'last_used_at'>
type DeviceStorage = Pick<Storage, 'getItem' | 'setItem'>
const MAX_DEVICES = 10

function storageKey(origin: string, userId: string) {
  if (!userId) return ''
  try { return `fasttask_panel_devices_v1:${encodeURIComponent(new URL(origin).origin)}:${encodeURIComponent(userId)}` }
  catch { return '' }
}

/** Retain only device metadata; credentials and profile fields are never copied. */
function sanitizeDevice(value: unknown): RememberedPanelDevice | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return
  const record = value as Record<string, unknown>
  if (typeof record.name !== 'string' || !/^WL[A-Z0-9_-]{1,32}$/i.test(record.name)) return
  if (typeof record.last_used_at !== 'string' || !Number.isFinite(Date.parse(record.last_used_at))) return
  const base = { name: record.name.toUpperCase(), last_used_at: new Date(record.last_used_at).toISOString() }
  if (record.transport === 'native' && typeof record.address === 'string' && /^(?:[0-9A-F]{2}:){5}[0-9A-F]{2}$/i.test(record.address)) return { ...base, transport: 'native', address: record.address.toUpperCase() }
  if (record.transport === 'browser' && typeof record.id === 'string' && record.id.length > 0 && record.id.length <= 256 && !/[\x00-\x1f\x7f]/.test(record.id)) return { ...base, transport: 'browser', id: record.id }
}

export function panelDeviceKey(device: Pick<RememberedPanelDevice, 'transport' | 'address' | 'id'>) { return `${device.transport}:${device.transport === 'native' ? device.address?.toUpperCase() : device.id}` }

export function loadPanelDeviceHistory(origin: string, userId: string, storage?: DeviceStorage): RememberedPanelDevice[] {
  const key = storageKey(origin, userId)
  if (!key) return []
  try {
    const values: unknown = JSON.parse((storage ?? localStorage).getItem(key) || '[]')
    if (!Array.isArray(values)) return []
    const unique = new Set<string>()
    return values.map(sanitizeDevice).filter((value): value is RememberedPanelDevice => Boolean(value))
      .sort((left, right) => Date.parse(right.last_used_at) - Date.parse(left.last_used_at))
      .filter(value => { const id = panelDeviceKey(value); if (unique.has(id)) return false; unique.add(id); return true })
      .slice(0, MAX_DEVICES)
  } catch { return [] }
}

export function rememberPanelDevice(origin: string, userId: string, device: DeviceInput, storage?: DeviceStorage, now = new Date()): RememberedPanelDevice[] {
  const previous = loadPanelDeviceHistory(origin, userId, storage), key = storageKey(origin, userId)
  const clean = sanitizeDevice({ ...device, last_used_at: now.toISOString() })
  if (!key || !clean) return previous
  const next = [clean, ...previous.filter(value => panelDeviceKey(value) !== panelDeviceKey(clean))].slice(0, MAX_DEVICES)
  try { (storage ?? localStorage).setItem(key, JSON.stringify(next)) } catch { /* Private browsing still permits this session's device selection. */ }
  return next
}
