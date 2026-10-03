import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AgentRuntimeOptions } from './runtime'
import type { ServerAgentState } from './state'

const { requestMock, runtimeCalls } = vi.hoisted(() => ({ requestMock: vi.fn(), runtimeCalls: [] as AgentRuntimeOptions[] }))

vi.mock('../api', () => ({
  request: requestMock,
  ensureFreshAccessToken: vi.fn(async () => 'test-token'),
}))

// The real runtime, counted: every call is a render of the component that hosts it.
vi.mock('./runtime', async importOriginal => {
  const actual = await importOriginal<typeof import('./runtime')>()
  return {
    ...actual,
    useAgentRuntime: (state: ServerAgentState, onNotice: (message: string) => void, options: AgentRuntimeOptions = {}) => {
      runtimeCalls.push(options)
      return actual.useAgentRuntime(state, onNotice, options)
    },
  }
})

import { AgentChat } from './index'

beforeEach(() => {
  requestMock.mockReset()
  runtimeCalls.length = 0
  globalThis.ResizeObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  Element.prototype.scrollIntoView ??= function scrollIntoView() {}
  Element.prototype.scrollTo ??= function scrollTo() {}
})
afterEach(cleanup)

const state: ServerAgentState = {
  messages: [{ id: 'msg_user', role: 'user', parts: [{ type: 'text', text: '帮我拆解论文' }], createdAt: '2026-09-22T10:00:00Z', status: { type: 'complete', reason: 'stop' } }],
  isRunning: false,
  fasttask: { threadId: 'thr_1', pendingProposals: [] },
}

// assistant-ui's transport runtime resets the thread to its initial state whenever the component that
// calls it re-renders. A finished run refreshes the thread list and the shell re-renders on every
// snackbar, so neither may re-render the conversation — or the reply the user just read disappears.
describe('AgentChat keeps a live conversation across re-renders around it', () => {
  it('does not re-render the runtime host when a run finishes or the shell re-renders', async () => {
    requestMock.mockImplementation(async (path: string) => path === '/agent/thread-state'
      ? { data: { state }, etag: null }
      : { data: { threads: [], next_cursor: null }, etag: null })
    let bumpShell = () => {}
    function Shell() {
      const [notice, setNotice] = useState('')
      bumpShell = () => setNotice(value => value + '!')
      return <><span>{notice}</span><AgentChat goalId={null} onNotice={setNotice} /></>
    }
    render(<Shell />)
    await screen.findByText('帮我拆解论文')
    const rendersAfterMount = runtimeCalls.length

    // The runtime's onFinish hook, as a finished run calls it.
    act(() => { runtimeCalls[runtimeCalls.length - 1].onThreadChanged?.() })
    // The workspace re-rendering for an unrelated reason (a snackbar notice).
    act(() => bumpShell())
    await waitFor(() => expect(requestMock).toHaveBeenCalledWith(expect.stringContaining('/agent/threads')))

    expect(runtimeCalls.length).toBe(rendersAfterMount)
    expect(screen.getByText('帮我拆解论文')).toBeInTheDocument()
  })
})
