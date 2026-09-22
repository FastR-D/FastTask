import { beforeEach, describe, expect, it, vi } from 'vitest'
import { resetModeProbe } from '../harness'
import { extractUserText, prepareAgentCommand } from './runtime'

// The probe is mocked: it compiles 2 MB of wasm in a browser, which is not what this test is about
// (doc/harness.md §3.2). What is under test here is that the request the server receives names a
// thread it can trust and a mode the probe actually produced.
vi.mock('../harness', async () => {
  const actual = await vi.importActual<typeof import('../harness')>('../harness')
  return { ...actual, selectMode: vi.fn(async () => ({ mode: 'wasm', reason: 'LIBFX_JSPI_AVAILABLE' })) }
})

describe('agent command identity', () => {
  beforeEach(() => resetModeProbe())

  it('uses a server thread ID only after the server returns one', async () => {
    const local = '__LOCALID_browser-generated'
    const first = await prepareAgentCommand({ threadId: local, state: { messages: [], isRunning: false, fasttask: {} } })
    expect(first.threadId).toBeNull()

    const next = await prepareAgentCommand({
      threadId: local,
      state: { messages: [], isRunning: false, fasttask: { threadId: 'thr_server' } },
    })
    expect(next.threadId).toBe('thr_server')
  })

  it('names the harness mode the probe selected (§1.2)', async () => {
    const body = await prepareAgentCommand({ threadId: null, state: { messages: [], isRunning: false, fasttask: {} } })
    expect(body.harness_mode).toBe('wasm')
  })

  it('sends no mode when no host is available, so the server keeps the run (§3.2)', async () => {
    const harness = await import('../harness')
    vi.mocked(harness.selectMode).mockResolvedValueOnce({ mode: 'unavailable', reason: 'LIBFX_WASM_LOAD_FAILED' })
    const body = await prepareAgentCommand({ threadId: null, state: { messages: [], isRunning: false, fasttask: {} } })
    expect(body.harness_mode).toBeUndefined()
  })

  it('reads the user text out of the commands the runtime assembled', () => {
    expect(extractUserText([
      { type: 'add-message', message: { role: 'user', parts: [{ type: 'text', text: '帮我' }, { type: 'text', text: '推进论文' }] } },
    ])).toBe('帮我推进论文')
    expect(extractUserText([{ type: 'add-tool-result', toolCallId: 'c1' }])).toBe('')
    expect(extractUserText(undefined)).toBe('')
  })
})
