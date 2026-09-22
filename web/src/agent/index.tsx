import {
  AssistantRuntimeProvider,
  ComposerPrimitive,
  MessagePrimitive,
  ThreadPrimitive,
} from '@assistant-ui/react'
import { useAgentRuntime } from './runtime'
import { DailyPlanProposalUI, TaskTreeProposalUI } from './tools/ProposalApproval'
import { VoiceButton } from './VoiceButton'
import '../agent.css'

// AgentChat is the workflow-B mount point (frontend.md §2). It自带
// AssistantRuntimeProvider, so workflow A only places <AgentChat goalId={...} /> in
// the layout and never wraps it. Everything inside lives in web/src/agent/ and is
// styled with mdui tokens only (ADR-0004).
export function AgentChat({ goalId, onNotice }: { goalId: string | null; onNotice?: (message: string) => void }) {
  const notify = onNotice ?? (() => {})
  const runtime = useAgentRuntime(goalId, notify)
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
