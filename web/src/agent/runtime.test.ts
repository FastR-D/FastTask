import { describe, expect, it } from 'vitest'
import { prepareAgentCommand } from './runtime'

describe('agent command identity', () => {
  it('uses a server thread ID only after the server returns one', () => {
    const local = '__LOCALID_browser-generated'
    const first = prepareAgentCommand({ threadId: local, state: { messages: [], isRunning: false, fasttask: {} } })
    expect(first.threadId).toBeNull()

    const next = prepareAgentCommand({
      threadId: local,
      state: { messages: [], isRunning: false, fasttask: { threadId: 'conv_server' } },
    })
    expect(next.threadId).toBe('conv_server')
  })
})
