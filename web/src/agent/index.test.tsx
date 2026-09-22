import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServerAgentState } from './state'

const { requestMock } = vi.hoisted(() => ({ requestMock: vi.fn() }))

// AgentChat restores through api.ts's request so a 401 still rides the shared
// refresh path; the mock stands in for the network only.
vi.mock('../api', () => ({
  request: requestMock,
  ensureFreshAccessToken: vi.fn(async () => 'test-token'),
}))

import { AgentChat } from './index'

// ResizeObserver and the scroll APIs are absent in jsdom but used by the
// assistant-ui viewport; the chat does not assert on their behaviour.
beforeEach(() => {
  requestMock.mockReset()
  globalThis.ResizeObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  Element.prototype.scrollIntoView ??= function scrollIntoView() {}
  Element.prototype.scrollTo ??= function scrollTo() {}
})

// Vitest runs without `globals`, so testing-library's auto cleanup never
// registers; each render needs an explicit unmount.
afterEach(cleanup)

function restored(text: string): ServerAgentState {
  return {
    messages: [
      { id: 'msg_user', role: 'user', parts: [{ type: 'text', text }], createdAt: '2026-09-22T10:00:00Z', status: { type: 'complete', reason: 'stop' } },
      { id: 'msg_assistant', role: 'assistant', parts: [{ type: 'text', text: '已经记下了。' }], createdAt: '2026-09-22T10:00:01Z', status: { type: 'complete', reason: 'stop' } },
    ],
    isRunning: false,
    fasttask: { threadId: 'conv_restored', pendingProposals: [] },
  }
}

describe('AgentChat restore', () => {
  it('renders the conversation the server still holds after a remount', async () => {
    requestMock.mockResolvedValue({ data: { state: restored('帮我把论文拆成任务') }, etag: null })
    render(<AgentChat goalId={null} />)
    expect(await screen.findByText('帮我把论文拆成任务')).toBeInTheDocument()
    expect(await screen.findByText('已经记下了。')).toBeInTheDocument()
    expect(requestMock).toHaveBeenCalledWith('/agent/thread-state')
  })

  it('stays usable when the restore read fails', async () => {
    requestMock.mockRejectedValue(new Error('需要联网'))
    const onNotice = vi.fn()
    render(<AgentChat goalId={null} onNotice={onNotice} />)
    expect(await screen.findByPlaceholderText(/说点什么/)).toBeInTheDocument()
    expect(onNotice).toHaveBeenCalledWith(expect.stringContaining('需要联网'))
    expect(screen.queryByText('正在载入对话…')).not.toBeInTheDocument()
  })

  it('starts an empty conversation when the user has no agent history', async () => {
    requestMock.mockResolvedValue({ data: undefined, etag: null })
    render(<AgentChat goalId={null} />)
    expect(await screen.findByPlaceholderText(/说点什么/)).toBeInTheDocument()
    expect(screen.queryByText('正在载入对话…')).not.toBeInTheDocument()
  })
})
