import {
  AssistantRuntimeProvider,
  ComposerPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
} from '@assistant-ui/react'
import { useEffect, useRef, useState } from 'react'
import { request } from '../api'
import { useAgentRuntime } from './runtime'
import type { ServerAgentState } from './state'
import { DailyPlanProposalUI, TaskTreeProposalUI } from './tools/ProposalApproval'
import { VoiceButton } from './VoiceButton'
import '../agent.css'

// emptyState is the conversation a user with no agent history starts from. It is
// also the fallback when the restore read fails, so a failed fetch degrades to a
// usable empty chat rather than a view stuck on its loading state.
function emptyState(goalId: string | null): ServerAgentState {
  return {
    messages: [],
    isRunning: false,
    fasttask: goalId ? { activeGoalId: goalId, pendingProposals: [] } : { pendingProposals: [] },
  }
}

// goalHint keeps the mount prop's optimistic active-goal hint (frontend.md §2)
// when the restored snapshot does not carry one yet. The server stays the source
// of truth and pushes fasttask.activeGoalId back over the stream (§2.7).
function goalHint(state: ServerAgentState, goalId: string | null): ServerAgentState {
  if (!goalId || state.fasttask?.activeGoalId) return state
  return { ...state, fasttask: { ...state.fasttask, activeGoalId: goalId } }
}

// AgentChat is the workflow-B mount point (frontend.md §2). It自带
// AssistantRuntimeProvider, so workflow A only places <AgentChat goalId={...} /> in
// the layout and never wraps it. Everything inside lives in web/src/agent/ and is
// styled with mdui tokens only (ADR-0004).
//
// The runtime is created only after the server state arrives. assistant-ui keeps
// the conversation in memory, so a mount that starts from an empty initialState
// silently discards everything the user already said — which is what a tab switch
// or a reload used to do. GET /agent/thread-state is the restore read.
export function AgentChat({ goalId, onNotice }: { goalId: string | null; onNotice?: (message: string) => void }) {
  const [initialState, setInitialState] = useState<ServerAgentState | null>(null)
  useEffect(() => {
    let alive = true
    request<{ state: ServerAgentState }>('/agent/thread-state')
      .then(({ data }) => {
        if (alive) setInitialState(goalHint(data?.state ?? emptyState(goalId), goalId))
      })
      .catch(error => {
        if (!alive) return
        setInitialState(emptyState(goalId))
        onNotice?.(`对话历史恢复失败：${error instanceof Error ? error.message : '请稍后重试'}`)
      })
    return () => { alive = false }
  }, [goalId, onNotice])
  if (!initialState) return <div className="agent-loading">正在载入对话…</div>
  return <AgentConversation initialState={initialState} onNotice={onNotice} />
}

function AgentConversation({ initialState, onNotice }: { initialState: ServerAgentState; onNotice?: (message: string) => void }) {
  const notify = onNotice ?? (() => {})
  const runtime = useAgentRuntime(initialState, notify)
  const resumed = useRef(false)
  useEffect(() => {
    // A run keeps executing after the client disconnects (agent-impl.md §2.8), so
    // a restored conversation that is still running re-attaches its stream once.
    // resumeRun is typed void: transport failures arrive through the runtime's
    // onError, which already reports them, so only a synchronous throw is caught.
    if (!initialState.isRunning || resumed.current) return
    resumed.current = true
    try {
      runtime.thread.resumeRun({ parentId: null })
    } catch (error) {
      notify(`对话续流失败：${error instanceof Error ? error.message : '请稍后重试'}`)
    }
  }, [runtime, initialState.isRunning, notify])
  return (
    <AssistantRuntimeProvider runtime={runtime}>
      {/* Rendering these registers the proposal approval cards by tool name (§7.1). */}
      <TaskTreeProposalUI />
      <DailyPlanProposalUI />
      <ThreadPrimitive.Root className="agent-chat">
        <ThreadPrimitive.Viewport className="agent-viewport">
          <ThreadPrimitive.Empty>
            <div className="agent-empty">
              <mdui-icon name="forum" />
              <p>和 agent 说说今天的目标，它会帮你把研究拆成可执行、可验证的小步。</p>
            </div>
          </ThreadPrimitive.Empty>
          <ThreadPrimitive.Messages components={{ Message: AssistantMessage, UserMessage, AssistantMessage }} />
        </ThreadPrimitive.Viewport>
        <Composer onNotice={notify} />
      </ThreadPrimitive.Root>
    </AssistantRuntimeProvider>
  )
}

function UserMessage() {
  return (
    <MessagePrimitive.Root className="agent-msg agent-msg-user">
      <div className="agent-bubble">
        <MessagePrimitive.Parts />
      </div>
    </MessagePrimitive.Root>
  )
}

function AssistantMessage() {
  return (
    <MessagePrimitive.Root className="agent-msg agent-msg-assistant">
      <div className="agent-bubble">
        <MessagePrimitive.Parts />
      </div>
    </MessagePrimitive.Root>
  )
}

function Composer({ onNotice }: { onNotice: (message: string) => void }) {
  return (
    <ComposerPrimitive.Root className="agent-composer">
      <ComposerPrimitive.Input
        className="agent-input"
        placeholder="说点什么…（Enter 发送，Shift+Enter 换行）"
        rows={1}
        autoFocus
      />
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
