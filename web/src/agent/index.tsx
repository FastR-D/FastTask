import {
  AttachmentPrimitive,
  AssistantRuntimeProvider,
  ComposerPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
} from '@assistant-ui/react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { request } from '../api'
import { createImageAttachmentAdapter } from './attachments'
import { HarnessModeControl, HarnessStatusLine, useOnline } from './HarnessStatus'
import { MarkdownText, UserMarkdownText } from './Markdown'
import { ReasoningPart } from './Reasoning'
import { useAgentRuntime } from './runtime'
import type { ServerAgentState } from './state'
import { ThreadList } from './ThreadList'
import { threadState } from './threads'
import { DailyPlanProposalUI, TaskTreeProposalUI } from './tools/ProposalApproval'
import { VoiceButton } from './VoiceButton'
import '../agent.css'

// The agent chat view (doc/frontend.md §2, doc/chat-features.md §2, §3, §4).
//
// Everything inside lives in web/src/agent/ and is styled with mdui tokens only (ADR-0004). The runtime
// is created only after the server state arrives: assistant-ui keeps the conversation in memory, so a mount
// that starts from an empty initialState silently discards what the user already said.
//
// Multi-session is a view keyed by thread. Switching conversation remounts the runtime with that thread's
// authoritative state rather than mutating a live one, because a transport runtime's initialState is exactly
// that — initial (doc/chat-features.md §2.1).

// emptyState is the conversation a user with no agent history starts from. It is also the fallback when a
// restore read fails, so a failed fetch degrades to a usable empty chat rather than a stuck loading state.
function emptyState(goalId: string | null): ServerAgentState {
  return {
    messages: [],
    isRunning: false,
    fasttask: goalId ? { activeGoalId: goalId, pendingProposals: [] } : { pendingProposals: [] },
  }
}

// goalHint keeps the mount prop's optimistic active-goal hint (frontend.md §2) when the restored snapshot
// does not carry one yet. The server stays the source of truth and pushes fasttask.activeGoalId back over
// the stream (§2.7).
function goalHint(state: ServerAgentState, goalId: string | null): ServerAgentState {
  if (!goalId || state.fasttask?.activeGoalId) return state
  return { ...state, fasttask: { ...state.fasttask, activeGoalId: goalId } }
}

/** Which conversation the view is showing. */
type View =
  | { kind: 'latest' }
  | { kind: 'thread'; id: string }
  | { kind: 'new'; nonce: number }

function keyOf(view: View): string {
  switch (view.kind) {
    case 'thread':
      return view.id
    case 'new':
      return `new-${view.nonce}`
    default:
      return 'latest'
  }
}

export function AgentChat({ goalId, onNotice }: { goalId: string | null; onNotice?: (message: string) => void }) {
  const notify = onNotice ?? (() => {})
  const [view, setView] = useState<View>({ kind: 'latest' })
  const [threadId, setThreadId] = useState<string | null>(null)

  // A finished run gives the thread a title and moves it to the top of the list; bumping this re-reads it.
  const [revision, setRevision] = useState(0)
  const activeId = view.kind === 'thread' ? view.id : threadId

  return (
    <div className="agent-layout">
      <ThreadList
        key={revision}
        activeId={activeId}
        onOpen={id => setView(id ? { kind: 'thread', id } : { kind: 'new', nonce: Date.now() })}
        onNotice={notify}
      />
      <div className="agent-main">
        <AgentConversation
          key={keyOf(view)}
          view={view}
          goalId={goalId}
          onNotice={notify}
          onThreadId={setThreadId}
          onThreadChanged={() => setRevision(value => value + 1)}
        />
      </div>
    </div>
  )
}

function AgentConversation({
  view,
  goalId,
  onNotice,
  onThreadId,
  onThreadChanged,
}: {
  view: View
  goalId: string | null
  onNotice: (message: string) => void
  onThreadId: (threadId: string) => void
  onThreadChanged: () => void
}) {
  const [initialState, setInitialState] = useState<ServerAgentState | null>(null)

  useEffect(() => {
    let alive = true
    // 'latest' restores whichever conversation the user was last in, so a reload or a tab switch does not
    // look like a new chat; a named thread reads that thread; 'new' starts empty (§2).
    const load = view.kind === 'thread'
      ? threadState(view.id).then(state => ({ state, id: view.id }))
      : view.kind === 'new'
        ? Promise.resolve({ state: emptyState(goalId), id: null })
        : request<{ state: ServerAgentState }>('/agent/thread-state')
          .then(({ data }) => ({
            state: data?.state ?? emptyState(goalId),
            id: data?.state?.fasttask?.threadId ?? null,
          }))

    load
      .then(({ state, id }) => {
        if (!alive) return
        setInitialState(goalHint(state, goalId))
        if (id) onThreadId(id)
      })
      .catch(error => {
        if (!alive) return
        setInitialState(emptyState(goalId))
        onNotice(`对话历史恢复失败：${error instanceof Error ? error.message : '请稍后重试'}`)
      })
    return () => {
      alive = false
    }
  }, [view, goalId, onNotice, onThreadId])

  if (!initialState) return <div className="agent-loading">正在载入对话…</div>
  return (
    <Conversation
      initialState={initialState}
      onNotice={onNotice}
      onThreadChanged={onThreadChanged}
    />
  )
}

function Conversation({
  initialState,
  onNotice,
  onThreadChanged,
}: {
  initialState: ServerAgentState
  onNotice: (message: string) => void
  onThreadChanged: () => void
}) {
  // One adapter instance per mount: assistant-ui reloads state when an adapter identity changes.
  const attachments = useMemo(() => createImageAttachmentAdapter(), [])
  const runtime = useAgentRuntime(initialState, onNotice, { attachments, onThreadChanged })
  const resumed = useRef(false)
  useEffect(() => {
    // A run keeps executing after the client disconnects (agent-impl.md §2.8), so a restored conversation
    // that is still running re-attaches its stream once. resumeRun is typed void: transport failures arrive
    // through the runtime's onError, which already reports them, so only a synchronous throw is caught.
    if (!initialState.isRunning || resumed.current) return
    resumed.current = true
    try {
      runtime.thread.resumeRun({ parentId: null })
    } catch (error) {
      onNotice(`对话续流失败：${error instanceof Error ? error.message : '请稍后重试'}`)
    }
  }, [runtime, initialState.isRunning, onNotice])

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      {/* Rendering these registers the proposal approval cards by tool name (§7.1). */}
      <TaskTreeProposalUI />
      <DailyPlanProposalUI />
      <ThreadPrimitive.Root className="agent-chat">
        <div className="agent-harness-bar">
          <HarnessStatusLine />
          <HarnessModeControl />
        </div>
        <ThreadPrimitive.Viewport className="agent-viewport">
          <ThreadPrimitive.Empty>
            <div className="agent-empty">
              <mdui-icon name="forum" />
              <p>和 agent 说说今天的目标，它会帮你把研究拆成可执行、可验证的小步。</p>
            </div>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages components={{ Message: AssistantMessage, UserMessage, AssistantMessage }} />
        </ThreadPrimitive.Viewport>
        <Composer onNotice={onNotice} />
      </ThreadPrimitive.Root>
    </AssistantRuntimeProvider>
  )
}

function UserMessage() {
  return (
    <MessagePrimitive.Root className="agent-msg agent-msg-user">
      <div className="agent-bubble agent-bubble-markdown">
        <MessagePrimitive.Parts components={{ Text: UserMarkdownText, Image: AttachedImage, Reasoning: ReasoningPart }} />
      </div>
    </MessagePrimitive.Root>
  )
}

function AssistantMessage() {
  return (
    <MessagePrimitive.Root className="agent-msg agent-msg-assistant">
      <div className="agent-bubble agent-bubble-markdown">
        <MessagePrimitive.Parts components={{ Text: MarkdownText, Image: AttachedImage, Reasoning: ReasoningPart }} />
      </div>
    </MessagePrimitive.Root>
  )
}

/** AttachedImage renders a stored attachment through the owner-scoped endpoint (doc/chat-features.md §4.4). */
function AttachedImage({ image }: { image?: string }) {
  const src = typeof image === 'string' ? image : ''
  if (!src) return null
  return <img className="agent-attachment" src={src} alt="附件" loading="lazy" />
}

function Composer({ onNotice }: { onNotice: (message: string) => void }) {
  // An offline PWA can still render the cached conversation, but it cannot start a run: the model call needs
  // a network. The entry is greyed out and says why, rather than accepting a message that will fail
  // (doc/harness.md §9.3).
  const online = useOnline()
  return (
    <ComposerPrimitive.Root className={`agent-composer${online ? '' : ' offline'}`}>
      <ComposerPrimitive.Attachments>
        {({ attachment }) => (
          <AttachmentPrimitive.Root className="agent-composer-attachment">
            {/* No thumbnail here on purpose: the message renders the stored image once it is sent, and a
                second copy from an object URL would be a second thing to revoke. */}
            <span className="agent-composer-attachment-name">
              <mdui-icon name="image" />
              <AttachmentPrimitive.Name />
            </span>
            <AttachmentPrimitive.Remove className="agent-composer-attachment-remove" aria-label="移除附件">
              <mdui-icon name="close" />
            </AttachmentPrimitive.Remove>
          </AttachmentPrimitive.Root>
        )}
      </ComposerPrimitive.Attachments>
      <ComposerPrimitive.Input
        className="agent-input"
        placeholder={online ? '说点什么…（Enter 发送，Shift+Enter 换行）' : '离线中，联网后才能继续对话'}
        rows={1}
        autoFocus
        disabled={!online}
      />
      <ComposerPrimitive.AddAttachment className="agent-composer-btn agent-attach" aria-label="添加图片">
        <mdui-icon name="image" />
      </ComposerPrimitive.AddAttachment>
      <VoiceButton onNotice={onNotice} />
      <ComposerPrimitive.Cancel className="agent-composer-btn agent-cancel" aria-label="停止生成">
        <mdui-icon name="stop" />
      </ComposerPrimitive.Cancel>
      <ComposerPrimitive.Send className="agent-composer-btn agent-send" aria-label="发送" disabled={!online}>
        <mdui-icon name="arrow_upward" />
      </ComposerPrimitive.Send>
    </ComposerPrimitive.Root>
  )
}
