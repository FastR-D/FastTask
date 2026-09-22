import { describe, expect, it } from 'vitest'
import type { AssistantTransportConnectionMetadata } from '@assistant-ui/react'
import { convertState, normalizeStatus } from './converter'
import type { ServerAgentState } from './state'

const idleMeta = { pendingCommands: [], isSending: false, toolStatuses: {} } as AssistantTransportConnectionMetadata

function state(partial: Partial<ServerAgentState>): ServerAgentState {
  return { messages: [], isRunning: false, fasttask: { pendingProposals: [] }, ...partial }
}

// contentOf reaches into the converted ThreadMessage's first content part.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function firstPart(message: unknown): any {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return (message as any).content[0]
}

describe('convertState (agent-impl.md §2.6/§2.7, frontend.md §4.1)', () => {
  it('maps server messages to assistant-ui messages, preserving role/text/status', () => {
    const out = convertState(state({
      messages: [
        { id: 'm1', role: 'user', parts: [{ type: 'text', text: 'hi' }], status: { type: 'complete', reason: 'stop' } },
        { id: 'm2', role: 'assistant', parts: [{ type: 'text', text: 'hello' }], status: { type: 'running' } },
      ],
    }), idleMeta)
    expect(out.messages).toHaveLength(2)
    expect(out.messages[0].role).toBe('user')
    expect(out.messages[1].role).toBe('assistant')
    expect(firstPart(out.messages[0])).toMatchObject({ type: 'text', text: 'hi' })
    expect(out.messages[1].status).toEqual({ type: 'running' })
  })

  it('passes the fasttask namespace through as state (§4.1)', () => {
    const out = convertState(state({
      fasttask: { activeGoalId: 'g1', pendingProposals: [{ id: 'p1', goalId: 'g1', baseRevision: 3, summary: 'x' }] },
    }), idleMeta)
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const st = out.state as any
    expect(st.activeGoalId).toBe('g1')
    expect(st.pendingProposals).toHaveLength(1)
  })

  it('ORs server isRunning with connectionMetadata.isSending (§4.1)', () => {
    expect(convertState(state({ isRunning: false }), { ...idleMeta, isSending: true }).isRunning).toBe(true)
    expect(convertState(state({ isRunning: true }), { ...idleMeta, isSending: false }).isRunning).toBe(true)
    expect(convertState(state({ isRunning: false }), { ...idleMeta, isSending: false }).isRunning).toBe(false)
  })

  it('drops the FastTask approval field — never maps it to part.approval (ADR-0002 §3.1)', () => {
    const out = convertState(state({
      messages: [{
        id: 'm1',
        role: 'assistant',
        parts: [{ type: 'tool-call', toolCallId: 't1', toolName: 'propose_task_tree_patch', args: { summary: 's' }, approval: { status: 'pending' } }],
        status: { type: 'requires-action', reason: 'tool-calls' },
      }],
    }), idleMeta)
    const part = firstPart(out.messages[0])
    expect(part.type).toBe('tool-call')
    expect(part.toolName).toBe('propose_task_tree_patch')
    expect(part.approval).toBeUndefined()
  })

  it('tolerates partial/empty state without throwing (§4.1: pure, handles incomplete)', () => {
    expect(() => convertState({ messages: [], isRunning: false, fasttask: {} }, idleMeta)).not.toThrow()
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    expect(convertState({} as any, idleMeta).messages).toEqual([])
    // A mid-stream assistant message whose only text part is still empty renders
    // with no content yet: fromThreadMessageLike drops whitespace-only text parts,
    // so the bubble appears empty until the first delta arrives (no throw).
    const empty = convertState(state({ messages: [{ id: 'm', role: 'assistant', parts: [{ type: 'text', text: '' }] }] }), idleMeta)
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    expect((empty.messages[0] as any).content).toEqual([])
    // Once a partial (half-sentence) delta lands, it is preserved verbatim.
    const partial = convertState(state({ messages: [{ id: 'm', role: 'assistant', parts: [{ type: 'text', text: '半句' }], status: { type: 'running' } }] }), idleMeta)
    expect(firstPart(partial.messages[0]).text).toBe('半句')
  })
})

describe('normalizeStatus', () => {
  it('passes through valid assistant-ui statuses', () => {
    expect(normalizeStatus({ type: 'running' })).toEqual({ type: 'running' })
    expect(normalizeStatus({ type: 'complete', reason: 'stop' })).toEqual({ type: 'complete', reason: 'stop' })
    expect(normalizeStatus({ type: 'requires-action', reason: 'tool-calls' })).toEqual({ type: 'requires-action', reason: 'tool-calls' })
    expect(normalizeStatus({ type: 'incomplete', reason: 'cancelled' })).toEqual({ type: 'incomplete', reason: 'cancelled' })
  })
  it('folds server-only reasons (max-turns, run-timeout) into other', () => {
    expect(normalizeStatus({ type: 'incomplete', reason: 'max-turns' })).toEqual({ type: 'incomplete', reason: 'other' })
    expect(normalizeStatus({ type: 'incomplete', reason: 'run-timeout' })).toEqual({ type: 'incomplete', reason: 'other' })
  })
  it('defaults a missing/unknown status to complete (never perpetually running)', () => {
    expect(normalizeStatus(undefined)).toEqual({ type: 'complete', reason: 'unknown' })
    expect(normalizeStatus({ type: 'complete' })).toEqual({ type: 'complete', reason: 'unknown' })
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    expect(normalizeStatus({ type: 'bogus' } as any)).toEqual({ type: 'complete', reason: 'unknown' })
  })
})
