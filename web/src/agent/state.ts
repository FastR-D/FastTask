// Server-side agent state — the frontend mirror of internal/agent/protocol/state.go
// (doc/agent-impl.md §2.7). This is FastTask's OWN shape, NOT an assistant-ui type;
// converter.ts maps it to ThreadMessage. It is the FE/BE contract: changing any of
// it requires changing the server AND doc/agent-impl.md §2.7 together.

export type ServerRole = 'user' | 'assistant'

// Status types/reasons the server may emit (protocol/state.go). Note `max-turns`
// and `run-timeout` are NOT valid assistant-ui MessageStatus reasons, so the
// converter folds them into `other`.
export type ServerStatusType = 'running' | 'complete' | 'incomplete' | 'requires-action'
export type ServerMessageStatus = { type: ServerStatusType; reason?: string }

export type ServerTextPart = { type: 'text'; text: string }

export type ServerApprovalStatus = 'pending' | 'approved' | 'rejected'
export type ServerApproval = { status: ServerApprovalStatus; options?: unknown[] }

export type ServerToolCallPart = {
  type: 'tool-call'
  toolCallId: string
  toolName: string
  args?: Record<string, unknown>
  result?: unknown
  isError?: boolean
  // FastTask-owned. The converter MUST NOT map this onto
  // ToolCallMessagePart.approval (ADR-0002 §3.1, agent-impl.md §2.7); the
  // approval card keys off toolName + whether a result exists yet.
  approval?: ServerApproval
}

// A reasoning part carries a chain of thought (doc/chat-features.md §3.3). It is display-only: nothing may
// parse it, and the converter must not let it influence a tool call or a status.
export type ServerReasoningPart = { type: 'reasoning'; id?: string; text: string }

// An image part carries a reference to a server-owned attachment, never bytes (§4.4).
export type ServerImagePart = { type: 'image'; image: string }

export type ServerMessagePart = ServerTextPart | ServerToolCallPart | ServerReasoningPart | ServerImagePart

export type ServerMessage = {
  id: string
  role: ServerRole
  parts: ServerMessagePart[]
  createdAt?: string
  status?: ServerMessageStatus
}

export type PendingProposal = {
  id: string
  goalId: string
  baseRevision: number
  summary: string
}

// FasttaskState carries business state alongside the conversation so both share
// one stream instead of a separate poll (agent-impl.md §2.7). threadId is pushed
// on a new thread's first run; activeGoalId is server-owned; runId names the run a
// harness host has to drive (doc/harness.md §10.2).
export type FasttaskState = {
  threadId?: string
  activeGoalId?: string
  runId?: string
  pendingProposals?: PendingProposal[]
}

export type ServerAgentState = {
  messages: ServerMessage[]
  isRunning: boolean
  fasttask: FasttaskState
}

// The decision body回传 via add-tool-result (agent-impl.md §7.1). Approvals go
// through addToolResult only — never respondToApproval/hitl (ADR-0002 §3.1).
export type ApprovalDecision = { decision: 'approve' } | { decision: 'reject'; reason?: string }

// Proposal tool names the server registers (phases D/E).
export const TOOL_TASK_TREE_PATCH = 'propose_task_tree_patch'
export const TOOL_DAILY_PLAN = 'propose_daily_plan'
