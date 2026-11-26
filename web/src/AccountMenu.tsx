import '@fastrd/ui-foundation/controls.css'
import { Button } from '@fastrd/ui-react'
import { useEffect, useId, useRef, useState, type ReactNode } from 'react'
import './AccountMenu.css'

export type AccountAction = { label: string; onSelect: () => void; icon?: ReactNode; danger?: boolean; disabled?: boolean }
export function AccountMenu({ name, detail, actions, label = '账号菜单' }: { name: string; detail?: string; actions: AccountAction[]; label?: string }) {
  const [open, setOpen] = useState(false)
  const root = useRef<HTMLDivElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const id = useId()
  const cancel = () => clearTimeout(timer.current)
  useEffect(() => {
    if (!open) return
    const outside = (event: PointerEvent) => { if (!root.current?.contains(event.target as Node)) setOpen(false) }
    const escape = (event: KeyboardEvent) => { if (event.key === 'Escape') { setOpen(false); if (root.current?.contains(document.activeElement)) trigger.current?.focus() } }
    document.addEventListener('keydown', escape)
    document.addEventListener('pointerdown', outside)
    return () => { document.removeEventListener('pointerdown', outside); document.removeEventListener('keydown', escape) }
  }, [open])
  useEffect(() => () => clearTimeout(timer.current), [])
  function focusItem(last = false) {
    requestAnimationFrame(() => {
      const items = root.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)')
      items?.[last ? items.length - 1 : 0]?.focus()
    })
  }
  return <div className="rd-account-menu" ref={root}
    onPointerEnter={event => { if (event.pointerType === 'mouse') { cancel(); setOpen(true) } }}
    onPointerLeave={event => { if (event.pointerType === 'mouse') { cancel(); timer.current = setTimeout(() => { if (!root.current?.contains(document.activeElement)) setOpen(false) }, 180) } }}
    onBlur={event => { if (!event.currentTarget.contains(event.relatedTarget)) setOpen(false) }}
    onKeyDown={event => { if (event.key === 'Escape' && open) { event.stopPropagation(); setOpen(false); trigger.current?.focus() } }}>
    <Button ref={trigger} variant="quiet" className="rd-account-trigger" aria-label={label} aria-haspopup="menu" aria-expanded={open} aria-controls={open ? id : undefined}
      onClick={() => { cancel(); setOpen(true) }}
      onKeyDown={event => { if (event.key === 'ArrowDown' || event.key === 'ArrowUp') { event.preventDefault(); setOpen(true); focusItem(event.key === 'ArrowUp') } }}>
      <span className="rd-account-avatar" aria-hidden="true">{Array.from(name.trim())[0] || 'F'}</span>
      <span className="rd-account-trigger-name">{name}</span>
    </Button>
    {open && <div className="rd-account-popover" id={id} role="menu" aria-label={label}
      onKeyDown={event => {
        const items = Array.from(event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)'))
        const current = items.indexOf(document.activeElement as HTMLButtonElement)
        const index = event.key === 'Home' ? 0 : event.key === 'End' ? items.length - 1 : event.key === 'ArrowDown' ? (current + 1) % items.length : event.key === 'ArrowUp' ? (current + items.length - 1) % items.length : -1
        if (index >= 0) { event.preventDefault(); items[index]?.focus() }
      }}>
      <div className="rd-account-identity" role="presentation"><strong>{name}</strong>{detail && <small>{detail}</small>}</div>
      {actions.map(action => <Button key={action.label} role="menuitem" variant="quiet" className={action.danger ? 'rd-account-action rd-account-danger' : 'rd-account-action'} disabled={action.disabled}
        onClick={() => { setOpen(false); trigger.current?.focus(); action.onSelect() }}>{action.icon}{action.label}</Button>)}
    </div>}
  </div>
}
