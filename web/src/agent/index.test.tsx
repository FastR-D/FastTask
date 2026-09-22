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

  it('renders markdown in both bubbles and keeps the user line breaks', async () => {
    const state = restored('## 目标\n\n第一行\n第二行\n\n| 项 | 值 |\n| --- | --- |\n| a | 1 |')
    requestMock.mockResolvedValue({ data: { state }, etag: null })
    const { container } = render(<AgentChat goalId={null} />)
    await screen.findByText('已经记下了。')

    const user = container.querySelector('.agent-msg-user .agent-bubble')
    const assistant = container.querySelector('.agent-msg-assistant .agent-bubble')
    // Both sides go through agent/Markdown.tsx, so both carry the .agent-md tree.
    expect(user?.querySelector('.agent-md')).not.toBeNull()
    expect(assistant?.querySelector('.agent-md')).not.toBeNull()
    // The user side additionally runs remark-breaks: Shift+Enter survives.
    expect(user?.querySelector('h2')).toHaveTextContent('目标')
    expect(user?.querySelectorAll('p br')).toHaveLength(1)
    expect(user?.querySelectorAll('.agent-md-table table tbody tr')).toHaveLength(1)
    // Neither side may render raw HTML from the other party's text.
    expect(container.querySelector('.agent-md script')).toBeNull()
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

// Phase G's exit criterion (doc/harness.md §9.3): offline, the conversation entry is greyed out and says
// why, instead of accepting a message that cannot reach a model.
describe('AgentChat offline', () => {
  const setOnline = (value: boolean) => Object.defineProperty(navigator, 'onLine', { value, configurable: true })

  it('disables the composer and says that a new conversation needs a network', async () => {
    setOnline(false)
    try {
      requestMock.mockResolvedValue({ data: { state: restored('离线之前的问题') }, etag: null })
      const { container } = render(<AgentChat goalId={null} />)
      // The cached conversation is still readable — that is the point of the PWA snapshot.
      expect(await screen.findByText('离线之前的问题')).toBeInTheDocument()

      const composer = container.querySelector('.agent-composer')
      expect(composer).toHaveClass('offline')
      const input = container.querySelector('.agent-input') as HTMLTextAreaElement | null
      expect(input).not.toBeNull()
      expect(input?.disabled).toBe(true)
      expect(input?.placeholder).toBe('离线中，联网后才能继续对话')
    } finally {
      setOnline(true)
    }
  })

  it('takes the entry back once the device is online again', async () => {
    setOnline(true)
    requestMock.mockResolvedValue({ data: { state: restored('在线的问题') }, etag: null })
    const { container } = render(<AgentChat goalId={null} />)
    await screen.findByText('在线的问题')
    const input = container.querySelector('.agent-input') as HTMLTextAreaElement | null
    expect(input?.disabled).toBe(false)
    expect(container.querySelector('.agent-composer')).not.toHaveClass('offline')
  })
})
