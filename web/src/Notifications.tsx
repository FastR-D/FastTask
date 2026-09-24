import { useEffect, useRef, useState } from 'react'
import { idem, request, resourceETag } from './api'
import { fieldValue, useMduiEvent } from './mdui-react'
import type { NotificationChannel, NotificationCheck, NotificationDelivery, NotificationDispatch, NotificationMessage, NotificationTarget, User } from './types'

// The notification console (doc/notification.md §9). One screen, three jobs, in the
// order an operator does them: make a channel exist, point it at people, and see what
// actually arrived. Everything here is the administrator's half; a user manages their
// own subscriptions from their own settings, through the same API.
//
// Two rules shape the surface:
//   - A credential is written once and never read back. The form has no value to show
//     after a save, only the masked hint the server returns, so "编辑" cannot leak one
//     and cannot silently erase one either: leaving the field empty keeps what is stored.
//   - An address is never displayed in full either. For FCM and APNs it is a device
//     credential, so rows carry the server's masked hint plus the label a person typed.

const PROVIDER_LABEL: Record<string, string> = {
  telegram: 'Telegram 机器人', bark: 'Bark（iOS）', fcm: 'Firebase Cloud Messaging', apns: 'APNs（Apple 推送）',
}
const CHANNEL_STATUS: Record<string, string> = { active: '启用', disabled: '已停用' }
const TARGET_STATUS: Record<string, string> = { active: '在用', disabled: '已停用', invalid: '已失效' }
const MESSAGE_STATUS: Record<string, string> = { queued: '待发送', sending: '发送中', sent: '已送达', failed: '失败' }
const CHECK_STATUS: Record<string, string> = { ok: '通过', failed: '未通过', '': '未验证' }

type SettingField = { key: string; label: string; required?: boolean; options?: string[] }

// ProviderShape is the per-provider description of the form. The four providers differ in
// what they need, and none of that difference belongs in JSX: one table drives the create
// form, the edit form, the "add a recipient" placeholder and the test-send dialog, so a
// fifth provider is a table entry rather than a fourth copy of a form.
type ProviderShape = {
  secretLabel: string
  secretRequired: boolean
  secretRows?: number
  endpointLabel: string
  endpointRequired: boolean
  settings: SettingField[]
  addressLabel: string
  addressHint: string
  note: string
}

const PROVIDERS: Record<string, ProviderShape> = {
  telegram: {
    secretLabel: 'Bot Token', secretRequired: true,
    endpointLabel: 'API 地址（默认 https://api.telegram.org）', endpointRequired: false,
    settings: [],
    addressLabel: 'Chat ID', addressHint: '群组或频道 ID，例如 -1001234567890，或 @username',
    note: '先把机器人拉进群或频道，它才能发言；对方 ID 用 @userinfobot 或群管理页取。',
  },
  bark: {
    secretLabel: '', secretRequired: false,
    endpointLabel: 'Bark 服务器地址', endpointRequired: true,
    settings: [
      { key: 'group', label: '分组名（默认 FastTask）' },
      { key: 'sound', label: '提示音（可选）' },
      { key: 'level', label: '提醒级别', options: ['active', 'timeSensitive', 'passive', 'critical'] },
      { key: 'icon', label: '图标 URL（可选）' },
    ],
    addressLabel: 'Device Key', addressHint: 'Bark App 推送 URL 里的设备密钥',
    note: 'Bark 不需要服务端凭据：设备密钥就是权限，所以它按接收端保存，不按通道保存。',
  },
  fcm: {
    secretLabel: '服务账号 JSON', secretRequired: true, secretRows: 6,
    endpointLabel: 'API 地址（默认 https://fcm.googleapis.com）', endpointRequired: false,
    settings: [{ key: 'project_id', label: '项目 ID（留空则取 JSON 内的值）' }],
    addressLabel: 'FCM 注册 Token', addressHint: '客户端 Firebase Messaging 颁发的 registration token',
    note: 'Firebase 控制台 → 项目设置 → 服务账号 → 生成新的私钥，整份 JSON 粘贴到这里。',
  },
  apns: {
    secretLabel: '.p8 私钥（PEM 全文）', secretRequired: true, secretRows: 6,
    endpointLabel: 'API 地址（留空按环境自动选择）', endpointRequired: false,
    settings: [
      { key: 'key_id', label: 'Key ID', required: true },
      { key: 'team_id', label: 'Team ID', required: true },
      { key: 'topic', label: 'Bundle ID（apns-topic）', required: true },
      { key: 'environment', label: '环境', options: ['production', 'sandbox'] },
      { key: 'sound', label: '提示音（默认 default）' },
    ],
    addressLabel: 'Device Token', addressHint: '客户端 APNs 颁发的设备 token（十六进制）',
    note: 'Certificates, Identifiers & Profiles → Keys 里创建，.p8 只能下载一次。',
  },
}

const PROVIDER_ORDER = ['telegram', 'bark', 'fcm', 'apns']

function stamp(value?: string) { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '' }
function errorText(error: unknown) { return error instanceof Error ? error.message : '操作失败' }

// emptyDraft is the create form's blank state. Settings start empty so an untouched
// optional field is not submitted as an empty string the server would have to strip.
function emptyDraft() { return { name: '', endpoint: '', secret: '', status: 'active', settings: {} as Record<string, string> } }

export function NotificationAdmin({ onNotice }: { onNotice: (value: string) => void }) {
  const [channels, setChannels] = useState<NotificationChannel[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [provider, setProvider] = useState('telegram')
  const [draft, setDraft] = useState(emptyDraft())
  const [editing, setEditing] = useState<NotificationChannel | null>(null)
  const [editSecret, setEditSecret] = useState('')
  const [channelID, setChannelID] = useState('')
  const [targets, setTargets] = useState<NotificationTarget[]>([])
  const [messages, setMessages] = useState<NotificationMessage[]>([])
  const [messageStatus, setMessageStatus] = useState('')
  const [busy, setBusy] = useState('')
  const [recipient, setRecipient] = useState({ user_id: '', address: '', label: '' })
  const [testFor, setTestFor] = useState<{ channel?: NotificationChannel; target?: NotificationTarget } | null>(null)
  const [testAddress, setTestAddress] = useState('')
  const [broadcast, setBroadcast] = useState<{ channel_id: string; title: string; body: string } | null>(null)
  const dialogRef = useRef<HTMLElement>(null)
  useMduiEvent(dialogRef, 'close', () => { setTestFor(null); setBroadcast(null) })

  const shape = PROVIDERS[provider] ?? PROVIDERS.telegram

  async function loadChannels() {
    const { data } = await request<{ items: NotificationChannel[] }>('/admin/notifications/channels')
    setChannels(data.items ?? [])
  }
  async function loadUsers() {
    const { data } = await request<{ items: User[] }>('/admin/users')
    setUsers(data.items ?? [])
  }
  async function loadTargets(id = channelID) {
    if (!id) { setTargets([]); return }
    const { data } = await request<{ items: NotificationTarget[] }>('/admin/notifications/targets?channel_id=' + encodeURIComponent(id))
    setTargets(data.items ?? [])
  }
  async function loadMessages(status = messageStatus) {
    const query = status ? '?status=' + encodeURIComponent(status) + '&limit=50' : '?limit=50'
    const { data } = await request<{ items: NotificationMessage[] }>('/admin/notifications/messages' + query)
    setMessages(data.items ?? [])
  }
  useEffect(() => {
    loadChannels().catch(e => onNotice(errorText(e)))
    loadUsers().catch(e => onNotice(errorText(e)))
    loadMessages('').catch(e => onNotice(errorText(e)))
  }, [])

  // after is what every mutation ends with: the list the operator is looking at, plus the
  // two panels that mirror it. One reload path means no panel can show a stale revision.
  async function after() {
    await loadChannels()
    await loadTargets()
    await loadMessages()
  }
  // act runs one mutation, then reloads everything the operator is looking at so no panel
  // can show a stale revision. A run that returns a string reports its own outcome — the
  // dispatcher's counts, say — and that replaces the generic message rather than being
  // overwritten by it a millisecond later.
  async function act(key: string, run: () => Promise<unknown>, done: string) {
    setBusy(key)
    try {
      const result = await run()
      onNotice(typeof result === 'string' && result ? result : done)
      await after()
    } catch (e) { onNotice(errorText(e)) } finally { setBusy('') }
  }

  function setting(key: string) { return draft.settings[key] ?? '' }
  function setSetting(key: string, value: string) { setDraft(d => ({ ...d, settings: { ...d.settings, [key]: value } })) }

  // draftValid is the same question the server asks, answered early enough to disable the
  // button: a provider's required fields, and its credential when it needs one.
  const draftValid = draft.name.trim().length > 0
    && (!shape.secretRequired || draft.secret.trim().length > 0)
    && (!shape.endpointRequired || draft.endpoint.trim().length > 0)
    && shape.settings.every(field => !field.required || (draft.settings[field.key] ?? '').trim().length > 0)

  function createChannel() {
    return act('create', () => request('/admin/notifications/channels', {
      method: 'POST', headers: { 'Idempotency-Key': idem() },
      body: JSON.stringify({ name: draft.name, provider, endpoint: draft.endpoint, settings: draft.settings, secret: draft.secret, status: draft.status }),
    }), '通道已创建')
      .then(() => { setDraft(emptyDraft()) })
  }

  function saveChannel(channel: NotificationChannel) {
    const body: Record<string, unknown> = {
      name: channel.name, endpoint: channel.endpoint, settings: channel.settings ?? {}, status: channel.status,
    }
    // An empty secret field means "keep what is stored", which is the only honest option
    // when the server never showed it.
    if (editSecret.trim()) body.secret = editSecret.trim()
    return act('save', () => request('/admin/notifications/channels/' + channel.id, {
      method: 'PATCH', headers: { 'If-Match': resourceETag('notify_channel', channel.id, channel.revision) }, body: JSON.stringify(body),
    }), '通道已更新').then(() => { setEditing(null); setEditSecret('') })
  }

  async function verify(channel: NotificationChannel) {
    setBusy('verify-' + channel.id)
    try {
      const { data } = await request<NotificationCheck>('/admin/notifications/channels/' + channel.id + '/verification', { method: 'POST', headers: { 'Idempotency-Key': idem() } })
      onNotice(data.verified ? `${channel.name} 凭据验证通过` : (data.detail || '凭据验证未通过'))
      await loadChannels()
    } catch (e) { onNotice(errorText(e)) } finally { setBusy('') }
  }

  async function sendTest() {
    if (!testFor) return
    const url = testFor.channel
      ? '/admin/notifications/channels/' + testFor.channel.id + '/test'
      : '/admin/notifications/targets/' + testFor.target?.id + '/test'
    const init: RequestInit = { method: 'POST', headers: { 'Idempotency-Key': idem() } }
    if (testFor.channel) init.body = JSON.stringify({ address: testAddress })
    setBusy('test')
    try {
      const { data } = await request<NotificationDelivery>(url, init)
      onNotice(data.delivered ? '测试消息已送达' : (data.detail || '测试消息未送达'))
      setTestFor(null); setTestAddress('')
      await after()
    } catch (e) { onNotice(errorText(e)) } finally { setBusy('') }
  }

  function dispatchNow() {
    return act('dispatch', async () => {
      const { data } = await request<NotificationDispatch>('/admin/notifications/dispatch', { method: 'POST', headers: { 'Idempotency-Key': idem() } })
      if (data.claimed === 0) return '没有到期的通知，队列已空'
      return `本次投递：送达 ${data.sent}，重试 ${data.retried}，失败 ${data.failed}${data.retired ? `，失效接收端 ${data.retired}` : ''}`
    }, '待发通知已处理')
  }

  function sendBroadcast() {
    if (!broadcast || !broadcast.title.trim()) return Promise.resolve()
    return act('broadcast', () => request('/admin/notifications/broadcast', {
      method: 'POST', headers: { 'Idempotency-Key': idem() },
      body: JSON.stringify({ title: broadcast.title, body: broadcast.body, channel_id: broadcast.channel_id || undefined, topic: 'notification.broadcast' }),
    }), '广播已进入队列').then(() => setBroadcast(null))
  }

  function createTarget() {
    if (!recipient.user_id || !recipient.address.trim() || !channelID) return Promise.resolve()
    return act('recipient', () => request('/admin/notifications/targets', {
      method: 'POST', headers: { 'Idempotency-Key': idem() },
      body: JSON.stringify({ user_id: recipient.user_id, channel_id: channelID, address: recipient.address.trim(), label: recipient.label }),
    }), '接收端已添加').then(() => setRecipient({ user_id: '', address: '', label: '' }))
  }

  function patchTarget(target: NotificationTarget, body: Record<string, unknown>, message: string) {
    return act('target-' + target.id, () => request('/admin/notifications/targets/' + target.id, {
      method: 'PATCH', headers: { 'If-Match': resourceETag('notify_target', target.id, target.revision) }, body: JSON.stringify(body),
    }), message)
  }

  const selectedChannel = channels.find(item => item.id === channelID) ?? null

  return (
    <div className="admin-layout">
      <div className="admin-form">
        <h2>新增通道</h2>
        <mdui-select label="推送方式" value={provider} onChange={e => { setProvider(fieldValue(e)); setDraft(emptyDraft()) }}>
          {PROVIDER_ORDER.map(key => <mdui-menu-item key={key} value={key}>{PROVIDER_LABEL[key]}</mdui-menu-item>)}
        </mdui-select>
        <mdui-text-field label="通道名称" variant="outlined" required value={draft.name} onChange={e => setDraft(d => ({ ...d, name: fieldValue(e) }))} />
        <mdui-text-field label={shape.endpointLabel} variant="outlined" required={shape.endpointRequired} value={draft.endpoint} onChange={e => setDraft(d => ({ ...d, endpoint: fieldValue(e) }))} />
        {shape.settings.map(field => field.options ? (
          <mdui-select key={field.key} label={field.label} value={setting(field.key)} onChange={e => setSetting(field.key, fieldValue(e))}>
            <mdui-menu-item value="">默认</mdui-menu-item>
            {field.options.map(option => <mdui-menu-item key={option} value={option}>{option}</mdui-menu-item>)}
          </mdui-select>
        ) : (
          <mdui-text-field key={field.key} label={field.label} variant="outlined" required={field.required} value={setting(field.key)} onChange={e => setSetting(field.key, fieldValue(e))} />
        ))}
        {shape.secretRequired && (shape.secretRows
          ? <mdui-text-field label={shape.secretLabel} variant="outlined" required rows={shape.secretRows} value={draft.secret} onChange={e => setDraft(d => ({ ...d, secret: fieldValue(e) }))} />
          : <mdui-text-field label={shape.secretLabel} type="password" toggle-password variant="outlined" required value={draft.secret} onChange={e => setDraft(d => ({ ...d, secret: fieldValue(e) }))} />)}
        <mdui-select label="创建后状态" value={draft.status} onChange={e => setDraft(d => ({ ...d, status: fieldValue(e) }))}>
          <mdui-menu-item value="active">启用</mdui-menu-item>
          <mdui-menu-item value="disabled">先停用</mdui-menu-item>
        </mdui-select>
        <mdui-button variant="filled" disabled={!draftValid || busy === 'create'} loading={busy === 'create'} onClick={() => { void createChannel() }}>创建通道</mdui-button>
        <small>{shape.note}</small>
        <small>凭据用服务端 AES-GCM 加密入库，接口永不回显明文；编辑时留空即保留原凭据。</small>
      </div>

      <div className="admin-content">
        <div className="admin-toolbar">
          <mdui-select label="投递记录筛选" value={messageStatus} onChange={e => { const next = fieldValue(e); setMessageStatus(next); loadMessages(next).catch(err => onNotice(errorText(err))) }}>
            <mdui-menu-item value="">全部</mdui-menu-item>
            {Object.keys(MESSAGE_STATUS).map(key => <mdui-menu-item key={key} value={key}>{MESSAGE_STATUS[key]}</mdui-menu-item>)}
          </mdui-select>
          <div className="notify-toolbar">
            <mdui-button variant="tonal" icon="send" disabled={busy === 'dispatch'} loading={busy === 'dispatch'} onClick={() => { void dispatchNow() }}>立即投递</mdui-button>
            <mdui-button variant="outlined" icon="campaign" onClick={() => { setTestFor(null); setBroadcast({ channel_id: channelID, title: '', body: '' }) }}>广播</mdui-button>
          </div>
        </div>

        <div className="admin-grid">
          {channels.map(channel => (
            <mdui-card variant="outlined" key={channel.id}
              className={'admin-card' + (channel.id === channelID ? ' selected' : '') + (channel.status === 'disabled' ? ' muted' : '')}>
              {editing?.id === channel.id ? (
                <div className="edit-stack">
                  <mdui-text-field label="通道名称" variant="outlined" value={editing.name} onChange={e => setEditing({ ...editing, name: fieldValue(e) })} />
                  <mdui-text-field label="服务地址" variant="outlined" value={editing.endpoint} onChange={e => setEditing({ ...editing, endpoint: fieldValue(e) })} />
                  {(PROVIDERS[channel.provider]?.settings ?? []).map(field => (
                    <mdui-text-field key={field.key} label={field.label} variant="outlined" value={editing.settings?.[field.key] ?? ''}
                      onChange={e => setEditing({ ...editing, settings: { ...editing.settings, [field.key]: fieldValue(e) } })} />
                  ))}
                  {PROVIDERS[channel.provider]?.secretRequired && (
                    PROVIDERS[channel.provider]?.secretRows
                      ? <mdui-text-field label="替换凭据（留空则保留）" variant="outlined" rows={PROVIDERS[channel.provider].secretRows} value={editSecret} onChange={e => setEditSecret(fieldValue(e))} />
                      : <mdui-text-field label="替换凭据（留空则保留）" type="password" toggle-password variant="outlined" value={editSecret} onChange={e => setEditSecret(fieldValue(e))} />)}
                  <mdui-select label="状态" value={editing.status} onChange={e => setEditing({ ...editing, status: fieldValue(e) })}>
                    <mdui-menu-item value="active">启用</mdui-menu-item>
                    <mdui-menu-item value="disabled">停用</mdui-menu-item>
                  </mdui-select>
                  <div className="admin-actions">
                    <mdui-button variant="filled" disabled={busy === 'save'} onClick={() => { void saveChannel(editing) }}>保存</mdui-button>
                    <mdui-button variant="text" onClick={() => { setEditing(null); setEditSecret('') }}>取消</mdui-button>
                  </div>
                </div>
              ) : (
                <>
                  <header>
                    <div>
                      <p className="eyebrow">{PROVIDER_LABEL[channel.provider] ?? channel.provider} · {CHANNEL_STATUS[channel.status] ?? channel.status}</p>
                      <h3>{channel.name}</h3>
                      <small>接收端 {channel.active_target_count}/{channel.target_count} · 凭据 {channel.secret_set ? (channel.secret_hint || '已设置') : '不需要'}</small>
                    </div>
                  </header>
                  {channel.endpoint && <p className="provider-url">{channel.endpoint}</p>}
                  <p className="notify-check">
                    最近验证：{CHECK_STATUS[channel.last_check_status] ?? (channel.last_check_status || '未验证')}
                    {channel.last_check_at ? ' · ' + stamp(channel.last_check_at) : ''}
                    {channel.last_error ? <span className="notify-error">{channel.last_error}</span> : null}
                  </p>
                  <div className="admin-actions">
                    <mdui-button variant="filled" onClick={() => { setChannelID(channel.id); void loadTargets(channel.id).catch(e => onNotice(errorText(e))) }} disabled={channel.id === channelID}>接收端</mdui-button>
                    <mdui-button variant="outlined" icon="bolt" disabled={busy === 'verify-' + channel.id} loading={busy === 'verify-' + channel.id} onClick={() => { void verify(channel) }}>验证凭据</mdui-button>
                    <mdui-button variant="text" icon="notifications_active" onClick={() => { setBroadcast(null); setTestFor({ channel }); setTestAddress('') }}>试发</mdui-button>
                    <mdui-button variant="text" icon="edit" onClick={() => { setEditing(channel); setEditSecret('') }}>编辑</mdui-button>
                    <mdui-button variant="text" onClick={() => { void act('status-' + channel.id, () => request('/admin/notifications/channels/' + channel.id, { method: 'PATCH', headers: { 'If-Match': resourceETag('notify_channel', channel.id, channel.revision) }, body: JSON.stringify({ status: channel.status === 'active' ? 'disabled' : 'active' }) }), '状态已更新') }}>{channel.status === 'active' ? '停用' : '启用'}</mdui-button>
                    <mdui-button variant="text" icon="delete" onClick={() => { void act('delete-' + channel.id, () => request('/admin/notifications/channels/' + channel.id, { method: 'DELETE' }), '通道已删除').then(() => { if (channelID === channel.id) { setChannelID(''); setTargets([]) } }) }}>删除</mdui-button>
                  </div>
                </>
              )}
            </mdui-card>
          ))}
        </div>
        {channels.length === 0 && <p className="empty">还没有通道。先添加一个 Telegram 机器人或 Bark 服务器，再为用户登记接收端。</p>}

        <section className="notify-panel">
          <header>
            <h3>接收端{selectedChannel ? ` · ${selectedChannel.name}` : ''}</h3>
            {selectedChannel && <p className="meta">地址形如 {PROVIDERS[selectedChannel.provider]?.addressLabel ?? '地址'}：{PROVIDERS[selectedChannel.provider]?.addressHint ?? ''}</p>}
          </header>
          {!selectedChannel ? <p className="empty">选择一个通道后，可以在这里为用户登记地址、停用失效设备或发一条测试。</p> : (
            <>
              <div className="notify-form">
                <mdui-select label="用户" value={recipient.user_id} onChange={e => setRecipient(r => ({ ...r, user_id: fieldValue(e) }))}>
                  {users.map(user => <mdui-menu-item key={user.id} value={user.id}>{user.display_name}（{user.identifier}）</mdui-menu-item>)}
                </mdui-select>
                <mdui-text-field label={PROVIDERS[selectedChannel.provider]?.addressLabel ?? '地址'} variant="outlined" value={recipient.address} onChange={e => setRecipient(r => ({ ...r, address: fieldValue(e) }))} />
                <mdui-text-field label="备注（例如：张三的 iPhone）" variant="outlined" value={recipient.label} onChange={e => setRecipient(r => ({ ...r, label: fieldValue(e) }))} />
                <mdui-button variant="tonal" icon="add" disabled={!recipient.user_id || !recipient.address.trim()} onClick={() => { void createTarget() }}>添加</mdui-button>
              </div>
              {targets.length === 0 ? <p className="empty">这个通道还没有接收端。</p> : (
                <ul className="notify-rows">
                  {targets.map(target => (
                    <li key={target.id} className={target.status === 'active' ? '' : 'muted'}>
                      <div className="notify-row-main">
                        <strong>{target.user_display_name || target.user_identifier || target.user_id}</strong>
                        <span>{target.label || '未命名'}</span>
                        <code>{target.address_hint}</code>
                        <span>{TARGET_STATUS[target.status] ?? target.status}</span>
                        {target.failure_count > 0 && <span>失败 {target.failure_count} 次</span>}
                        {target.last_sent_at && <span>最近送达 {stamp(target.last_sent_at)}</span>}
                      </div>
                      {target.last_error && <p className="notify-error">{target.last_error}</p>}
                      <div className="notify-row-actions">
                        <mdui-button variant="text" icon="send" onClick={() => { setBroadcast(null); setTestFor({ target }); setTestAddress('') }}>测试</mdui-button>
                        <mdui-button variant="text" onClick={() => { void patchTarget(target, { status: target.status === 'active' ? 'disabled' : 'active' }, '接收端状态已更新') }}>{target.status === 'active' ? '停用' : '启用'}</mdui-button>
                        <mdui-button variant="text" icon="delete" onClick={() => { void act('delete-target-' + target.id, () => request('/admin/notifications/targets/' + target.id, { method: 'DELETE' }), '接收端已删除') }}>删除</mdui-button>
                      </div>
                    </li>
                  ))}
                </ul>
              )}
            </>
          )}
        </section>

        <section className="notify-panel">
          <header><h3>投递记录</h3><p className="meta">保留 14 天。失败原因来自提供方本身，不含任何凭据。</p></header>
          {messages.length === 0 ? <p className="empty">暂无投递记录。</p> : (
            <ul className="notify-rows">
              {messages.map(message => (
                <li key={message.id} className={message.status === 'failed' ? 'failed' : ''}>
                  <div className="notify-row-main">
                    <strong>{message.title || message.topic}</strong>
                    <span>{MESSAGE_STATUS[message.status] ?? message.status}</span>
                    <span>{message.user_display_name || message.user_identifier || message.user_id}</span>
                    <span>{message.channel_name || message.provider}</span>
                    <span>{message.target_hint}</span>
                    <time>{stamp(message.created_at)}</time>
                  </div>
                  <p className="meta">{message.topic} · 第 {message.attempts}/{message.max_attempts} 次{message.provider_message_id ? ' · ' + message.provider_message_id : ''}</p>
                  {message.last_error && <p className="notify-error">{message.last_error}</p>}
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>

      {testFor && (
        <mdui-dialog ref={dialogRef} open icon="send" headline="发送测试通知"
          description={testFor.target
            ? `向 ${testFor.target.label || testFor.target.address_hint} 发送一条测试消息，结果会记入投递记录。`
            : `直接向一个地址发送测试消息，不会保存为接收端。`}>
          {testFor.channel && (
            <mdui-text-field label={PROVIDERS[testFor.channel.provider]?.addressLabel ?? '地址'} variant="outlined" required
              helper={PROVIDERS[testFor.channel.provider]?.addressHint ?? ''} value={testAddress} onChange={e => setTestAddress(fieldValue(e))} />
          )}
          <mdui-button slot="action" onClick={() => setTestFor(null)}>取消</mdui-button>
          <mdui-button slot="action" variant="filled" disabled={busy === 'test' || (Boolean(testFor.channel) && !testAddress.trim())} loading={busy === 'test'} onClick={() => { void sendTest() }}>发送</mdui-button>
        </mdui-dialog>
      )}

      {broadcast && (
        <mdui-dialog ref={dialogRef} open icon="campaign" headline="广播通知" description="向所有在用接收端排队发送一条消息。通道留空表示全部通道。">
          <mdui-select label="通道" value={broadcast.channel_id} onChange={e => setBroadcast(b => b && { ...b, channel_id: fieldValue(e) })}>
            <mdui-menu-item value="">全部通道</mdui-menu-item>
            {channels.map(channel => <mdui-menu-item key={channel.id} value={channel.id}>{channel.name}</mdui-menu-item>)}
          </mdui-select>
          <mdui-text-field label="标题" variant="outlined" required value={broadcast.title} onChange={e => setBroadcast(b => b && { ...b, title: fieldValue(e) })} />
          <mdui-text-field label="正文" variant="outlined" rows={3} value={broadcast.body} onChange={e => setBroadcast(b => b && { ...b, body: fieldValue(e) })} />
          <mdui-button slot="action" onClick={() => setBroadcast(null)}>取消</mdui-button>
          <mdui-button slot="action" variant="filled" disabled={!broadcast.title.trim()} onClick={() => { void sendBroadcast() }}>发送</mdui-button>
        </mdui-dialog>
      )}
    </div>
  )
}
