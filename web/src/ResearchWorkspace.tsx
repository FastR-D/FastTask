import type { CSSProperties, KeyboardEvent, PointerEvent, ReactNode } from 'react'
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { AgentChat, type GoalChatRequest } from './agent'

const ASSISTANT_OPEN_KEY = 'fasttask.research-assistant.open'
const ASSISTANT_WIDTH_KEY = 'fasttask.research-assistant.width'
const MIN_ASSISTANT_WIDTH = 320
const MAX_ASSISTANT_WIDTH = 800
const DEFAULT_ASSISTANT_WIDTH = 400
const RESIZER_WIDTH = 8

export function readResearchAssistantOpen(): boolean {
  try {
    const value = window.localStorage.getItem(ASSISTANT_OPEN_KEY)
    return value === null ? true : value === 'true'
  } catch {
    return true
  }
}

function readAssistantWidth(): number {
  try {
    const stored = window.localStorage.getItem(ASSISTANT_WIDTH_KEY)
    if (stored === null) return DEFAULT_ASSISTANT_WIDTH
    const value = Number(stored)
    return Number.isFinite(value) ? clampWidth(value) : DEFAULT_ASSISTANT_WIDTH
  } catch {
    return DEFAULT_ASSISTANT_WIDTH
  }
}

function clampWidth(value: number, availableMax = MAX_ASSISTANT_WIDTH): number {
  const max = Math.max(MIN_ASSISTANT_WIDTH, Math.min(MAX_ASSISTANT_WIDTH, Math.round(availableMax)))
  return Math.min(max, Math.max(MIN_ASSISTANT_WIDTH, Math.round(value)))
}

type Props = {
  children: ReactNode
  userId?: string
  assistantOpen: boolean
  goalChatRequest?: GoalChatRequest
  onAssistantClose: () => void
  onNotice: (message: string) => void
  onBusinessChanged: () => void
}

// The assistant is a workbench-level pane. Business pages are replaced inside the page viewport,
// while this component and AgentChat remain mounted so the runtime, draft and scroll position survive
// navigation between pages.
export function ResearchWorkspace({ children, userId, assistantOpen, goalChatRequest, onAssistantClose, onNotice, onBusinessChanged }: Props) {
  const [assistantWidth, setAssistantWidth] = useState(readAssistantWidth)
  const [availableMaxWidth, setAvailableMaxWidth] = useState(MAX_ASSISTANT_WIDTH)
  const [resizing, setResizing] = useState(false)
  const workspaceRef = useRef<HTMLDivElement>(null)
  const resizeStart = useRef<{ x: number; width: number } | null>(null)

  useLayoutEffect(() => {
    const workspace = workspaceRef.current
    if (!workspace) return
    const measure = () => setAvailableMaxWidth(Math.max(MIN_ASSISTANT_WIDTH, Math.min(MAX_ASSISTANT_WIDTH, workspace.clientWidth - MIN_ASSISTANT_WIDTH - RESIZER_WIDTH)))
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(workspace)
    return () => observer.disconnect()
  }, [])

  useEffect(() => {
    try { window.localStorage.setItem(ASSISTANT_WIDTH_KEY, String(assistantWidth)) } catch { /* local preferences are optional */ }
  }, [assistantWidth])

  useEffect(() => {
    const move = (event: globalThis.PointerEvent) => {
      if (!resizeStart.current) return
      setAssistantWidth(clampWidth(resizeStart.current.width - (event.clientX - resizeStart.current.x), availableMaxWidth))
    }
    const end = () => { resizeStart.current = null; setResizing(false) }
    window.addEventListener('pointermove', move)
    window.addEventListener('pointerup', end)
    window.addEventListener('pointercancel', end)
    return () => {
      window.removeEventListener('pointermove', move)
      window.removeEventListener('pointerup', end)
      window.removeEventListener('pointercancel', end)
    }
  }, [availableMaxWidth])

  function startResize(event: PointerEvent<HTMLDivElement>) {
    if (event.button !== 0) return
    event.preventDefault()
    resizeStart.current = { x: event.clientX, width: clampWidth(assistantWidth, availableMaxWidth) }
    setResizing(true)
  }

  function resizeByKeyboard(event: KeyboardEvent<HTMLDivElement>) {
    const step = event.shiftKey ? 48 : 16
    if (event.key === 'ArrowLeft') {
      event.preventDefault()
      setAssistantWidth(value => clampWidth(clampWidth(value, availableMaxWidth) + step, availableMaxWidth))
    } else if (event.key === 'ArrowRight') {
      event.preventDefault()
      setAssistantWidth(value => clampWidth(clampWidth(value, availableMaxWidth) - step, availableMaxWidth))
    } else if (event.key === 'Home') {
      event.preventDefault()
      setAssistantWidth(MIN_ASSISTANT_WIDTH)
    } else if (event.key === 'End') {
      event.preventDefault()
      setAssistantWidth(availableMaxWidth)
    }
  }

  const effectiveAssistantWidth = clampWidth(assistantWidth, availableMaxWidth)
  const style = { '--research-assistant-width': `${effectiveAssistantWidth}px` } as CSSProperties
  return <div ref={workspaceRef} className="research-workspace" data-assistant-open={assistantOpen} data-assistant-resizing={resizing} style={style}>
    <main className="research-page-viewport" aria-label="当前工作页面">
      {children}
    </main>
    <button className="research-assistant-backdrop" type="button" aria-label="关闭研究助手" tabIndex={assistantOpen ? 0 : -1} onClick={onAssistantClose}/>
    <div
      className="research-assistant-resizer"
      role="separator"
      aria-label="调整研究助手宽度"
      aria-orientation="vertical"
      aria-valuemin={MIN_ASSISTANT_WIDTH}
      aria-valuemax={availableMaxWidth}
      aria-valuenow={effectiveAssistantWidth}
      tabIndex={assistantOpen ? 0 : -1}
      onPointerDown={startResize}
      onKeyDown={resizeByKeyboard}
    />
    <aside id="research-assistant" className="research-chat" aria-label="科研对话区域" aria-hidden={!assistantOpen}>
      <header className="research-chat-head">
        <div><h2>研究助手</h2><p>说说你的目标，或现在遇到的阻碍。</p></div>
        <div className="research-chat-head-actions">
          <mdui-icon name="forum"/>
          <mdui-button-icon icon="close" aria-label="收起研究助手" onClick={onAssistantClose}/>
        </div>
      </header>
      <AgentChat goalId={null} goalRequest={goalChatRequest} userId={userId} onNotice={onNotice} onBusinessChanged={onBusinessChanged} compact active/>
    </aside>
  </div>
}
