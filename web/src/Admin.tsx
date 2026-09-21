import { useEffect, useRef, useState } from 'react'
import './admin.css'
import { idem, request } from './api'
import { fieldValue, useMduiEvent } from './mdui-react'
import type { AuditView, ModelProvider, SessionView, User } from './types'

type Section = 'users' | 'models' | 'audit'

export function Admin({ user, onNotice }: { user: User; onNotice: (value: string) => void }) {
  const [section, setSection] = useState<Section>('users')
  return (
    <section className="admin-page">
      <header className="page-head compact">
        <div>
          <p className="eyebrow">ADMIN PLATFORM</p>
          <h1>统一控制台。<br /><em>可继续扩展。</em></h1>
        </div>
      </header>
      <mdui-tabs className="admin-tabs" value={section} onChange={e => setSection(fieldValue(e) as Section)}>
        <mdui-tab value="users" icon="group">用户管理</mdui-tab>
        <mdui-tab value="models" icon="hub">模型管理</mdui-tab>
        <mdui-tab value="audit" icon="history">审计日志</mdui-tab>
      </mdui-tabs>
      {section === 'users' && <UserAdmin currentID={user.id} onNotice={onNotice} />}
      {section === 'models' && <ProviderAdmin onNotice={onNotice} />}
      {section === 'audit' && <AuditAdmin onNotice={onNotice} />}
    </section>
  )
}

function UserAdmin({ currentID, onNotice }: { currentID: string; onNotice: (value: string) => void }) {
  const [users, setUsers] = useState<User[]>([])
  const [query, setQuery] = useState('')
  const [sessions, setSessions] = useState<SessionView[]>([])
  const [selected, setSelected] = useState<User | null>(null)
  const [draft, setDraft] = useState({ identifier: '', displayName: '', password: '', timezone: 'Asia/Shanghai', role: 'member' })
  const [resetTarget, setResetTarget] = useState<User | null>(null)
  const [resetValue, setResetValue] = useState('')
  const resetDialogRef = useRef<HTMLElement>(null)
  useMduiEvent(resetDialogRef, 'close', () => setResetTarget(null))

  async function loadUsers(searchQuery = query) {
    const search = new URLSearchParams(searchQuery ? { q: searchQuery } : {})
    const { data } = await request<{ items: User[] }>('/admin/users?' + search)
    setUsers(data.items)
  }
  async function loadSessions(selectedUser: User) {
    setSelected(selectedUser)
    const { data } = await request<{ items: SessionView[] }>('/admin/users/' + selectedUser.id + '/sessions?active_only=true')
    setSessions(data.items)
  }
  useEffect(() => { loadUsers().catch(e => onNotice(errorText(e))) }, [])

  async function createUser() {
    if (!draft.identifier.trim() || !draft.displayName.trim() || draft.password.length < 12) return
    try {
      await request('/admin/users', { method: 'POST', headers: { 'Idempotency-Key': idem() }, body: JSON.stringify({ identifier: draft.identifier, password: draft.password, display_name: draft.displayName, timezone: draft.timezone || 'Asia/Shanghai', locale: 'zh-CN', role: draft.role }) })
      setDraft({ identifier: '', displayName: '', password: '', timezone: 'Asia/Shanghai', role: 'member' }); onNotice('用户已创建'); loadUsers()
    } catch (e) { onNotice(errorText(e)) }
  }
  async function patchUser(target: User, body: Record<string, unknown>, message: string) {
    try {
      await request('/admin/users/' + target.id, { method: 'PATCH', headers: { 'If-Match': etag('user', target.id, target.revision) }, body: JSON.stringify(body) })
      onNotice(message); loadUsers()
    } catch (e) { onNotice(errorText(e)) }
  }
  async function resetPassword(target: User) { setResetTarget(target); setResetValue('') }
  async function confirmReset() {
    if (!resetTarget || resetValue.length < 12) return
    try {
      await request('/admin/users/' + resetTarget.id + '/password', { method: 'POST', headers: { 'Idempotency-Key': idem() }, body: JSON.stringify({ password: resetValue }) })
      setResetTarget(null); setResetValue(''); onNotice('密码已重置，旧会话已撤销'); loadUsers()
    } catch (e) { onNotice(errorText(e)) }
  }
  async function revoke(target: User) {
    try {
      await request('/admin/users/' + target.id + '/session-revocation', { method: 'POST', headers: { 'Idempotency-Key': idem() } })
      onNotice('会话已撤销'); if (selected?.id === target.id) loadSessions(target)
    } catch (e) { onNotice(errorText(e)) }
  }

  return (
    <div className="admin-layout">
      <div className="admin-form">
        <h2>新增用户</h2>
        <mdui-text-field label="登录账号" variant="outlined" required minlength={3} value={draft.identifier} onChange={e => setDraft(d => ({ ...d, identifier: fieldValue(e) }))} />
        <mdui-text-field label="显示名" variant="outlined" required value={draft.displayName} onChange={e => setDraft(d => ({ ...d, displayName: fieldValue(e) }))} />
        <mdui-text-field label="初始密码（至少12位）" type="password" toggle-password variant="outlined" required minlength={12} value={draft.password} onChange={e => setDraft(d => ({ ...d, password: fieldValue(e) }))} />
        <mdui-text-field label="时区" variant="outlined" value={draft.timezone} onChange={e => setDraft(d => ({ ...d, timezone: fieldValue(e) }))} />
        <mdui-select label="角色" value={draft.role} onChange={e => setDraft(d => ({ ...d, role: fieldValue(e) }))}>
          <mdui-menu-item value="member">成员</mdui-menu-item>
          <mdui-menu-item value="admin">管理员</mdui-menu-item>
        </mdui-select>
        <mdui-button variant="filled" disabled={draft.identifier.trim().length < 3 || !draft.displayName.trim() || draft.password.length < 12} onClick={createUser}>创建</mdui-button>
      </div>
      <div className="admin-content">
        <div className="admin-toolbar">
          <mdui-text-field variant="outlined" clearable value={query} onChange={e => setQuery(fieldValue(e))} placeholder="搜索账号或姓名" />
          <mdui-button variant="tonal" icon="search" onClick={() => loadUsers().catch(e => onNotice(errorText(e)))}>搜索</mdui-button>
        </div>
        <div className="admin-grid">
          {users.map(item => (
            <mdui-card variant="outlined" className={item.status === 'disabled' ? 'admin-card muted' : 'admin-card'} key={item.id}>
              <header><div><p className="eyebrow">{item.role} · {item.status}</p><h3>{item.display_name}</h3><small>{item.identifier}</small></div></header>
              <div className="admin-actions">
                <mdui-button variant="outlined" onClick={() => patchUser(item, { role: item.role === 'admin' ? 'member' : 'admin' }, '角色已更新')} disabled={item.id === currentID}>{item.role === 'admin' ? '设为成员' : '设为管理员'}</mdui-button>
                <mdui-button variant="outlined" onClick={() => patchUser(item, { status: item.status === 'active' ? 'disabled' : 'active' }, '状态已更新')} disabled={item.id === currentID}>{item.status === 'active' ? '禁用' : '启用'}</mdui-button>
                <mdui-button variant="text" icon="key" onClick={() => resetPassword(item)}>重置密码</mdui-button>
                <mdui-button variant="text" onClick={() => loadSessions(item)}>会话</mdui-button>
                <mdui-button variant="text" onClick={() => revoke(item)}>撤销会话</mdui-button>
              </div>
            </mdui-card>
          ))}
        </div>
        {selected && <section className="session-panel"><h3>{selected.display_name} 的活跃会话</h3>{sessions.length === 0 ? <p>无活跃会话。</p> : sessions.map(session => <div key={session.id}><code>{session.id}</code><span>到期 {new Date(session.expires_at).toLocaleString()}</span></div>)}</section>}
      </div>
      {resetTarget && <mdui-dialog ref={resetDialogRef} open icon="key" headline="重置密码" description={`为 ${resetTarget.identifier} 设置至少 12 位新密码，旧会话将被全部撤销。`}>
        <mdui-text-field label="新密码（至少12位）" type="password" toggle-password variant="outlined" required minlength={12} value={resetValue} onChange={e => setResetValue(fieldValue(e))} />
        <mdui-button slot="action" onClick={() => setResetTarget(null)}>取消</mdui-button>
        <mdui-button slot="action" variant="filled" disabled={resetValue.length < 12} onClick={confirmReset}>确认重置</mdui-button>
      </mdui-dialog>}
    </div>
  )
}

function ProviderAdmin({ onNotice }: { onNotice: (value: string) => void }) {
  const [providers, setProviders] = useState<ModelProvider[]>([])
  const [editing, setEditing] = useState<ModelProvider | null>(null)
  const [apiKey, setApiKey] = useState('')
  const [testingID, setTestingID] = useState('')
  const [draft, setDraft] = useState({ name: '', baseUrl: '', modelName: '', transcriptionModel: '', apiKey: '' })

  async function load() {
    const { data } = await request<{ items: ModelProvider[] }>('/admin/model-providers')
    setProviders(data.items)
  }
  useEffect(() => { load().catch(e => onNotice(errorText(e))) }, [])

  async function create() {
    if (!draft.name.trim() || !draft.baseUrl.trim() || !draft.modelName.trim() || draft.apiKey.length < 12) return
    try {
      await request('/admin/model-providers', { method: 'POST', headers: { 'Idempotency-Key': idem() }, body: JSON.stringify({ name: draft.name, base_url: draft.baseUrl, model_name: draft.modelName, transcription_model: draft.transcriptionModel, api_key: draft.apiKey, is_default: providers.length === 0 }) })
      setDraft({ name: '', baseUrl: '', modelName: '', transcriptionModel: '', apiKey: '' }); onNotice('Provider 已创建'); load()
    } catch (e) { onNotice(errorText(e)) }
  }
  async function activate(provider: ModelProvider) {
    try { await request('/admin/model-providers/' + provider.id + '/activation', { method: 'POST', headers: { 'Idempotency-Key': idem() } }); onNotice('默认模型已切换'); load() } catch (e) { onNotice(errorText(e)) }
  }
  async function save() {
    if (!editing) return
    try {
      const body: Record<string, unknown> = { name: editing.name, base_url: editing.base_url, model_name: editing.model_name, transcription_model: editing.transcription_model }
      if (apiKey) body.api_key = apiKey
      await request('/admin/model-providers/' + editing.id, { method: 'PATCH', headers: { 'If-Match': etag('provider', editing.id, editing.revision) }, body: JSON.stringify(body) })
      setEditing(null); setApiKey(''); onNotice('Provider 已更新'); load()
    } catch (e) { onNotice(errorText(e)) }
  }
  async function remove(provider: ModelProvider) {
    try { await request('/admin/model-providers/' + provider.id, { method: 'DELETE' }); onNotice('Provider 已删除'); load() } catch (e) { onNotice(errorText(e)) }
  }
  async function verify(provider: ModelProvider) {
    setTestingID(provider.id)
    try {
      const { data } = await request<{ verified: boolean; provider: string; detail?: string }>('/admin/model-providers/' + provider.id + '/verification', { method: 'POST', headers: { 'Idempotency-Key': idem() } })
      onNotice(data.verified ? 'Provider 连接验证通过' : (data.detail || 'Provider 连接验证失败'))
    } catch (e) { onNotice(errorText(e)) } finally { setTestingID('') }
  }

  return (
    <div className="admin-layout">
      <div className="admin-form">
        <h2>新增 Provider</h2>
        <mdui-text-field label="显示名称" variant="outlined" required value={draft.name} onChange={e => setDraft(d => ({ ...d, name: fieldValue(e) }))} />
        <mdui-text-field label="https://provider.example/v1" variant="outlined" required value={draft.baseUrl} onChange={e => setDraft(d => ({ ...d, baseUrl: fieldValue(e) }))} />
        <mdui-text-field label="chat model" variant="outlined" required value={draft.modelName} onChange={e => setDraft(d => ({ ...d, modelName: fieldValue(e) }))} />
        <mdui-text-field label="语音转写 model（可选）" variant="outlined" value={draft.transcriptionModel} onChange={e => setDraft(d => ({ ...d, transcriptionModel: fieldValue(e) }))} />
        <mdui-text-field label="API Key" type="password" toggle-password variant="outlined" required minlength={12} value={draft.apiKey} onChange={e => setDraft(d => ({ ...d, apiKey: fieldValue(e) }))} />
        <mdui-button variant="filled" disabled={!draft.name.trim() || !draft.baseUrl.trim() || !draft.modelName.trim() || draft.apiKey.length < 12} onClick={create}>创建</mdui-button>
        <small>Key 使用服务端 AES-GCM 加密后入库，接口永远不回显明文。</small>
      </div>
      <div className="admin-content">
        <div className="admin-grid">
          {providers.map(provider => (
            <mdui-card variant="outlined" className={provider.is_default ? 'admin-card selected' : 'admin-card'} key={provider.id}>
              <header><div><p className="eyebrow">{provider.is_default ? '默认' : provider.status}</p><h3>{provider.name}</h3><small>{provider.model_name}</small></div></header>
              <p className="provider-url">{provider.base_url}</p>
              <p>API Key：<code>{provider.api_key_hint || 'not set'}</code></p>
              {editing?.id === provider.id ? (
                <div className="edit-stack">
                  <mdui-text-field label="显示名称" variant="outlined" value={editing.name} onChange={e => setEditing({ ...editing, name: fieldValue(e) })} />
                  <mdui-text-field label="Base URL" variant="outlined" value={editing.base_url} onChange={e => setEditing({ ...editing, base_url: fieldValue(e) })} />
                  <mdui-text-field label="chat model" variant="outlined" value={editing.model_name} onChange={e => setEditing({ ...editing, model_name: fieldValue(e) })} />
                  <mdui-text-field label="语音转写 model" variant="outlined" value={editing.transcription_model} onChange={e => setEditing({ ...editing, transcription_model: fieldValue(e) })} />
                  <mdui-text-field type="password" toggle-password variant="outlined" placeholder="仅填写则更换 API Key" value={apiKey} onChange={e => setApiKey(fieldValue(e))} />
                  <div className="admin-actions"><mdui-button variant="filled" onClick={save}>保存</mdui-button><mdui-button variant="text" onClick={() => { setEditing(null); setApiKey('') }}>取消</mdui-button></div>
                </div>
              ) : (
                <div className="admin-actions">
                  <mdui-button variant="filled" onClick={() => activate(provider)} disabled={provider.is_default}>设为默认</mdui-button>
                  <mdui-button variant="outlined" icon="bolt" onClick={() => verify(provider)} disabled={testingID === provider.id}>{testingID === provider.id ? '验证中' : '测试连接'}</mdui-button>
                  <mdui-button variant="text" icon="edit" onClick={() => { setEditing(provider); setApiKey('') }}>编辑</mdui-button>
                  <mdui-button variant="text" icon="delete" onClick={() => remove(provider)} disabled={provider.is_default}>删除</mdui-button>
                </div>
              )}
            </mdui-card>
          ))}
        </div>
        {providers.length === 0 && <p className="empty">还没有 Provider。未配置时任务拆解使用本地确定性演示，真实 LLM 需要在此添加。</p>}
      </div>
    </div>
  )
}

function AuditAdmin({ onNotice }: { onNotice: (value: string) => void }) {
  const [events, setEvents] = useState<AuditView[]>([])
  const [action, setAction] = useState('')
  const [loading, setLoading] = useState(true)

  async function load(nextAction = action) {
    setLoading(true)
    try {
      const search = nextAction ? '?action=' + encodeURIComponent(nextAction) + '&limit=100' : '?limit=100'
      const { data } = await request<{ items: AuditView[] }>('/admin/audit-events' + search)
      setEvents(data.items)
    } catch (e) { onNotice(errorText(e)) } finally { setLoading(false) }
  }
  useEffect(() => { load().catch(e => onNotice(errorText(e))) }, [])

  return (
    <section className="audit-panel">
      <div className="admin-toolbar">
        <mdui-text-field variant="outlined" clearable value={action} onChange={e => setAction(fieldValue(e))} placeholder="按 action 过滤，例如 model_provider.activated" />
        <mdui-button variant="tonal" icon="search" onClick={() => load()}>查询</mdui-button>
      </div>
      {loading ? <p className="empty">正在读取审计记录…</p> : events.length === 0 ? <p className="empty">暂无管理审计记录。</p> : events.map(event => (
        <mdui-card variant="outlined" className="audit-item" key={event.id}>
          <header>
            <strong>{event.action}</strong>
            <time>{new Date(event.created_at).toLocaleString()}</time>
          </header>
          <p>{event.actor_display_name || event.actor_identifier}{event.target_identifier ? ' → ' + (event.target_display_name || event.target_identifier) : ''}</p>
          <code>{event.detail_json}</code>
        </mdui-card>
      ))}
    </section>
  )
}

function etag(kind: string, id: string, revision: number) { return '"' + kind + '_' + id + '_rev_' + revision + '"' }
function errorText(error: unknown) { return error instanceof Error ? error.message : '操作失败' }
