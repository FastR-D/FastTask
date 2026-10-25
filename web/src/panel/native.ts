export const NATIVE_PANEL_URL = 'http://127.0.0.1:19743'
export type NativeDevice = { name: string; address: string; rssi?: number }
export type NativeConfig = { enabled: boolean; device_name: string; address?: string; page: number; has_token: boolean; fasttask_url?: string; pairing_id?: string }
export type NativeHealth = { ok: boolean; version: string; ble_supported: boolean; busy?: boolean; config: NativeConfig; last_status?: { status?: string; operation?: string; message?: string; started_at?: string; finished_at?: string } | null; next_run?: string | null }

export type LocalNetworkPermission = PermissionState | 'unknown'
type LoopbackRequestInit = RequestInit & { targetAddressSpace: 'loopback' }

/** These permissions are not yet present in every browser's TypeScript/DOM API. */
export async function localNetworkPermission(): Promise<LocalNetworkPermission> {
  if (!navigator.permissions?.query) return 'unknown'
  for (const name of ['loopback-network', 'local-network-access']) {
    try { return (await navigator.permissions.query({ name } as PermissionDescriptor)).state }
    catch { /* Older browsers reject permission names they do not implement. */ }
  }
  return 'unknown'
}

export function isLoopbackPanelPage(origin: string): boolean {
  try { return ['127.0.0.1', 'localhost', '[::1]'].includes(new URL(origin).hostname) }
  catch { return false }
}

export function nativeConfigMatchesOrigin(config: NativeConfig, origin: string): boolean {
  try {
    const url = new URL(config.fasttask_url || '')
    return url.origin === origin && url.pathname === '/' && !url.search && !url.hash && !url.username && !url.password
  } catch { return false }
}

export function nativeStartCommand(origin: string): string {
  // PowerShell single-quoted strings escape a literal quote by doubling it.
  return `powershell.exe -NoProfile -ExecutionPolicy Bypass -File ".\\windows-panel\\start.ps1" -Install -Origin '${origin.replaceAll("'", "''")}'`
}

export class NativePanelError extends Error {
  readonly code: string
  constructor(message: string, code: string) { super(message); this.name = 'NativePanelError'; this.code = code }
}

export async function nativePanel<T>(path: string, body?: object, method?: string): Promise<T> {
  let response: Response
  const options: LoopbackRequestInit = {
    method: method || (body ? 'POST' : 'GET'),
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body ? JSON.stringify(body) : undefined,
    signal: AbortSignal.timeout(path === '/send' || path === '/refresh' ? 320_000 : path === '/scan' ? 45_000 : path === '/config' ? 30_000 : 20_000),
    cache: 'no-store',
    credentials: 'omit',
    redirect: 'error',
    referrerPolicy: 'no-referrer',
    targetAddressSpace: 'loopback',
  }
  try { response = await fetch(`${NATIVE_PANEL_URL}${path}`, options) } catch (error) {
    if ((path === '/send' || path === '/refresh') && error instanceof DOMException && ['AbortError', 'TimeoutError'].includes(error.name)) throw new Error('本地等待已超时，墨水屏操作仍可能执行。请查询助手状态，勿重复提交。')
    throw new NativePanelError('助手未启动、启动来源不匹配，或本地网络访问被拒绝。请按“连接帮助”核对后重新检测。', 'unreachable')
  }
  const parsed: unknown = await response.json().catch(() => null)
  const data = parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed as Record<string, unknown> : {}
  if (!response.ok || data.ok === false) {
    const code = typeof data.error === 'string' ? data.error : 'request_failed'
    const message = code === 'invalid_origin' ? 'Windows 助手的启动来源与当前页面不匹配。请关闭旧助手，使用下方当前页面的启动命令重新运行，再在此账号重新授权。' : typeof data.message === 'string' ? data.message : `Windows 蓝牙助手请求失败 (${response.status})，请重新检测助手状态。`
    throw new NativePanelError(message, code)
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new NativePanelError('助手返回的状态无法识别，请重新启动 Windows 助手。', 'invalid_response')
  return data as T
}
