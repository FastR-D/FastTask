import { useCallback, useEffect, useState } from 'react'
import { AgentChat, type GoalChatRequest } from './agent'
import { Today } from './Today'

type Props = {
  active: boolean
  timezone?: string
  userId?: string
  refreshVersion: number
  chatRequest?: number
  goalChatRequest?: GoalChatRequest
  onNotice: (message: string) => void
  onBusinessChanged: () => void
}

// The conversation has one permanent home, including while another application
// page is open. Responsive layout hides panes; it never remounts the runtime.
export function ResearchWorkspace({ active, timezone, userId, refreshVersion, chatRequest = 0, goalChatRequest, onNotice, onBusinessChanged }: Props) {
  const [pane, setPane] = useState<'today' | 'chat'>('today')
  const openChat = useCallback(() => setPane('chat'), [])
  useEffect(() => { if (chatRequest) setPane('chat') }, [chatRequest])

  return <div className="research-workspace" data-pane={pane} hidden={!active}>
    <nav className="research-pane-switch" aria-label="工作台视图">
      <button type="button" aria-pressed={pane==='today'} onClick={()=>setPane('today')}>今日任务</button>
      <button type="button" aria-pressed={pane==='chat'} onClick={openChat}>对话</button>
    </nav>
    <div className="research-today" aria-label="今日任务区域">
      <Today userId={userId} timezone={timezone} onNotice={onNotice} refreshVersion={refreshVersion} onBusinessChanged={onBusinessChanged} onOpenChat={openChat}/>
    </div>
    <aside className="research-chat" aria-label="科研对话区域">
      <header className="research-chat-head"><div><h2>研究助手</h2><p>说说你的目标，或现在遇到的阻碍。</p></div><mdui-icon name="forum"/></header>
      <AgentChat goalId={null} goalRequest={goalChatRequest} userId={userId} onNotice={onNotice} onBusinessChanged={onBusinessChanged} compact active={active}/>
    </aside>
  </div>
}
