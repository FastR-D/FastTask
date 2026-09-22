import {
  fromThreadMessageLike,
  type AssistantTransportConnectionMetadata,
  type MessageStatus,
  type ThreadMessage,
  type ThreadMessageLike,
} from '@assistant-ui/react'
import type { ReadonlyJSONValue } from 'assistant-stream/utils'
import type { ServerAgentState, ServerMessage, ServerMessagePart } from './state'

// convertState maps the authoritative server state to assistant-ui's transport
// state (doc/agent-impl.md §2.6, frontend.md §4.1). It is PURE and tolerates
// partial state: during streaming it is called repeatedly with half-built parts
// and half-sentences. The return shape is assistant-ui's AssistantTransportState
// ({ messages, state?, isRunning }); that type is not re-exported from the react
// entry, so the return is inferred and checked structurally at the hook call site.
//   - messages come from server state, NOT from stream chunks (§2.6).
//   - `state` is the fasttask business namespace, passed through for other UI (§4.1).
//   - isRunning ORs the server flag with connectionMetadata.isSending, so the UI
//     shows busy while a command is still in flight (§4.1).
export function convertState(
  state: ServerAgentState,
  connectionMetadata: AssistantTransportConnectionMetadata,
) {
  const messages = (state?.messages ?? []).map(toThreadMessage)
  return {
    messages,
    state: (state?.fasttask ?? {}) as ReadonlyJSONValue,
    isRunning: Boolean(state?.isRunning) || Boolean(connectionMetadata?.isSending),
  }
}

function toThreadMessage(message: ServerMessage): ThreadMessage {
  const status = normalizeStatus(message?.status)
  const id = message?.id || 'unknown'
  const isUser = message?.role === 'user'
  const like: ThreadMessageLike = {
    role: isUser ? 'user' : 'assistant',
    id,
    createdAt: message?.createdAt ? safeDate(message.createdAt) : undefined,
    // assistant-ui only permits `status` on assistant messages; a user message
    // carrying one makes fromThreadMessageLike throw. The server sends a status on
    // every message, so it is dropped for user messages here.
    status: isUser ? undefined : status,
    content: (message?.parts ?? []).map(partToContent) as ThreadMessageLike['content'],
  }
  return fromThreadMessageLike(like, id, status)
}

// partToContent maps a server part to a ThreadMessageLike content entry. The
// FastTask `approval` field is DELIBERATELY dropped: mapping it onto
// ToolCallMessagePart.approval makes assistant-ui render its native approval
// controls, which assistant-transport never wires up (onRespondToToolApproval is
// not connected), producing buttons that do nothing (ADR-0002 §3.1, §2.7).
function partToContent(part: ServerMessagePart) {
  if (part?.type === 'text') {
    return { type: 'text' as const, text: part.text ?? '' }
  }
  return {
    type: 'tool-call' as const,
    toolCallId: part.toolCallId,
    toolName: part.toolName,
    args: part.args ?? {},
    result: part.result,
    isError: part.isError ?? false,
  }
}

// normalizeStatus maps the server's MessageStatus onto assistant-ui's, folding
// reasons assistant-ui does not define (max-turns, run-timeout) into `other`. A
// missing or malformed status defaults to a complete message so a partial stream
// never renders as perpetually running.
export function normalizeStatus(status: ServerMessage['status']): MessageStatus {
  if (!status || typeof status.type !== 'string') return { type: 'complete', reason: 'unknown' }
  switch (status.type) {
    case 'running':
      return { type: 'running' }
    case 'requires-action':
      return { type: 'requires-action', reason: status.reason === 'interrupt' ? 'interrupt' : 'tool-calls' }
    case 'incomplete':
      return { type: 'incomplete', reason: incompleteReason(status.reason) }
    case 'complete':
    default:
      return { type: 'complete', reason: status.reason === 'stop' ? 'stop' : 'unknown' }
  }
}

function incompleteReason(
  reason: string | undefined,
): 'cancelled' | 'tool-calls' | 'length' | 'content-filter' | 'other' | 'error' {
  switch (reason) {
    case 'cancelled':
      return 'cancelled'
    case 'tool-calls':
      return 'tool-calls'
    case 'length':
      return 'length'
    case 'content-filter':
      return 'content-filter'
    case 'error':
      return 'error'
    default:
      // max-turns, run-timeout, and anything unknown fold into 'other'.
      return 'other'
  }
}

function safeDate(value: string): Date | undefined {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date
}
