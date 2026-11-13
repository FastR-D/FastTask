import type { CSSProperties, KeyboardEvent, PointerEvent, ReactNode } from 'react'
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { AgentChat, type GoalChatRequest } from './agent'
import { ASSISTANT_RATIO_KEY, ASSISTANT_WIDTH_KEY, DEFAULT_ASSISTANT_RATIO, MIN_PANE_WIDTH, RESIZER_WIDTH, assistantBounds, assistantWidth, parseAssistantRatio, parseLegacyAssistantWidth, ratioAtPointer, ratioForWidth } from './research-split'

export function readResearchAssistantOpen(): boolean {
  // Each entry starts with the assistant open. Closing it remains a session action.
  return true
}

function readSplitPreference() {
  try {
    const ratio = parseAssistantRatio(window.localStorage.getItem(ASSISTANT_RATIO_KEY))
    return { ratio: ratio ?? DEFAULT_ASSISTANT_RATIO, legacyWidth: ratio === null ? parseLegacyAssistantWidth(window.localStorage.getItem(ASSISTANT_WIDTH_KEY)) : null }
  } catch {
    return { ratio: DEFAULT_ASSISTANT_RATIO, legacyWidth: null }
  }
}

function persistRatio(ratio: number) {
  try { window.localStorage.setItem(ASSISTANT_RATIO_KEY, String(ratio)) } catch { /* local preferences are optional */ }
}

type Props = {
  children: ReactNode
  userId?: string
  assistantOpen: boolean
  assistantFocusRequest?: number
  goalChatRequest?: GoalChatRequest
  onAssistantClose: () => void
  onNotice: (message: string) => void
  onBusinessChanged: () => void
}

// The assistant is a workbench-level pane. Business pages are replaced inside the page viewport,
// while this component and AgentChat remain mounted so the runtime, draft and scroll position survive
// navigation between pages.
export function ResearchWorkspace({ children, userId, assistantOpen, assistantFocusRequest = 0, goalChatRequest, onAssistantClose, onNotice, onBusinessChanged }: Props) {
  const [preference] = useState(readSplitPreference)
  const [ratio, setRatio] = useState(preference.ratio)
  const [workspaceWidth, setWorkspaceWidth] = useState(0)
  const [resizing, setResizing] = useState(false)
  const workspaceRef = useRef<HTMLDivElement>(null)
  const assistantRef = useRef<HTMLElement>(null)
  const separatorRef = useRef<HTMLDivElement>(null)
  const ratioRef = useRef(ratio)
  const legacyWidthRef = useRef(preference.legacyWidth)
  const drag = useRef<{ pointerId: number; grabOffset: number } | null>(null)
  const endDragRef = useRef<() => void>(() => {})

  function updateRatio(next: number) {
    legacyWidthRef.current = null
    ratioRef.current = next
    setRatio(next)
    persistRatio(next)
  }

  useLayoutEffect(() => {
    const workspace = workspaceRef.current
    if (!workspace) return
    const measure = () => {
      const width = workspace.clientWidth
      setWorkspaceWidth(width)
      // Migrate an earlier pixel preference only once the two panes can actually
      // fit. A mobile overlay must not turn a desktop width into a new preference.
      if (legacyWidthRef.current !== null && width >= MIN_PANE_WIDTH * 2 + RESIZER_WIDTH && separatorRef.current?.getBoundingClientRect().width) {
        const next = ratioForWidth(width, legacyWidthRef.current)
        legacyWidthRef.current = null
        ratioRef.current = next
        setRatio(next)
        persistRatio(next)
      }
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(workspace)
    window.addEventListener('resize', measure)
    return () => { observer.disconnect(); window.removeEventListener('resize', measure) }
  }, [assistantOpen])

  useEffect(() => {
    const move = (event: globalThis.PointerEvent) => {
      const current = drag.current
      const workspace = workspaceRef.current
      if (!current || !workspace || current.pointerId !== event.pointerId) return
      if (!separatorRef.current?.getBoundingClientRect().width) { end(); return }
      const rect = workspace.getBoundingClientRect()
      const next = ratioAtPointer(rect.width, rect.right, event.clientX, current.grabOffset)
      legacyWidthRef.current = null
      ratioRef.current = next
      setRatio(next)
    }
    const end = () => {
      const current = drag.current
      if (!current) return
      drag.current = null
      setResizing(false)
      const separator = separatorRef.current
      if (separator?.hasPointerCapture?.(current.pointerId)) separator.releasePointerCapture(current.pointerId)
      persistRatio(ratioRef.current)
    }
    const finishPointer = (event: globalThis.PointerEvent) => { if (drag.current?.pointerId === event.pointerId) end() }
    const escape = (event: globalThis.KeyboardEvent) => { if (event.key === 'Escape') end() }
    endDragRef.current = end
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', finishPointer)
    window.addEventListener('pointercancel', finishPointer)
    window.addEventListener('blur', end)
    window.addEventListener('resize', end)
    window.addEventListener('keydown', escape)
    return () => {
      end()
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', finishPointer)
      window.removeEventListener('pointercancel', finishPointer)
      window.removeEventListener('blur', end)
      window.removeEventListener('resize', end)
      window.removeEventListener('keydown', escape)
      endDragRef.current = () => {}
    }
  }, [])

  useEffect(() => { if (!assistantOpen) endDragRef.current() }, [assistantOpen])

  useLayoutEffect(() => {
    if (!assistantOpen || !assistantFocusRequest) return
    const pane = assistantRef.current
    // Focus the region rather than opening the mobile keyboard before the user starts typing.
    pane?.scrollIntoView({ block: 'start', inline: 'nearest', behavior: 'instant' })
    pane?.focus({ preventScroll: true })
  }, [assistantOpen, assistantFocusRequest])

  function startResize(event: PointerEvent<HTMLDivElement>) {
    if (event.button !== 0 || !assistantOpen || workspaceWidth < MIN_PANE_WIDTH * 2 + RESIZER_WIDTH || drag.current) return
    const rect = event.currentTarget.getBoundingClientRect()
    if (!rect.width) return
    event.preventDefault()
    // Anchor to the rendered separator, not the preferred width that may have
    // been limited by the current window size.
    drag.current = { pointerId: event.pointerId, grabOffset: event.clientX - (rect.left + rect.width / 2) }
    event.currentTarget.focus({ preventScroll: true })
    event.currentTarget.setPointerCapture(event.pointerId)
    setResizing(true)
  }

  const bounds = assistantBounds(workspaceWidth)
  const effectiveAssistantWidth = assistantWidth(workspaceWidth, ratio)
  function resizeByKeyboard(event: KeyboardEvent<HTMLDivElement>) {
    if (!assistantOpen || !separatorRef.current?.getBoundingClientRect().width) return
    const step = event.shiftKey ? 48 : 16
    let requested: number
    switch (event.key) {
      case 'ArrowLeft': requested = effectiveAssistantWidth + step; break
      case 'ArrowRight': requested = effectiveAssistantWidth - step; break
      case 'Home': requested = bounds.min; break
      case 'End': requested = bounds.max; break
      case 'Enter': event.preventDefault(); updateRatio(DEFAULT_ASSISTANT_RATIO); return
      default: return
    }
    event.preventDefault()
    updateRatio(ratioForWidth(workspaceWidth, requested))
  }

  const percent = bounds.total ? Math.round(effectiveAssistantWidth / bounds.total * 100) : 40
  const style = { '--research-assistant-width': `${effectiveAssistantWidth}px` } as CSSProperties
  return <div ref={workspaceRef} className="research-workspace" data-assistant-open={assistantOpen} data-assistant-resizing={resizing} style={style}>
    <main id="research-page" className="research-page-viewport" aria-label="当前工作页面">
      {children}
    </main>
    <button className="research-assistant-backdrop" type="button" aria-label="关闭科研助手" tabIndex={assistantOpen ? 0 : -1} onClick={onAssistantClose}/>
    <div
      ref={separatorRef}
      className="research-assistant-resizer"
      role="separator"
      aria-label="调整科研助手宽度"
      aria-orientation="vertical"
      aria-controls="research-page research-assistant"
      aria-valuemin={Math.round(bounds.min)}
      aria-valuemax={Math.round(bounds.max)}
      aria-valuenow={Math.round(effectiveAssistantWidth)}
      aria-valuetext={`科研助手 ${percent}%，工作页面 ${100 - percent}%`}
      title="左右拖动调整比例；方向键微调；双击或按 Enter 恢复默认"
      tabIndex={assistantOpen ? 0 : -1}
      onPointerDown={startResize}
      onLostPointerCapture={() => endDragRef.current()}
      onKeyDown={resizeByKeyboard}
      onDoubleClick={() => updateRatio(DEFAULT_ASSISTANT_RATIO)}
    />
    <aside ref={assistantRef} id="research-assistant" className="research-chat" aria-label="科研对话区域" aria-hidden={!assistantOpen} tabIndex={-1}>
      <header className="research-chat-head">
        <div><h2>科研助手</h2><p>说说你的目标，或现在遇到的阻碍。</p></div>
        <div className="research-chat-head-actions">
          <mdui-icon name="forum"/>
          <mdui-button-icon icon="close" aria-label="收起科研助手" onClick={onAssistantClose}/>
        </div>
      </header>
      <AgentChat goalId={null} goalRequest={goalChatRequest} userId={userId} onNotice={onNotice} onBusinessChanged={onBusinessChanged} compact active/>
    </aside>
  </div>
}
