import { makeAssistantToolUI } from '@assistant-ui/react'
import { useState } from 'react'
import { fieldValue } from '../../mdui-react'
import { TOOL_DAILY_PLAN, TOOL_TASK_TREE_PATCH, type ApprovalDecision } from '../state'

// ProposalCard renders one awaiting-approval proposal and returns the decision via
// addResult — assistant-ui's addToolResult, which assistant-transport delivers as
// an `add-tool-result` command (agent-impl.md §7.1). It must NOT use
// respondToApproval/hitl/humanTool: useAssistantTransportRuntime does not wire
// onRespondToToolApproval, so those controls render but do nothing (ADR-0002 §3.1).
// A proposal is "pending" while it has no result yet; once the decision回传s, the
// server applies it (or returns 412/409) and pushes the result back over the stream.
type CardProps = {
  title: string
  args: Record<string, unknown> | undefined
  result: ApprovalDecision | undefined
  addResult: (result: ApprovalDecision) => void
}

function ProposalCard({ title, args, result, addResult }: CardProps) {
  const [reason, setReason] = useState('')
  const decided = result?.decision

  return (
    <div className={`agent-proposal${decided ? ' decided' : ''}`}>
      <header className="agent-proposal-head">
        <mdui-icon name={decided === 'approve' ? 'check_circle' : decided === 'reject' ? 'cancel' : 'rule'} />
        <b>{title}</b>
        {decided && <span className="agent-proposal-state">{decided === 'approve' ? '已批准' : '已拒绝'}</span>}
      </header>

      <ProposalSummary args={args} />

      {decided === 'reject' && result && 'reason' in result && result.reason && (
        <p className="agent-proposal-reason">理由：{result.reason}</p>
      )}

      {!decided && (
        <div className="agent-proposal-actions">
          <mdui-text-field
            label="拒绝理由（可选，会回灌模型）"
            variant="outlined"
            value={reason}
            onChange={e => setReason(fieldValue(e))}
          />
          <div className="agent-proposal-buttons">
            <mdui-button variant="filled" icon="check" onClick={() => addResult({ decision: 'approve' })}>批准</mdui-button>
            {/* Outlined, not tonal: one card gets one accent. The app's other proposal pair (App.tsx
                ProposalCard) is already outlined-reject + filled-apply, and two filled buttons next to
                a 56px field made the decision read as three equally loud controls. */}
            <mdui-button variant="outlined" icon="close" onClick={() => addResult({ decision: 'reject', reason: reason.trim() || undefined })}>拒绝</mdui-button>
          </div>
        </div>
      )}
    </div>
  )
}

// ProposalSummary renders whatever the proposal args carry, defensively: the exact
// arg shape belongs to the server tool, and the card must not break on partial or
// unexpected args (the converter is called mid-stream).
function ProposalSummary({ args }: { args: Record<string, unknown> | undefined }) {
  if (!args) return null
  const summary = typeof args.summary === 'string' ? args.summary : ''
  const list = Array.isArray(args.candidates)
    ? args.candidates
    : Array.isArray(args.nodes)
      ? args.nodes
      : Array.isArray(args.operations)
        ? args.operations
        : null
  return (
    <div className="agent-proposal-body">
      {summary && <p>{summary}</p>}
      {list && <p className="agent-proposal-count">{list.length} 项变更</p>}
      {!summary && !list && <p className="agent-proposal-count">请审阅后决定是否应用。</p>}
    </div>
  )
}

// The two proposal tools the server registers (phases D/E). Rendering these
// components inside the runtime provider registers the cards by tool name
// (frontend.md §4: "提案审批卡片用 makeAssistantToolUI 按工具名注册").
export const TaskTreeProposalUI = makeAssistantToolUI<Record<string, unknown>, ApprovalDecision>({
  toolName: TOOL_TASK_TREE_PATCH,
  display: 'standalone',
  render: ({ args, result, addResult }) => (
    <ProposalCard title="任务树变更提案" args={args} result={result as ApprovalDecision | undefined} addResult={addResult} />
  ),
})

export const DailyPlanProposalUI = makeAssistantToolUI<Record<string, unknown>, ApprovalDecision>({
  toolName: TOOL_DAILY_PLAN,
  display: 'standalone',
  render: ({ args, result, addResult }) => (
    <ProposalCard title="今日计划提案" args={args} result={result as ApprovalDecision | undefined} addResult={addResult} />
  ),
})
