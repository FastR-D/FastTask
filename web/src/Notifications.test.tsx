import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { NotificationAdmin } from './Notifications'
import type { NotificationChannel, NotificationMessage, NotificationTarget, User } from './types'

// The notification console's contract, as a browser would see it: the four providers
// share one form, and nothing that is a credential — a bot token, a device key, an FCM
// registration token — is ever rendered, because the server does not send one back.

const TELEGRAM_TOKEN = '123456789:AA-super-secret-token'
const CHAT_ID = '-1009876543210'

const channel: NotificationChannel = {
  id: 'notify_channel_1', name: '研究群机器人', provider: 'telegram', endpoint: 'https://telegram.example.test',
  settings: { api_base: 'https://telegram.example.test' }, secret_hint: '1234...oken', secret_set: true,
  status: 'active', last_check_status: 'ok', last_check_at: '2026-09-24T02:00:00Z', last_error: '',
  target_count: 2, active_target_count: 1, revision: 3,
  created_at: '2026-09-20T02:00:00Z', updated_at: '2026-09-24T02:00:00Z',
}

const target: NotificationTarget = {
  id: 'notify_target_1', user_id: 'user_1', user_identifier: 'researcher', user_display_name: '研究员',
  channel_id: 'notify_channel_1', channel_name: '研究群机器人', provider: 'telegram', label: '张三的 iPhone',
  address_hint: '…7890', status: 'active', failure_count: 0, last_error: '', last_sent_at: '2026-09-24T02:05:00Z',
  revision: 1, created_at: '2026-09-21T02:00:00Z', updated_at: '2026-09-24T02:05:00Z',
}

const message: NotificationMessage = {
  id: 'notify_msg_1', user_id: 'user_1', user_identifier: 'researcher', user_display_name: '研究员',
  channel_id: 'notify_channel_1', channel_name: '研究群机器人', provider: 'telegram', target_id: 'notify_target_1',
  target_hint: '…3210', topic: 'proposal.pending', title: '有待确认的任务树变更', body: '拆解《论文初稿》', url: '',
  payload_json: '{"collapse_id":"proposal-1"}', status: 'sent', attempts: 1, max_attempts: 3,
  provider_message_id: '4217', last_error: '', run_after: '2026-09-24T02:05:00Z',
  created_at: '2026-09-24T02:05:00Z', updated_at: '2026-09-24T02:05:01Z', sent_at: '2026-09-24T02:05:01Z',
}

const admin: User = {
  id: 'user_admin', identifier: 'admin', display_name: '管理员', timezone: 'Asia/Shanghai', locale: 'zh-CN',
  role: 'admin', status: 'active', revision: 1, created_at: '2026-09-20T02:00:00Z',
}
const member: User = { ...admin, id: 'user_1', identifier: 'researcher', display_name: '研究员', role: 'member' }

type Call = { url: string; method: string; body: unknown; headers: Record<string, string> }

function jsonResponse(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300, status, statusText: status === 200 ? 'OK' : 'ERROR',
    headers: { get: () => null }, json: async () => body,
  } as unknown as Response
}

// installFetch routes by URL substring and records every call, so a test can assert both
// what the console showed and what it actually sent.
function installFetch(routes: Record<string, unknown>, overrides: Record<string, unknown> = {}) {
  const calls: Call[] = []
  const all = { ...routes, ...overrides }
  // Longest key first: /admin/notifications/channels is a prefix of
  // /admin/notifications/channels/{id}/verification, and the specific route must win.
  const keys = Object.keys(all).sort((a, b) => b.length - a.length)
  vi.stubGlobal('fetch', vi.fn(async (url: unknown, init?: RequestInit) => {
    const text = String(url)
    calls.push({
      url: text, method: init?.method ?? 'GET', headers: Object.fromEntries(new Headers(init?.headers).entries()),
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
    })
    for (const key of keys) {
      if (text.includes(key)) return jsonResponse(all[key])
    }
    return jsonResponse({ items: [] })
  }))
  return calls
}

function defaultRoutes(overrides: Record<string, unknown> = {}) {
  return installFetch({
    '/admin/notifications/channels': { items: [channel] },
    '/admin/notifications/targets': { items: [target] },
    '/admin/notifications/messages': { items: [message] },
    '/admin/users': { items: [admin, member] },
  }, overrides)
}

function field(label: string) {
  const element = document.querySelector(`mdui-text-field[label="${label}"]`)
  if (!element) throw new Error(`no text field labelled ${label}`)
  return element as HTMLElement
}

function select(label: string) {
  const element = document.querySelector(`mdui-select[label="${label}"]`)
  if (!element) throw new Error(`no select labelled ${label}`)
  return element as HTMLElement
}

// mdui components are custom elements and are deliberately not loaded in tests, so there
// is no native value setter for fireEvent to use. Setting the property and dispatching the
// same event the component fires is what the console sees (agent/HarnessStatus.test.tsx).
function type(label: string, value: string) {
  const element = field(label) as HTMLElement & { value?: string }
  element.value = value
  act(() => { element.dispatchEvent(new Event('change', { bubbles: true })) })
}

function choose(label: string, value: string) {
  const element = select(label) as HTMLElement & { value?: string }
  element.value = value
  act(() => { element.dispatchEvent(new Event('change', { bubbles: true })) })
}

// The channel name appears in the card, in the recipients header and on every log row, so
// the card is found by its heading rather than by text.
async function channelCard() {
  return screen.findByRole('heading', { name: '研究群机器人' })
}

// logPanel is the delivery-log half of the console. Its status words also exist as options
// in the filter select, so assertions about a delivery have to be scoped to the rows.
function logPanel() {
  const panels = document.querySelectorAll<HTMLElement>('section.notify-panel')
  const panel = panels[panels.length - 1]
  if (!panel) throw new Error('the delivery log did not render')
  return within(panel)
}

// A button is found by its own label, not by the text it contains: 接收端 is also the
// recipients panel's heading and part of a channel card's caption.
function button(name: string) {
  const element = Array.from(document.querySelectorAll('mdui-button')).find(item => item.textContent?.trim() === name)
  if (!element) throw new Error(`no button named ${name}`)
  return element as HTMLElement
}

// 停用 appears on the channel card and on every recipient row, so a row action has to be
// found inside its row.
function rowButton(rowText: string, name: string) {
  const row = Array.from(document.querySelectorAll('.notify-rows li')).find(item => item.textContent?.includes(rowText))
  if (!row) throw new Error(`no row containing ${rowText}`)
  const element = Array.from(row.querySelectorAll('mdui-button')).find(item => item.textContent?.trim() === name)
  if (!element) throw new Error(`no button named ${name} in the row ${rowText}`)
  return element as HTMLElement
}

// mdui-dialog is not upgraded in tests, so its headline stays an attribute rather than
// becoming text. The dialog is asserted by the attribute a person would read.
function dialog(headline: string) {
  const element = document.querySelector(`mdui-dialog[headline="${headline}"]`)
  if (!element) throw new Error(`no dialog headed ${headline}`)
  return element
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

describe('NotificationAdmin', () => {
  it('shows the channels, their reach and the delivery log', async () => {
    defaultRoutes()
    render(<NotificationAdmin onNotice={vi.fn()} />)

    expect(await channelCard()).toBeInTheDocument()
    expect(screen.getByText('Telegram 机器人 · 启用')).toBeInTheDocument()
    // Reach is what tells an operator whether a channel is worth keeping.
    expect(screen.getByText(/接收端 1\/2/)).toBeInTheDocument()
    expect(screen.getByText(/凭据 1234\.\.\.oken/)).toBeInTheDocument()
    expect(screen.getByText(/最近验证：通过/)).toBeInTheDocument()

    const log = logPanel()
    expect(log.getByText('有待确认的任务树变更')).toBeInTheDocument()
    expect(log.getByText('已送达')).toBeInTheDocument()
    expect(log.getByText(/proposal\.pending · 第 1\/3 次 · 4217/)).toBeInTheDocument()
  })

  it('never renders a credential or a full address', async () => {
    defaultRoutes()
    render(<NotificationAdmin onNotice={vi.fn()} />)
    await channelCard()
    // The recipient row is loaded with the channel panel; open it so its address is on screen.
    fireEvent.click(button('接收端'))
    await screen.findByText('张三的 iPhone')

    const text = document.body.textContent ?? ''
    expect(text).not.toContain(TELEGRAM_TOKEN)
    expect(text).not.toContain(CHAT_ID)
    expect(text).toContain('…3210')
  })

  it('drives the form from the chosen provider', async () => {
    defaultRoutes()
    render(<NotificationAdmin onNotice={vi.fn()} />)
    await channelCard()

    // Telegram needs one token and nothing else.
    expect(field('Bot Token')).toBeInTheDocument()
    expect(document.querySelector('mdui-text-field[label="Key ID"]')).toBeNull()

    choose('推送方式', 'apns')
    await waitFor(() => expect(field('Key ID')).toBeInTheDocument())
    expect(field('Team ID')).toBeInTheDocument()
    expect(field('Bundle ID（apns-topic）')).toBeInTheDocument()
    expect(select('环境')).toBeInTheDocument()
    // A .p8 is a multi-line PEM, so the field has to accept one.
    expect(field('.p8 私钥（PEM 全文）').getAttribute('rows')).toBe('6')

    // APNs refuses to build without its three identifiers, so the console must not offer
    // to submit until they are there.
    type('通道名称', '苹果推送')
    expect(button('创建通道')).toHaveAttribute('disabled')
    type('.p8 私钥（PEM 全文）', '-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----')
    type('Key ID', 'KEYID12345')
    type('Team ID', 'TEAMID67890')
    type('Bundle ID（apns-topic）', 'com.fasttask.app')
    expect(button('创建通道')).not.toHaveAttribute('disabled')
  })

  it('creates a channel with the provider settings the form collected', async () => {
    const calls = defaultRoutes({ '/admin/notifications/channels': { items: [] } })
    render(<NotificationAdmin onNotice={vi.fn()} />)
    await waitFor(() => expect(calls.some(call => call.url.includes('/admin/notifications/channels'))).toBe(true))

    choose('推送方式', 'bark')
    await waitFor(() => expect(field('Bark 服务器地址')).toBeInTheDocument())
    type('通道名称', 'Bark 推送')
    type('Bark 服务器地址', 'https://bark.example.test')
    choose('提醒级别', 'timeSensitive')
    type('分组名（默认 FastTask）', 'FastTask 研究')
    fireEvent.click(button('创建通道'))

    await waitFor(() => {
      const created = calls.find(call => call.method === 'POST' && call.url.endsWith('/admin/notifications/channels'))
      expect(created).toBeTruthy()
      expect(created?.body).toEqual({
        name: 'Bark 推送', provider: 'bark', endpoint: 'https://bark.example.test',
        settings: { level: 'timeSensitive', group: 'FastTask 研究' }, secret: '', status: 'active',
      })
      expect(created?.headers['idempotency-key']).toBeTruthy()
    })
  })

  it('verifies credentials and reports the provider diagnosis', async () => {
    const onNotice = vi.fn()
    defaultRoutes({
      '/admin/notifications/channels/notify_channel_1/verification': {
        verified: false, provider: 'telegram', channel: '研究群机器人', detail: 'telegram: provider answered 401: Unauthorized', checked_at: '2026-09-24T03:00:00Z',
      },
    })
    render(<NotificationAdmin onNotice={onNotice} />)
    await channelCard()

    fireEvent.click(button('验证凭据'))
    await waitFor(() => expect(onNotice).toHaveBeenCalledWith('telegram: provider answered 401: Unauthorized'))
  })

  it('sends a test to an address without storing it', async () => {
    const calls = defaultRoutes({
      '/admin/notifications/channels/notify_channel_1/test': { delivered: true, provider: 'telegram', channel: '研究群机器人', provider_message_id: '4218' },
    })
    const onNotice = vi.fn()
    render(<NotificationAdmin onNotice={onNotice} />)
    await channelCard()

    fireEvent.click(button('试发'))
    await waitFor(() => expect(dialog('发送测试通知')).toBeInTheDocument())
    type('Chat ID', CHAT_ID)
    fireEvent.click(button('发送'))

    await waitFor(() => {
      const test = calls.find(call => call.method === 'POST' && call.url.endsWith('/test'))
      expect(test?.body).toEqual({ address: CHAT_ID })
    })
    await waitFor(() => expect(onNotice).toHaveBeenCalledWith('测试消息已送达'))
    // The address was typed into a dialog and posted; it is not part of what the console shows.
    expect(document.body.textContent ?? '').not.toContain(CHAT_ID)
  })

  it('drains the queue and broadcasts on request', async () => {
    const calls = defaultRoutes({
      '/admin/notifications/dispatch': { claimed: 3, sent: 2, retried: 1, failed: 0, retired: 0 },
      '/admin/notifications/broadcast': { queued: 5 },
    })
    const onNotice = vi.fn()
    render(<NotificationAdmin onNotice={onNotice} />)
    await channelCard()

    fireEvent.click(button('立即投递'))
    await waitFor(() => expect(onNotice).toHaveBeenCalledWith('本次投递：送达 2，重试 1，失败 0'))
    expect(calls.some(call => call.method === 'POST' && call.url.endsWith('/admin/notifications/dispatch'))).toBe(true)

    fireEvent.click(button('广播'))
    await waitFor(() => expect(dialog('广播通知')).toBeInTheDocument())
    type('标题', '维护通知')
    type('正文', '今晚 23:00 停机十分钟')
    fireEvent.click(button('发送'))
    await waitFor(() => {
      const broadcast = calls.find(call => call.method === 'POST' && call.url.endsWith('/admin/notifications/broadcast'))
      expect(broadcast?.body).toMatchObject({ title: '维护通知', body: '今晚 23:00 停机十分钟', topic: 'notification.broadcast' })
    })
  })

  it('registers a recipient for a user and can retire one', async () => {
    const calls = defaultRoutes()
    const onNotice = vi.fn()
    render(<NotificationAdmin onNotice={onNotice} />)
    await channelCard()

    fireEvent.click(button('接收端'))
    expect(await screen.findByText('张三的 iPhone')).toBeInTheDocument()

    choose('用户', 'user_1')
    type('Chat ID', CHAT_ID)
    type('备注（例如：张三的 iPhone）', '研究室 iPad')
    fireEvent.click(button('添加'))
    await waitFor(() => {
      const created = calls.find(call => call.method === 'POST' && call.url.endsWith('/admin/notifications/targets'))
      expect(created?.body).toEqual({ user_id: 'user_1', channel_id: 'notify_channel_1', address: CHAT_ID, label: '研究室 iPad' })
    })
    await waitFor(() => expect(onNotice).toHaveBeenCalledWith('接收端已添加'))

    // Retiring a device is a status change with the revision it was read at, so two
    // administrators cannot silently overwrite each other.
    fireEvent.click(rowButton('张三的 iPhone', '停用'))
    await waitFor(() => {
      const patched = calls.find(call => call.method === 'PATCH' && call.url.includes('/admin/notifications/targets/notify_target_1'))
      expect(patched?.body).toEqual({ status: 'disabled' })
      expect(patched?.headers['if-match']).toBe('"notify_target_notify_target_1_rev_1"')
    })
  })

  it('shows why a delivery failed and lets the operator narrow the log', async () => {
    const calls = defaultRoutes({
      '/admin/notifications/messages': {
        items: [{
          ...message, id: 'notify_msg_2', status: 'failed', attempts: 3, provider_message_id: '',
          last_error: 'telegram: provider answered 403: Forbidden: bot was blocked by the user',
        }],
      },
    })
    render(<NotificationAdmin onNotice={vi.fn()} />)

    const log = logPanel()
    expect(await log.findByText('失败')).toBeInTheDocument()
    expect(log.getByText('telegram: provider answered 403: Forbidden: bot was blocked by the user')).toBeInTheDocument()

    choose('投递记录筛选', 'sent')
    await waitFor(() => expect(calls.some(call => call.url.includes('/admin/notifications/messages?status=sent'))).toBe(true))
  })
})
