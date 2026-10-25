import { useEffect, useRef, useState } from 'react'
import { request, token } from './api'
import { assertAccessOwner, SESSION_OWNER_CHANGED } from './session-owner'
import type { User } from './types'
import { launchModuleWorkspace, workspaceURL } from './workspace-navigation'
import './module-workspace-link.css'

type Props = { url: string; system?: string; label: string; userId?: string; onNotice?: (message: string) => void; appearance?: 'button' | 'link'; disabled?: boolean }

export function ModuleWorkspaceLink({ url, system, label, userId, onNotice, appearance = 'button', disabled = false }: Props) {
  const [busy, setBusy] = useState(false), [message, setMessage] = useState('')
  const owner = useRef(userId), running = useRef(false), operation = useRef(0), abort = useRef<AbortController | null>(null)
  owner.current = userId
  useEffect(() => { setMessage(''); setBusy(false); running.current = false; return () => { operation.current++; abort.current?.abort() } }, [userId])
  const destination = workspaceURL(url, system)
  async function open() {
    if (disabled || running.current || !destination || !userId) return
    const account = userId, current = ++operation.current, controller = new AbortController()
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(15_000)])
    abort.current = controller; running.current = true; setBusy(true); setMessage('')
    const guard = () => { if (owner.current !== account || operation.current !== current) throw new Error(SESSION_OWNER_CHANGED); assertAccessOwner(token.get(), account) }
    try {
      const result = await launchModuleWorkspace({ url: destination, system, ownerId: account }, {
        openPopup: () => window.open('about:blank', '_blank'), assertOwner: guard,
        readFastTaskIdentity: async () => (await request<User>('/me', {signal}, account)).data,
        fetchHub: (input, init) => fetch(input, init), signal,
      })
      if (owner.current === account && operation.current === current && result.notice) { setMessage(result.notice); onNotice?.(result.notice) }
    } catch (error) {
      if (owner.current !== account || operation.current !== current || controller.signal.aborted) return
      const text = error instanceof Error ? error.message : '暂时无法打开模块，请重试。'
      setMessage(text); onNotice?.(text)
    } finally { if (owner.current === account && operation.current === current) { setBusy(false); running.current = false; abort.current = null } }
  }
  if (!destination) return <span className="meta" role="alert">{label} 网页暂不可用</span>
  const unavailable = disabled || busy || !userId
  return <span className="module-workspace-link">
    {appearance === 'link' ? <a className="agent-workspace-action" href={destination} target="_blank" rel="noopener noreferrer" aria-disabled={unavailable} onClick={event => { event.preventDefault(); void open() }} onAuxClick={event => { if (event.button === 1) { event.preventDefault(); void open() } }}>{busy ? '正在打开…' : `打开 ${label}`}<mdui-icon name="open_in_new"/></a> : <mdui-button variant="text" icon="open_in_new" disabled={unavailable} loading={busy} onClick={() => void open()}>打开 {label}</mdui-button>}
    {message && <span className="module-workspace-message meta" role="alert">{message}</span>}
  </span>
}
