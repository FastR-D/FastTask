import {
  AttachmentPrimitive,
  AssistantRuntimeProvider,
  ComposerPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
} from '@assistant-ui/react'
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ApiError, request } from '../api'
import { createCreationAttempt } from '../creation-attempt'
import { SESSION_OWNER_CHANGED } from '../session-owner'
import { createImageAttachmentAdapter } from './attachments'
import { HarnessModeControl, HarnessStatusLine } from './HarnessStatus'
import { MarkdownText, UserMarkdownText } from './Markdown'
import { ReasoningPart } from './Reasoning'
import { useAgentRuntime } from './runtime'
import type { ServerAgentState } from './state'
import { ThreadList } from './ThreadList'
import { threadState } from './threads'
import { DailyPlanProposalUI, GoalUpdateProposalUI, GoalWithTasksProposalUI, TaskTreeProposalUI } from './tools/ProposalApproval'
import { OpenToolWorkspaceUI } from './tools/OpenToolWorkspace'
import { VoiceButton } from './VoiceButton'
import { ComposerDraftPersistence, ComposerDraftUserProvider } from './ComposerDraft'
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

// emptyState is only for a new conversation or an account without history.
// A failed history read must not silently turn an existing conversation into a new one.
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
  | { kind: 'thread'; id: string; initialState?:ServerAgentState }
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

export type GoalChatRequest = { goalId:string; title:string; ownerId?:string; request:number }
type GoalChatAttempt = { goal:GoalChatRequest; attempt:ReturnType<typeof createCreationAttempt<{remote_id:string}>> }

export function AgentChat({ goalId, goalRequest, userId, onNotice, onBusinessChanged, compact = false, active = true }: { goalId: string | null; goalRequest?:GoalChatRequest; userId?: string; onNotice?: (message: string) => void; onBusinessChanged?: () => void; compact?: boolean; active?: boolean }) {
  // Every callback handed down is stable. The conversation below must not re-render because this
  // component did (see Conversation), so nothing passed to it may change identity on a re-render.
  const noticeRef = useRef(onNotice)
  noticeRef.current = onNotice
  const notify = useCallback((message: string) => noticeRef.current?.(message), [])
  const businessRef = useRef(onBusinessChanged)
  businessRef.current = onBusinessChanged
  const businessChanged = useCallback(() => businessRef.current?.(), [])
  const ownerRef = useRef(userId)
  ownerRef.current = userId
  const getUserId = useCallback(() => ownerRef.current, [])
  const [availability, setAvailability] = useState<{configured: boolean; mode: string; model_name?: string} | null>(null)
  useEffect(() => {
    if (!active) return
    let alive = true
    const load = () => {
      request<{configured: boolean; mode: string; model_name?: string}>('/agent/availability').then(({data}) => {
        if (alive && typeof data?.configured === 'boolean') setAvailability(data)
      }).catch(() => undefined)
    }
    load()
    window.addEventListener('focus', load)
    return () => { alive = false; window.removeEventListener('focus', load) }
  }, [active])
  const [view, setView] = useState<View>({ kind: 'latest' })
  const [threadId, setThreadId] = useState<string | null>(null)

  // Refresh recency and the server's first-message title fallback after a run.
  const [revision, setRevision] = useState(0)
  const threadChanged = useCallback(() => setRevision(value => value + 1), [])
  const activeId = view.kind === 'thread' ? view.id : threadId
  const activeThread=useRef(activeId)
  activeThread.current=activeId
  const goalAttempts=useRef(new Map<string,GoalChatAttempt>())
  const openingSequence=useRef(0)
  const [openingGoal,setOpeningGoal]=useState<{goal:GoalChatRequest;pending:boolean;error?:string;uncertain?:boolean}|null>(null)
  const [goalContext,setGoalContext]=useState<{threadId:string;title:string}|null>(null)

  async function openGoalChat(target:GoalChatRequest){
    const sequence=++openingSequence.current
    const key=JSON.stringify([target.ownerId??getUserId(),target.goalId])
    let entry=goalAttempts.current.get(key)
    if(!entry){
      const ownerId=target.ownerId??getUserId()
      entry={goal:{...target,ownerId},attempt:createCreationAttempt<{remote_id:string}>(ownerId,getUserId)}
      goalAttempts.current.set(key,entry)
    }
    const goal=entry.goal
    setOpeningGoal({goal:target,pending:true})
    let confirmedThreadId:string|undefined
    try{
      const created=await entry.attempt.recover('/agent/threads',{goal_id:goal.goalId,title:Array.from(goal.title).slice(0,120).join('')})
      const id=created.data.remote_id
      if(!id)throw new Error('无法确认新对话，请从会话列表核对。')
      confirmedThreadId=id
      const {data}=await request<{state:ServerAgentState}>(`/agent/threads/${encodeURIComponent(id)}/state`,{},goal.ownerId)
      if(goal.ownerId&&getUserId()!==goal.ownerId)throw new Error(SESSION_OWNER_CHANGED)
      if(data.state?.fasttask?.threadId!==id||data.state.fasttask.activeGoalId!==goal.goalId)throw new Error('对话与所选目标不一致，请重新载入后核对。')
      if(sequence!==openingSequence.current)return
      if(activeThread.current!==id)setView({kind:'thread',id,initialState:data.state})
      setThreadId(id);setGoalContext({threadId:id,title:target.title});setOpeningGoal(null);threadChanged()
    }catch(error){
      if(sequence!==openingSequence.current)return
      const missingConfirmed=Boolean(confirmedThreadId&&error instanceof ApiError&&error.status===404)
      // A definite missing confirmed resource permits a new explicit creation.
      // Unknown POST outcomes and unavailable/forbidden reads retain the attempt.
      if(missingConfirmed&&goalAttempts.current.get(key)===entry)goalAttempts.current.delete(key)
      const message=missingConfirmed?'原目标对话已不可用，重试会新建绑定这个目标的对话。':error instanceof Error?error.message:'暂时无法打开目标对话'
      setOpeningGoal({goal:target,pending:false,error:message,uncertain:entry.attempt.pending()})
      notify(message)
    }
  }
  useEffect(()=>{
    if(goalRequest)void openGoalChat(goalRequest)
    else setOpeningGoal(null)
    return()=>{openingSequence.current++}
  },[goalRequest])

  function openThread(id:string|null){
    openingSequence.current++;setOpeningGoal(null);setGoalContext(null)
    setView(id?{kind:'thread',id}:{kind:'new',nonce:Date.now()})
  }

  return (
    <ComposerDraftUserProvider userId={userId}><div className={`agent-layout${compact ? ' agent-layout-compact' : ''}`}>
      <ThreadList
        key={revision}
        activeId={activeId}
        onOpen={openThread}
        onNotice={notify}
      />
      <div className="agent-main">
        {availability?.configured === false && <p className="agent-model-status" role="status">演示模式 · 尚未配置 AI 模型。当前只提供固定演示回复，不能理解需求或创建任务；请在管理后台配置模型，或手动管理任务。</p>}
        {openingGoal&&<section aria-label="打开目标对话" aria-busy={openingGoal.pending}>
          <p role={openingGoal.error?'alert':'status'}>{openingGoal.error?`未能打开「${openingGoal.goal.title}」：${openingGoal.error}`:`正在打开「${openingGoal.goal.title}」的对话…`}</p>
          {openingGoal.uncertain&&<p className="meta">创建结果尚未确认。重试会核对同一请求，避免重复创建对话。</p>}
          <div className="actions">{!openingGoal.pending&&<mdui-button variant="outlined" onClick={()=>void openGoalChat(openingGoal.goal)}>重试打开目标对话</mdui-button>}<mdui-button variant="text" onClick={()=>{openingSequence.current++;setOpeningGoal(null)}}>返回原对话</mdui-button></div>
        </section>}
        <div className="agent-conversation-slot" hidden={Boolean(openingGoal)}>
        {goalContext?.threadId===activeId&&<p className="meta" role="status">从「{goalContext.title}」打开的对话</p>}
        <AgentConversation
          key={keyOf(view)}
          view={view}
          goalId={goalId}
          getUserId={getUserId}
          onNotice={notify}
          onThreadId={setThreadId}
          onThreadChanged={threadChanged}
          onBusinessChanged={businessChanged}
        />
        </div>
      </div>
    </div></ComposerDraftUserProvider>
  )
}

function AgentConversation({
  view,
  goalId,
  getUserId,
  onNotice,
  onThreadId,
  onThreadChanged,
  onBusinessChanged,
}: {
  view: View
  goalId: string | null
  getUserId: () => string | undefined
  onNotice: (message: string) => void
  onThreadId: (threadId: string) => void
  onThreadChanged: () => void
  onBusinessChanged: () => void
}) {
  const [initialState, setInitialState] = useState<ServerAgentState | null>(null)
  const [restoreError, setRestoreError] = useState(false)
  const [restoreAttempt, setRestoreAttempt] = useState(0)
  const goalRef = useRef(goalId)
  goalRef.current = goalId

  useEffect(() => {
    let alive = true
    setRestoreError(false)
    // 'latest' restores whichever conversation the user was last in, so a reload or a tab switch does not
    // look like a new chat; a named thread reads that thread; 'new' starts empty (§2).
    const load = view.kind === 'thread'
      ? (view.initialState?Promise.resolve(view.initialState):threadState(view.id)).then(state => ({ state, id: view.id }))
      : view.kind === 'new'
        ? Promise.resolve({ state: emptyState(goalRef.current), id: null })
        : request<{ state: ServerAgentState }>('/agent/thread-state')
          .then(({ data }) => ({
            state: data?.state ?? emptyState(goalRef.current),
            id: data?.state?.fasttask?.threadId ?? null,
          }))

    load
      .then(({ state, id }) => {
        if (!alive) return
        setInitialState(goalHint(state, goalRef.current))
        if (id) onThreadId(id)
      })
      .catch(error => {
        if (!alive) return
        setRestoreError(true)
        onNotice(`对话历史恢复失败：${error instanceof Error ? error.message : '请稍后重试'}`)
      })
    return () => {
      alive = false
    }
  }, [view, onNotice, onThreadId, restoreAttempt])

  if (restoreError) return <div className="agent-restore-error" role="alert">
    <p>暂时无法载入这段对话。请重试，或从会话列表选择其他对话。</p>
    <mdui-button variant="text" onClick={() => { setRestoreError(false); setRestoreAttempt(value => value + 1) }}>重试载入</mdui-button>
  </div>
  if (!initialState) return <div className="agent-loading">正在载入对话…</div>
  return (
    <Conversation
      initialState={initialState}
      getUserId={getUserId}
      onNotice={onNotice}
      onThreadChanged={onThreadChanged}
      onBusinessChanged={onBusinessChanged}
    />
  )
}

// Memoised on purpose, and its props must stay stable: assistant-ui's useAssistantTransportRuntime builds
// a new in-memory thread-list adapter on every render of the component that calls it, and a new adapter
// resets the thread to initialState. Re-rendering this component after a run finished — which the
// thread-list refresh did — wiped the reply the user had just watched stream in. State that changes while
// a conversation is open lives in the runtime, never in this component or its props.
const Conversation = memo(function Conversation({
  initialState,
  getUserId,
  onNotice,
  onThreadChanged,
  onBusinessChanged,
}: {
  initialState: ServerAgentState
  getUserId: () => string | undefined
  onNotice: (message: string) => void
  onThreadChanged: () => void
  onBusinessChanged: () => void
}) {
  // One adapter instance per mount: assistant-ui reloads state when an adapter identity changes.
  const attachments = useMemo(() => createImageAttachmentAdapter(), [])
  const runtime = useAgentRuntime(initialState, onNotice, { attachments, onThreadChanged, onBusinessChanged, getUserId })
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
      <GoalWithTasksProposalUI />
      <GoalUpdateProposalUI />
      <OpenToolWorkspaceUI />
      <ThreadPrimitive.Root className="agent-chat">
        <ComposerDraftPersistence thread={runtime.thread} />
        <div className="agent-harness-bar">
          <HarnessStatusLine />
          <details className="agent-advanced">
            <summary>高级选项</summary>
            <HarnessModeControl />
          </details>
        </div>
        <ThreadPrimitive.Viewport className="agent-viewport">
          <ThreadPrimitive.Empty>
            <div className="agent-empty">
              <mdui-icon name="forum" />
              <p>告诉助手你想推进的研究目标，一起找到今天能开始的小步骤。</p>
            </div>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages components={{ Message: AssistantMessage, UserMessage, AssistantMessage }} />
        </ThreadPrimitive.Viewport>
        <Composer onNotice={onNotice} />
      </ThreadPrimitive.Root>
    </AssistantRuntimeProvider>
  )
})

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
  // The browser's network flag does not establish whether a local FastTask
  // service is reachable. Keep drafts editable and let an explicit send use
  // the existing account and transport checks, which preserve failed text.
  return (
    <ComposerPrimitive.Root className="agent-composer">
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
        placeholder="说点什么…"
        rows={1}
        autoFocus
      />
      <ComposerPrimitive.AddAttachment className="agent-composer-btn agent-attach" aria-label="添加图片">
        <mdui-icon name="image" />
      </ComposerPrimitive.AddAttachment>
      <VoiceButton onNotice={onNotice} />
      <ComposerPrimitive.Cancel className="agent-composer-btn agent-cancel" aria-label="停止生成">
        <mdui-icon name="stop" />
      </ComposerPrimitive.Cancel>
      <ComposerPrimitive.Send className="agent-composer-btn agent-send" aria-label="发送">
        <mdui-icon name="arrow_upward" />
      </ComposerPrimitive.Send>
    </ComposerPrimitive.Root>
  )
}
