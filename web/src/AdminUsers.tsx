import { useEffect, useState } from 'react'
import { Alert, Badge, Button, Dialog, EmptyState, Loading, Select, StatusBar, TextField, Toolbar } from '@fastrd/ui-react'
import { ApiError, idem, request, resourceETag } from './api'
import type { User } from './types'
import './admin-users.css'

type Action = { type: 'create' } | { type: 'edit' | 'delete' | 'revoke'; user: User; etag: string }
type Props = { currentUser: User; onCurrentUserUpdated: (user: User) => void; onSessionRevoked: () => void | Promise<void> }

function errorText(error: unknown) {
  if (error instanceof ApiError) {
    if (error.status === 412) return '用户资料已被其他管理员修改，请关闭弹窗并刷新列表后重试。'
    if (error.status === 409) return '操作未完成：登录账号可能已被占用，或此操作会影响最后一位管理员。'
    if (error.status === 403) return '需要管理员权限，请确认当前账号权限后重试。'
    if (error.status === 422) return '请检查账号、姓名和密码是否符合要求。'
    if (error.status === 404) return '此用户已不存在，请刷新列表。'
  }
  return error instanceof Error ? error.message : '操作失败，请稍后重试。'
}

export function AdminUsers({ currentUser, onCurrentUserUpdated, onSessionRevoked }: Props) {
  const [users, setUsers] = useState<User[]>([])
  const [query, setQuery] = useState('')
  const [status, setStatus] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [loading, setLoading] = useState(true)
  const [opening, setOpening] = useState(false)
  const [error, setError] = useState('')
  const [loadError, setLoadError] = useState('')
  const [feedback, setFeedback] = useState('')
  const [action, setAction] = useState<Action | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setLoadError('')
    const timer = window.setTimeout(() => {
      const params = new URLSearchParams()
      if (query.trim()) params.set('q', query.trim())
      if (status) params.set('status', status)
      void request<{ items: User[] | null }>(`/admin/users?${params}`, { signal: controller.signal }, currentUser.id)
        .then(({ data }) => { if (!controller.signal.aborted) setUsers(data.items ?? []) })
        .catch(error => { if (!controller.signal.aborted) setLoadError(errorText(error)) })
        .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    }, 250)
    return () => { window.clearTimeout(timer); controller.abort() }
  }, [query, status, refresh, currentUser.id])

  async function open(type: 'edit' | 'delete' | 'revoke', user: User) {
    setOpening(true)
    setError('')
    setFeedback('')
    try {
      const result = await request<User>(`/admin/users/${encodeURIComponent(user.id)}`, {}, currentUser.id)
      setAction({ type, user: result.data, etag: result.etag || resourceETag('user', user.id, result.data.revision) })
    } catch (error) { setError(errorText(error)) }
    finally { setOpening(false) }
  }

  const filtered = Boolean(query.trim() || status)
  return <section className="admin-users" aria-labelledby="users-title">
    <header className="users-heading">
      <div><h1 id="users-title">用户管理</h1><p>管理平台账号、角色与登录访问权限。</p></div>
      <Button variant="primary" disabled={opening} onClick={() => { setFeedback(''); setAction({ type: 'create' }) }}><mdui-icon name="person_add" aria-hidden="true" />新增用户</Button>
    </header>
    {(error || loadError) && <Alert tone="danger">{error || loadError} <Button onClick={() => { setError(''); setRefresh(value => value + 1) }}>重新加载</Button></Alert>}
    {feedback && <Alert tone="success">{feedback}</Alert>}
    <div className="fastui-section users-panel">
      <Toolbar label="用户筛选" className="users-toolbar">
        <div className="users-search"><mdui-icon name="search" aria-hidden="true" /><TextField type="search" aria-label="搜索用户" placeholder="搜索姓名或登录账号…" value={query} onChange={event => setQuery(event.target.value)} /></div>
        <Select aria-label="账号状态筛选" value={status} onChange={event => setStatus(event.target.value)}><option value="">全部状态</option><option value="active">启用</option><option value="disabled">停用</option></Select>
        {filtered && <Button variant="quiet" onClick={() => { setQuery(''); setStatus('') }}>清除筛选</Button>}
        <Button className="users-refresh" loading={loading} disabled={opening} onClick={() => setRefresh(value => value + 1)}><mdui-icon name="refresh" aria-hidden="true" />刷新</Button>
      </Toolbar>
      {loading ? <div className="users-placeholder"><Loading label="正在加载用户…" /></div> : loadError ? <EmptyState title="暂时无法加载用户"><p>请重试加载用户列表。</p></EmptyState> : users.length === 0 ? <EmptyState title={filtered ? '没有匹配的用户' : '暂无用户'} action={filtered ? <Button onClick={() => { setQuery(''); setStatus('') }}>清除筛选</Button> : <Button variant="primary" onClick={() => setAction({ type: 'create' })}>新增用户</Button>}><p>{filtered ? '试试其他姓名、登录账号或状态。' : '创建第一个平台账号。'}</p></EmptyState> :
        <div className="fastui-table-scroll" role="region" aria-label="用户列表，可横向滚动" tabIndex={0}>
          <table className="fastui-table users-table" aria-label="用户列表">
            <thead><tr><th scope="col">用户</th><th scope="col">角色</th><th scope="col">状态</th><th scope="col">创建时间</th><th scope="col">操作</th></tr></thead>
            <tbody>{users.map(user => <tr key={user.id}>
              <td><div className="users-identity"><span className="users-avatar" aria-hidden="true">{Array.from(user.display_name || user.identifier)[0]}</span><div><div className="users-name"><strong>{user.display_name}</strong>{user.id === currentUser.id && <span className="users-current">当前账号</span>}</div><small>{user.identifier}</small></div></div></td>
              <td>{user.role === 'admin' ? '管理员' : '用户'}</td>
              <td><Badge tone={user.status === 'active' ? 'success' : 'warning'}>{user.status === 'active' ? '启用' : '停用'}</Badge></td>
              <td><time dateTime={user.created_at}>{new Date(user.created_at).toLocaleDateString('zh-CN', { timeZone: currentUser.timezone })}</time></td>
              <td><div className="users-row-actions">
                <Button size="xs" variant="quiet" disabled={opening} aria-label={`编辑 ${user.display_name}`} onClick={() => void open('edit', user)}>编辑</Button>
                <Button size="xs" variant="quiet" disabled={opening} aria-label={`撤销 ${user.display_name} 的会话`} onClick={() => void open('revoke', user)}>撤销会话</Button>
                <Button size="xs" variant="quiet" className="users-delete" disabled={opening || user.id === currentUser.id} aria-label={`删除 ${user.display_name}`} title={user.id === currentUser.id ? '不能删除当前账号' : undefined} onClick={() => void open('delete', user)}>删除</Button>
              </div></td>
            </tr>)}</tbody>
          </table>
        </div>}
      <StatusBar className="users-status"><span>{loading ? '正在读取用户列表' : loadError ? '用户列表加载失败' : `${filtered ? '匹配' : '已加载'} ${users.length} 位用户`}</span>{opening && <Loading label="正在读取用户资料…" />}{!loading && !loadError && users.length >= 500 && <span>最多展示 500 位用户，请通过搜索缩小范围。</span>}</StatusBar>
    </div>
    {action && <UserActionDialog action={action} currentUser={currentUser} onClose={() => setAction(null)} onSaved={async (updated, message, sessionRevoked) => {
      setAction(null)
      setFeedback(message)
      if (updated?.id === currentUser.id) onCurrentUserUpdated(updated)
      if (sessionRevoked) { await onSessionRevoked(); return }
      setRefresh(value => value + 1)
    }} />}
  </section>
}

function UserActionDialog({ action, currentUser, onClose, onSaved }: {
  action: Action
  currentUser: User
  onClose: () => void
  onSaved: (updated: User | undefined, message: string, sessionRevoked: boolean) => Promise<void>
}) {
  const user = 'user' in action ? action.user : undefined
  const current = user?.id === currentUser.id
  const editing = action.type === 'create' || action.type === 'edit'
  const [identifier, setIdentifier] = useState(user?.identifier ?? '')
  const [name, setName] = useState(user?.display_name ?? '')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState(user?.role ?? 'member')
  const [status, setStatus] = useState(user?.status ?? 'active')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [createKey] = useState(idem)
  const title = { create: '新增用户', edit: '编辑用户', delete: '删除用户', revoke: '撤销会话' }[action.type]

  async function save() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      let updated: User | undefined
      let sessionRevoked = false
      if (action.type === 'create') {
        updated = (await request<User>('/admin/users', { method: 'POST', headers: { 'Idempotency-Key': createKey }, body: JSON.stringify({ identifier: identifier.trim(), display_name: name.trim(), password, role, status }) }, currentUser.id)).data
      } else {
        const path = `/admin/users/${encodeURIComponent(action.user.id)}`
        if (action.type === 'delete') await request(path, { method: 'DELETE', headers: { 'If-Match': action.etag } }, currentUser.id)
        else if (action.type === 'revoke') {
          await request(`${path}/session-revocation`, { method: 'POST' }, currentUser.id)
          sessionRevoked = current
        } else {
          const changes: Record<string, string> = {}
          if (identifier.trim().toLowerCase() !== action.user.identifier) changes.identifier = identifier.trim()
          if (name.trim() !== action.user.display_name) changes.display_name = name.trim()
          if (role !== action.user.role) changes.role = role
          if (status !== action.user.status) changes.status = status
          if (password) changes.password = password
          updated = Object.keys(changes).length ? (await request<User>(path, { method: 'PATCH', headers: { 'If-Match': action.etag }, body: JSON.stringify(changes) }, currentUser.id)).data : action.user
          sessionRevoked = current && Boolean(changes.identifier || changes.password || changes.role)
        }
      }
      await onSaved(updated, { create: '用户已创建', edit: '用户资料已保存', delete: '用户已删除', revoke: '用户会话已撤销' }[action.type], sessionRevoked)
    } catch (error) { setError(errorText(error)) }
    finally { setBusy(false) }
  }

  return <Dialog open label={title} onClose={onClose} dismissible={!busy} className="users-dialog">
    <header><h2>{title}</h2><p>{user ? `${user.display_name} · ${user.identifier}` : '创建账号后，用户可使用登录账号和密码进入工作台。'}</p></header>
    <form id="admin-user-form" className="fastui-modal-body" onSubmit={event => { event.preventDefault(); void save() }}>
      {error && <Alert tone="danger">{error}</Alert>}
      {editing ? <fieldset disabled={busy} className="users-form-fields">
        <label className="fastui-field">登录账号<TextField autoFocus required minLength={3} maxLength={120} autoComplete="off" value={identifier} onChange={event => setIdentifier(event.target.value)} /></label>
        <label className="fastui-field">姓名<TextField required maxLength={120} value={name} onChange={event => setName(event.target.value)} /></label>
        <label className="fastui-field">{user ? '重置密码（留空保持原密码）' : '初始密码'}<TextField type="password" autoComplete="new-password" required={!user} minLength={12} maxLength={200} placeholder="至少 12 个字符" value={password} onChange={event => setPassword(event.target.value)} /></label>
        <div className="users-form-columns">
          <label className="fastui-field">角色<Select aria-label="角色" value={role} disabled={current} onChange={event => setRole(event.target.value)}><option value="member">用户</option><option value="admin">管理员</option></Select></label>
          <label className="fastui-field">状态<Select aria-label="状态" value={status} disabled={current} onChange={event => setStatus(event.target.value)}><option value="active">启用</option><option value="disabled">停用</option></Select></label>
        </div>
        {current && <p className="users-help">当前账号的角色和状态不可在此修改。</p>}
        {user && <p className="users-help">修改登录账号、角色或密码将撤销现有会话，用户需要重新登录。{current && '修改当前账号的登录账号或密码后，你也需要重新登录。'}</p>}
      </fieldset> : <p className="users-confirmation">{action.type === 'delete' ? '确认删除此用户？账号将无法登录，会话与设备访问将被撤销，业务历史保留。' : `确认撤销此用户的全部登录会话？${current ? '你将退出当前账号，需要重新登录。' : '用户需要重新登录。'}`}</p>}
    </form>
    <footer><Button disabled={busy} onClick={onClose}>取消</Button><Button type="submit" form="admin-user-form" variant={editing ? 'primary' : 'danger'} loading={busy}>{busy ? '正在处理…' : editing ? '保存用户' : `确认${title}`}</Button></footer>
  </Dialog>
}
