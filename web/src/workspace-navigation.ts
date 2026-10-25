export const FASTRESEARCH_ORIGIN = 'https://fastrd.io'
export const FASTRESEARCH_WORKSPACE = `${FASTRESEARCH_ORIGIN}/workspace`
const officialPages = {
  fastwrite: 'https://fastrd.io/write/', fastread: 'https://fastrd.io/read/',
  fastnews: 'https://47.110.133.67/news/', fastppt: 'https://fastrd.io/fastppt/',
} as const
type OfficialSystem = keyof typeof officialPages

/** No identity or arbitrary redirect data is accepted in saved module links. */
export function workspaceURL(value?: string, _system?: string): string | undefined {
  if (!value || /[\x00-\x1f\x7f\\]/.test(value)) return undefined
  try {
    const url = new URL(value)
    if (!['http:', 'https:'].includes(url.protocol) || !url.hostname || url.username || url.password || url.hash || url.search) return undefined
    return url.href
  } catch { return undefined }
}

export function officialWorkspaceSystem(value: string, system?: string): OfficialSystem | undefined {
  if (!system || !Object.prototype.hasOwnProperty.call(officialPages, system)) return
  const normalized = workspaceURL(value)
  if (!normalized) return
  const url = new URL(normalized), expected = new URL(officialPages[system as OfficialSystem])
  return url.origin === expected.origin && url.pathname.replace(/\/+$/, '') === expected.pathname.replace(/\/+$/, '') ? system as OfficialSystem : undefined
}

type ModulePopup = { closed?: boolean; opener: unknown; location: { replace(url: string): void }; close(): void }
type LaunchRequest = { url: string; system?: string; ownerId: string }
type LaunchDependencies = {
  openPopup: () => ModulePopup | null
  assertOwner: () => void
  readFastTaskIdentity: () => Promise<{id: string; identifier: string}>
  fetchHub: typeof fetch
  signal?: AbortSignal
}
export type WorkspaceLaunchResult = { loginRequired: boolean; notice?: string }

function safeError(error: unknown): string {
  if (error instanceof Error && error.name === 'TimeoutError') return '连接 FastResearch 超时，请检查网络后重试。'
  if (error instanceof TypeError) return '无法连接 FastResearch，请检查网络或门户是否允许当前 FastTask 地址访问。'
  return (error instanceof Error ? error.message : '暂时无法打开模块，请稍后重试。')
    .replace(/\b(Bearer\s+)[A-Za-z0-9._~+\/-]+=*/gi, '$1[已隐藏]')
    .replace(/\b((?:token|ticket|sso|key|session|access_token|refresh_token)\s*[:=]\s*)[^\s,;"'<>&?#]+/gi, '$1[已隐藏]')
}

async function hubError(response: Response): Promise<Error> {
  const payload: unknown = await response.json().catch(() => null)
  const message = payload && typeof payload === 'object' && 'error' in payload && typeof payload.error === 'string' ? payload.error : `FastResearch 请求失败（${response.status}）`
  return new Error(safeError(new Error(message)))
}

function checkedWriteURL(value: unknown): string {
  if (typeof value !== 'string' || /[\x00-\x1f\x7f\\]/.test(value)) throw new Error('FastWrite 返回了无法识别的登录地址。')
  const url = new URL(value), base = new URL(officialPages.fastwrite)
  if (url.origin !== base.origin || !url.pathname.startsWith(base.pathname) || url.username || url.password || url.hash) throw new Error('FastWrite 返回的登录地址与官方模块地址不匹配。')
  return url.href
}

/** Called directly from a click: open the tab before the first async request. */
export async function launchModuleWorkspace(input: LaunchRequest, dependencies: LaunchDependencies): Promise<WorkspaceLaunchResult> {
  const url = workspaceURL(input.url, input.system)
  if (!url) throw new Error('模块地址不可用，请联系管理员检查。')
  dependencies.assertOwner()
  const popup = dependencies.openPopup()
  if (!popup) throw new Error('浏览器阻止了新标签页，请允许弹出窗口后重试。')
  popup.opener = null
  const guard = () => {
    dependencies.signal?.throwIfAborted()
    dependencies.assertOwner()
    if (popup.closed) throw new Error('模块标签页已关闭。')
  }
  const navigate = (destination: string) => { guard(); popup.location.replace(destination) }
  const login = (): WorkspaceLaunchResult => {
    navigate(FASTRESEARCH_WORKSPACE)
    return { loginRequired: true, notice: '门户登录已过期，已打开 FastResearch。请使用个人 Key 登录后，返回 FastTask 重试。' }
  }
  try {
    const system = officialWorkspaceSystem(url, input.system)
    if (!system) { navigate(url); return { loginRequired: false } }
    const { id, identifier } = await dependencies.readFastTaskIdentity()
    guard()
    if (id !== input.ownerId) throw new Error('FastTask 登录账号已变化，请刷新后重试。')
    const options: RequestInit = { credentials: 'include', cache: 'no-store', redirect: 'error', referrerPolicy: 'no-referrer', signal: dependencies.signal }
    const me = await dependencies.fetchHub(`${FASTRESEARCH_ORIGIN}/api/content/me`, options)
    guard()
    if (me.status === 401) return login()
    if (!me.ok) throw await hubError(me)
    // The endpoint also returns a session token. Keep only the account reference.
    const { accountId } = await me.json() as {accountId?: unknown}
    guard()
    if (typeof accountId !== 'string' || !accountId) throw new Error('FastResearch 未返回有效账号信息，请在门户重新登录后重试。')
    if (identifier.startsWith('fastresearch:')) {
      const expected = identifier.slice('fastresearch:'.length)
      if (!expected || typeof accountId !== 'string' || accountId !== expected) throw new Error('当前 FastTask 与 FastResearch 登录的账号不一致。请在门户切换到同一账号后重试。')
    }
    if (system === 'fastwrite') {
      guard()
      const response = await dependencies.fetchHub(`${FASTRESEARCH_ORIGIN}/api/fastwrite/login-url`, { ...options, method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' })
      guard()
      if (response.status === 401) return login()
      if (!response.ok) throw await hubError(response)
      const { url: loginURL } = await response.json() as {url?: unknown}
      const destination = checkedWriteURL(loginURL)
      guard()
      // Cookie identity may change in another tab while the exchange is pending.
      const verification = await dependencies.fetchHub(`${FASTRESEARCH_ORIGIN}/api/content/me`, options)
      guard()
      if (verification.status === 401) return login()
      if (!verification.ok) throw await hubError(verification)
      const { accountId: currentAccount } = await verification.json() as {accountId?: unknown}
      if (currentAccount !== accountId) throw new Error('FastResearch 登录账号已变化，请确认门户账号后重试。')
      navigate(destination)
    } else if (system === 'fastread' || system === 'fastnews') {
      const launch = new URL(`${FASTRESEARCH_ORIGIN}/api/sso/launch`)
      launch.searchParams.set('audience', system === 'fastread' ? 'fast-read' : 'fast-news')
      navigate(launch.href)
    } else navigate(url)
    return { loginRequired: false }
  } catch (error) {
    popup.close()
    throw new Error(safeError(error))
  }
}
