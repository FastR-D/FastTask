import { FormEvent, useEffect, useState } from 'react'
import './admin.css'
import { idem, request } from './api'
import type { AuditView, ModelProvider, SessionView, User } from './types'

export function Admin({ user, onNotice }: { user: User; onNotice: (value: string) => void }) {
  const [section, setSection] = useState<'users' | 'models' | 'audit'>('users')
  return (
    <section>
      <header className="page-head compact">
        <div>
          <p className="eyebrow">ADMIN PLATFORM</p>
          <h1>统一控制台。<br /><em>可继续扩展。</em></h1>
        </div>
      </header>
      <div className="admin-tabs">
        <button className={section === 'users' ? 'active' : ''} onClick={() => setSection('users')}>用户管理</button>
        <button className={section === 'models' ? 'active' : ''} onClick={() => setSection('models')}>模型管理</button>
        <button className={section === 'audit' ? 'active' : ''} onClick={() => setSection('audit')}>审计日志</button>
      </div>
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

  async function createUser(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    try {
      await request('/admin/users', { method: 'POST', headers: { 'Idempotency-Key': idem() }, body: JSON.stringify({ identifier: form.get('identifier'), password: form.get('password'), display_name: form.get('display_name'), timezone: form.get('timezone') || 'Asia/Shanghai', locale: 'zh-CN', role: form.get('role') }) })
      event.currentTarget.reset(); onNotice('用户已创建'); loadUsers()
    } catch (e) { onNotice(errorText(e)) }
  }
  async function patchUser(target: User, body: Record<string, unknown>, message: string) {
    try {
      await request('/admin/users/' + target.id, { method: 'PATCH', headers: { 'If-Match': etag('user', target.id, target.revision) }, body: JSON.stringify(body) })
      onNotice(message); loadUsers()
    } catch (e) { onNotice(errorText(e)) }
  }
  async function resetPassword(target: User) {
    const password = window.prompt('为 ' + target.identifier + ' 设置至少 12 位新密码')
    if (!password) return
    try {
      await request('/admin/users/' + target.id + '/password', { method: 'POST', headers: { 'Idempotency-Key': idem() }, body: JSON.stringify({ password }) })
      onNotice('密码已重置，旧会话已撤销'); loadUsers()
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
      <form className="admin-form" onSubmit={createUser}>
        <h2>新增用户</h2>
        <input name="identifier" placeholder="登录账号" required minLength={3} />
        <input name="display_name" placeholder="显示名" required />
        <input name="password" type="password" placeholder="初始密码（至少12位）" required minLength={12} />
        <input name="timezone" defaultValue="Asia/Shanghai" />
        <select name="role" defaultValue="member"><option value="member">成员</option><option value="admin">管理员</option></select>
        <button className="primary">创建</button>
      </form>
      <div className="admin-content">
        <div className="admin-toolbar">
          <input value={query} onChange={e => setQuery(e.target.value)} placeholder="搜索账号或姓名" />
          <button onClick={() => loadUsers().catch(e => onNotice(errorText(e)))}>搜索</button>
        </div>
        <div className="admin-grid">
          {users.map(item => (
            <article className={item.status === 'disabled' ? 'admin-card muted' : 'admin-card'} key={item.id}>
              <header><div><p className="eyebrow">{item.role} · {item.status}</p><h3>{item.display_name}</h3><small>{item.identifier}</small></div></header>
              <div className="admin-actions">
                <button onClick={() => patchUser(item, { role: item.role === 'admin' ? 'member' : 'admin' }, '角色已更新')} disabled={item.id === currentID}>{item.role === 'admin' ? '设为成员' : '设为管理员'}</button>
                <button onClick={() => patchUser(item, { status: item.status === 'active' ? 'disabled' : 'active' }, '状态已更新')} disabled={item.id === currentID}>{item.status === 'active' ? '禁用' : '启用'}</button>
                <button onClick={() => resetPassword(item)}>重置密码</button>
                <button onClick={() => loadSessions(item)}>会话</button>
                <button onClick={() => revoke(item)}>撤销会话</button>
              </div>
            </article>
          ))}
        </div>
        {selected && <section className="session-panel"><h3>{selected.display_name} 的活跃会话</h3>{sessions.length === 0 ? <p>无活跃会话。</p> : sessions.map(session => <div key={session.id}><code>{session.id}</code><span>到期 {new Date(session.expires_at).toLocaleString()}</span></div>)}</section>}
      </div>
    </div>
  )
}

function ProviderAdmin({ onNotice }: { onNotice: (value: string) => void }) {
  const [providers, setProviders] = useState<ModelProvider[]>([])
  const [editing, setEditing] = useState<ModelProvider | null>(null)
  const [apiKey, setApiKey] = useState('')
  const [testingID, setTestingID] = useState('')

  async function load() {
    const { data } = await request<{ items: ModelProvider[] }>('/admin/model-providers')
    setProviders(data.items)
  }
  useEffect(() => { load().catch(e => onNotice(errorText(e))) }, [])

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    try {
      await request('/admin/model-providers', { method: 'POST', headers: { 'Idempotency-Key': idem() }, body: JSON.stringify({ name: form.get('name'), base_url: form.get('base_url'), model_name: form.get('model_name'), transcription_model: form.get('transcription_model'), api_key: form.get('api_key'), is_default: providers.length === 0 }) })
      event.currentTarget.reset(); setApiKey(''); onNotice('Provider 已创建'); load()
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
      <form className="admin-form" onSubmit={create}>
        <h2>新增 Provider</h2>
        <input name="name" placeholder="显示名称" required />
        <input name="base_url" placeholder="https://provider.example/v1" required />
        <input name="model_name" placeholder="chat model" required />
        <input name="transcription_model" placeholder="语音转写 model（可选）" />
        <input name="api_key" type="password" placeholder="API Key" required minLength={12} />
        <button className="primary">创建</button>
        <small>Key 使用服务端 AES-GCM 加密后入库，接口永远不回显明文。</small>
      </form>
      <div className="admin-content">
        <div className="admin-grid">
          {providers.map(provider => (
            <article className={provider.is_default ? 'admin-card selected' : 'admin-card'} key={provider.id}>
              <header><div><p className="eyebrow">{provider.is_default ? '默认' : provider.status}</p><h3>{provider.name}</h3><small>{provider.model_name}</small></div></header>
              <p className="provider-url">{provider.base_url}</p>
              <p>API Key：<code>{provider.api_key_hint || 'not set'}</code></p>
              {editing?.id === provider.id ? (
                <div className="edit-stack">
                  <input value={editing.name} onChange={e => setEditing({ ...editing, name: e.target.value })} />
                  <input value={editing.base_url} onChange={e => setEditing({ ...editing, base_url: e.target.value })} />
                  <input value={editing.model_name} onChange={e => setEditing({ ...editing, model_name: e.target.value })} />
                  <input value={editing.transcription_model} onChange={e => setEditing({ ...editing, transcription_model: e.target.value })} />
                  <input type="password" value={apiKey} placeholder="仅填写则更换 API Key" onChange={e => setApiKey(e.target.value)} />
                  <div className="admin-actions"><button onClick={save}>保存</button><button onClick={() => { setEditing(null); setApiKey('') }}>取消</button></div>
                </div>
              ) : (
                <div className="admin-actions">
                  <button className="primary" onClick={() => activate(provider)} disabled={provider.is_default}>设为默认</button>
                  <button onClick={() => verify(provider)} disabled={testingID === provider.id}>{testingID === provider.id ? '验证中' : '测试连接'}</button>
                  <button onClick={() => { setEditing(provider); setApiKey('') }}>编辑</button>
                  <button onClick={() => remove(provider)} disabled={provider.is_default}>删除</button>
                </div>
              )}
            </article>
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
        <input value={action} onChange={e => setAction(e.target.value)} placeholder="按 action 过滤，例如 model_provider.activated" />
        <button onClick={() => load()}>查询</button>
      </div>
      {loading ? <p className="empty">正在读取审计记录…</p> : events.length === 0 ? <p className="empty">暂无管理审计记录。</p> : events.map(event => (
        <article className="audit-item" key={event.id}>
          <header>
            <strong>{event.action}</strong>
            <time>{new Date(event.created_at).toLocaleString()}</time>
          </header>
          <p>{event.actor_display_name || event.actor_identifier}{event.target_identifier ? ' → ' + (event.target_display_name || event.target_identifier) : ''}</p>
          <code>{event.detail_json}</code>
        </article>
      ))}
    </section>
  )
}

function etag(kind: string, id: string, revision: number) { return '"' + kind + '_' + id + '_rev_' + revision + '"' }
function errorText(error: unknown) { return error instanceof Error ? error.message : '操作失败' }
