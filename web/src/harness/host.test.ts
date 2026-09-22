import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createFxAgent } from 'libfx/wasm'
import { resetModeProbe } from './backend'
import { activeRunId, cancelActiveRun, driveRun, forgetManifest, harnessStatus, subscribeHarnessStatus } from './host'

// The host loop (doc/harness.md §3.6, §3.7, §5.1, §7, §10.4, §14.18).
//
// libfx is replaced by a controllable fake so a test can hold a turn open, make it fail, or answer a
// heartbeat — the states a real run passes through but cannot be scripted from the outside. The HTTP
// surface is a router over fetch, so what is exercised is the real sequence of calls a browser makes.

vi.mock('libfx/wasm', () => ({
  supportsJspi: vi.fn(() => true),
  createFxAgent: vi.fn(),
}))

vi.mock('../api', () => ({
  request: vi.fn(),
  ensureFreshAccessToken: vi.fn(async () => 'user-jwt'),
}))

import { request } from '../api'

type FakeTool = { name: string; description: string; inputSchema: unknown; execute(input: unknown, ctx: { signal: AbortSignal }): Promise<unknown> }

type FakeAgent = {
  options: Record<string, unknown>
  tools: FakeTool[]
  closed: boolean
  cancelCalls: number
  promptInput: unknown
  promptSignal: AbortSignal | undefined
  settle(stopReason: string): void
  fail(message: string): void
  checkpointCalls: number
  // prompt is synchronous in libfx: it returns the turn, whose result carries the promise.
  prompt(input: unknown, options?: { signal?: AbortSignal }): unknown
  checkpoint(): Promise<Uint8Array>
  close(): Promise<void>
}

let agents: FakeAgent[] = []
let openAgents = 0
const mockCreateAgent = vi.mocked(createFxAgent)
const mockRequest = vi.mocked(request)

function installFakeAgent() {
  mockCreateAgent.mockImplementation((async (options: Record<string, unknown>) => {
    let settleResult: (value: { stopReason: string }) => void = () => {}
    let rejectResult: (error: Error) => void = () => {}
    const result = new Promise<{ stopReason: string }>((resolve, reject) => {
      settleResult = resolve
      rejectResult = reject
    })
    // An unhandled rejection would fail the test file if nobody awaits result.
    void result.catch(() => undefined)
    const agent = {
      options,
      tools: (options.tools as FakeTool[]) ?? [],
      closed: false,
      cancelCalls: 0,
      promptInput: undefined,
      promptSignal: undefined,
      checkpointCalls: 0,
      settle: (stopReason: string) => settleResult({ stopReason }),
      fail: (message: string) => rejectResult(new Error(message)),
      // prompt() is synchronous in libfx: it returns the turn, whose result is the promise. Making
      // this async would hand the host a Promise and hide exactly the mistake this test exists to
      // catch.
      prompt(input: unknown, promptOptions?: { signal?: AbortSignal }) {
        agent.promptInput = input
        agent.promptSignal = promptOptions?.signal
        const turn = {
          cancel() {
            agent.cancelCalls += 1
          },
          result,
          async *[Symbol.asyncIterator]() {
            yield { type: 'text_delta', delta: '好' }
            await result
          },
        }
        return turn as never
      },
      async checkpoint() {
        agent.checkpointCalls += 1
        if (agent.closed) throw new Error('fx agent is closed')
        return new Uint8Array([1, 2, 3])
      },
      async close() {
        agent.closed = true
        openAgents -= 1
      },
    } as unknown as FakeAgent
    openAgents += 1
    agents.push(agent)
    return agent as never
  }) as never)
}

/** routes is the server surface a host talks to. Each test installs its own answers. */
type RouteAnswer = { status?: number; body?: unknown; capture?: (init: RequestInit, url: string) => void }
let routes: Record<string, RouteAnswer> = {}
let fetchCalls: Array<{ method: string; url: string; authorization: string; body: unknown }> = []

function routeKey(method: string, url: string) {
  const path = url.replace(/^https?:\/\/[^/]+/, '').replace(/^\/api\/v1/, '')
  return `${method} ${path}`
}

function installFetch() {
  vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
    const method = (init?.method ?? 'GET').toUpperCase()
    const body = typeof init?.body === 'string' && init.body ? JSON.parse(init.body) : undefined
    fetchCalls.push({ method, url, authorization: String(new Headers(init?.headers).get('authorization') ?? ''), body })
    const answer = routes[routeKey(method, url)] ?? routes[`${method} *`]
    answer?.capture?.(init ?? {}, url)
    const status = answer?.status ?? 200
    return new Response(status === 204 ? null : JSON.stringify(answer?.body ?? {}), {
      status,
      headers: { 'content-type': 'application/json' },
    })
  }))
}

async function waitFor(predicate: () => boolean, message = 'condition never became true'): Promise<void> {
  const deadline = Date.now() + 2000
  while (Date.now() < deadline) {
    if (predicate()) return
    await new Promise(resolve => setTimeout(resolve, 5))
  }
  throw new Error(message)
}

function grant(overrides: Record<string, unknown> = {}) {
  return {
    harness_token: 'fth_token-1',
    run_id: 'run_1',
    thread_id: 'thr_1',
    model: 'qwen3.8-max',
    instructions: '服务端系统提示',
    tools_etag: '"etag-1"',
    expires_at: '2026-09-23T00:00:00Z',
    heartbeat_interval_s: 10,
    libfx_version: '0.0.10',
    reasoning: 'provider-default',
    context_degraded: false,
    ...overrides,
  }
}

beforeEach(() => {
  agents = []
  openAgents = 0
  fetchCalls = []
  routes = {}
  resetModeProbe()
  forgetManifest()
  installFakeAgent()
  installFetch()
  mockRequest.mockReset()
  mockRequest.mockImplementation((async (path: string, init?: RequestInit) => {
    const method = (init?.method ?? 'GET').toUpperCase()
    const body = typeof init?.body === 'string' && init.body ? JSON.parse(init.body) : undefined
    fetchCalls.push({ method, url: `/api/v1${path}`, authorization: 'Bearer user-jwt', body })
    if (path === '/agent/tools') return { data: { tools: [{ name: 'list_goals', description: '列出目标', input_schema: { type: 'object', properties: {} } }] }, etag: '"etag-1"' }
    if (path === '/agent/runs') return { data: routes['POST /agent/runs']?.body ?? grant(), etag: null }
    throw new Error(`unexpected user-JWT call ${method} ${path}`)
  }) as never)
  routes['POST /agent/runs/run_1/heartbeat'] = { body: { cancel_requested: false, run_status: 'running', expires_at: '2026-09-23T00:10:00Z' } }
  routes['GET /agent/threads/thr_1/checkpoint'] = { status: 404, body: { detail: 'no checkpoint yet' } }
  routes['PUT /agent/threads/thr_1/checkpoint'] = { body: { run_id: 'run_1', status: 'succeeded' } }
})

describe('driveRun', () => {
  it('drives one turn with the server’s model, instructions and tools, then reports completion (§3.3, §5.1)', async () => {
    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '帮我推进论文' })
    await waitFor(() => agents.length === 1, 'the agent was never created')
    const agent = agents[0]

    // Nothing about the model is a client default (§3.3).
    expect(agent.options.model).toBe('qwen3.8-max')
    expect(agent.options.instructions).toBe('服务端系统提示')
    expect(agent.options.apiKey).toBe('fth_token-1')
    expect(typeof agent.options.fetch).toBe('function')
    expect(agent.tools.map(tool => tool.name)).toEqual(['list_goals'])
    expect(agent.promptInput).toBe('帮我推进论文')

    agent.settle('stop')
    await driving

    // The checkpoint is written once, at the end of the turn, and it is what ends the run (§5.1).
    expect(agent.checkpointCalls).toBe(1)
    const put = fetchCalls.find(call => call.method === 'PUT')
    expect(put?.url).toBe('/api/v1/agent/threads/thr_1/checkpoint')
    expect(put?.authorization).toBe('Bearer fth_token-1')
    expect(put?.body).toMatchObject({ stop_reason: 'stop', libfx_version: '0.0.10' })
    expect(String((put?.body as { checkpoint: string }).checkpoint).length).toBeGreaterThan(0)
    expect(agent.closed).toBe(true)
    expect(harnessStatus().running).toBe(false)
  })

  it('holds at most one agent instance, cancelling the run it replaces (§3.7, §14.18)', async () => {
    const first = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '第一个问题' })
    await waitFor(() => agents.length === 1)
    expect(openAgents).toBe(1)

    // A second run starts while the first turn is still open: the old instance must be closed before
    // the new one exists, or a mobile browser carries two 2 MB instances.
    routes['POST /agent/runs'] = { body: grant({ run_id: 'run_2' }) }
    mockRequest.mockImplementation((async (path: string, init?: RequestInit) => {
      if (path === '/agent/tools') return { data: { tools: [] }, etag: '"etag-1"' }
      if (path === '/agent/runs') return { data: grant({ run_id: 'run_2', thread_id: 'thr_1' }), etag: null }
      void init
      throw new Error(`unexpected ${path}`)
    }) as never)
    const second = driveRun({ runId: 'run_2', forcedMode: 'wasm', text: '第二个问题' })
    await waitFor(() => agents.length === 2, 'the second agent was never created')
    expect(agents[0].closed).toBe(true)
    expect(agents[0].cancelCalls).toBeGreaterThan(0)
    expect(openAgents).toBe(1)

    agents[1].settle('stop')
    // The replaced turn still has to settle: a host that abandoned a promise would leak the instance
    // it was supposed to close.
    agents[0].settle('cancelled')
    await second
    await first.catch(() => undefined)
    expect(openAgents).toBe(0)
    expect(activeRunId()).toBeNull()
  })

  it('reports a failed turn so the run does not wait for the reaper (§5.1)', async () => {
    routes['PUT /agent/threads/thr_1/checkpoint'] = { body: { run_id: 'run_1', status: 'failed' } }
    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '问题' })
    await waitFor(() => agents.length === 1)
    agents[0].fail('the gateway refused the call')
    await driving

    const put = fetchCalls.find(call => call.method === 'PUT')
    expect(put?.body).toMatchObject({ stop_reason: 'error' })
    expect(String((put?.body as { error_message?: string }).error_message)).toContain('refused')
    expect(harnessStatus().error).toContain('refused')
  })

  it('rotates the capability token a heartbeat hands back (§10.4)', async () => {
    routes['POST /agent/runs/run_1/heartbeat'] = { body: { cancel_requested: false, run_status: 'running', harness_token: 'fth_token-2', expires_at: '2026-09-23T01:00:00Z' } }
    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '问题' })
    await waitFor(() => agents.length === 1)
    await waitFor(() => fetchCalls.some(call => call.url.endsWith('/heartbeat')), 'no heartbeat was sent')
    agents[0].settle('stop')
    await driving

    const put = fetchCalls.find(call => call.method === 'PUT')
    expect(put?.authorization).toBe('Bearer fth_token-2')
  })

  it('cancels the turn when a heartbeat reports a cancellation (§5.2 channel 1)', async () => {
    routes['POST /agent/runs/run_1/heartbeat'] = { body: { cancel_requested: true, run_status: 'cancelling' } }
    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '问题' })
    await waitFor(() => agents.length === 1)
    await waitFor(() => fetchCalls.some(call => call.url.endsWith('/heartbeat')))
    await waitFor(() => agents[0].cancelCalls > 0, 'the heartbeat cancellation never reached the turn')
    expect(agents[0].promptSignal?.aborted).toBe(true)
    agents[0].settle('cancelled')
    await driving
  })

  it('does not create an agent for a sidecar run (§1.2)', async () => {
    // A forced sidecar needs no probe: the Worker drives that run, and a browser that started an agent
    // too would run the same turn twice.
    await driveRun({ runId: 'run_1', forcedMode: 'sidecar', text: '问题' })
    expect(agents).toHaveLength(0)
    expect(fetchCalls.filter(call => call.method === 'POST' && call.url.endsWith('/agent/runs'))).toHaveLength(0)
    expect(harnessStatus().mode).toBe('sidecar')
  })

  it('waits out an approval inline so the same turn continues (§7)', async () => {
    routes['POST /agent/runs/run_1/tools/propose_task_tree_patch'] = { body: { status: 'pending', proposal_id: 'prop_1' } }
    let polls = 0
    routes['GET /agent/runs/run_1/approvals/prop_1'] = {
      body: { status: 'pending', proposal_id: 'prop_1' },
      capture: () => {
        polls += 1
        if (polls === 2) routes['GET /agent/runs/run_1/approvals/prop_1'] = {
          body: { status: 'approved', proposal_id: 'prop_1', result: { decision: 'approve', applied: true } },
        }
      },
    }

    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '拆解论文' })
    await waitFor(() => agents.length === 1)
    const tool = agents[0].tools.find(entry => entry.name === 'list_goals')
    // The manifest this test installed has one tool; the server would have advertised the proposal
    // tool too. What matters is that a pending outcome turns into a long poll, not a finished call.
    routes['POST /agent/runs/run_1/tools/list_goals'] = { body: { status: 'pending', proposal_id: 'prop_1' } }
    const outcome = await tool!.execute({ status: 'active' }, { signal: new AbortController().signal })
    expect(outcome).toEqual({ decision: 'approve', applied: true })
    expect(polls).toBeGreaterThanOrEqual(2)
    agents[0].settle('stop')
    await driving
  })

  it('hands a tool error back to the model instead of failing the turn (§5 item 6)', async () => {
    routes['POST /agent/runs/run_1/tools/list_goals'] = { body: { status: 'error', is_error: true, error: 'invalid arguments: missing required argument "status"' } }
    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '问题' })
    await waitFor(() => agents.length === 1)
    const tool = agents[0].tools[0]
    const outcome = (await tool.execute({}, { signal: new AbortController().signal })) as { type: string; isError: boolean; text: string }
    expect(outcome.type).toBe('libfx.tool-result')
    expect(outcome.isError).toBe(true)
    expect(outcome.text).toContain('status')
    agents[0].settle('stop')
    await driving
  })
})

describe('cancelActiveRun', () => {
  it('cancels through the user’s JWT, not the capability (§5.2, §10.2)', async () => {
    mockRequest.mockImplementation((async (path: string, init?: RequestInit) => {
      if (path === '/agent/tools') return { data: { tools: [] }, etag: '"etag-1"' }
      if (path === '/agent/runs') return { data: grant(), etag: null }
      if (path.endsWith('/cancellation')) {
        expect(init?.method).toBe('POST')
        // Recorded the way the real api.request sends it: with the user's own token, because a host
        // capability must not be able to cancel its own run (§10.2).
        fetchCalls.push({ method: 'POST', url: `/api/v1${path}`, authorization: 'Bearer user-jwt', body: undefined })
        return { data: { run_id: 'run_1', status: 'cancelling', cancel_requested: true }, etag: null }
      }
      throw new Error(`unexpected ${path}`)
    }) as never)

    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '问题' })
    await waitFor(() => agents.length === 1 && agents[0].promptInput !== undefined, 'the turn never started')
    await cancelActiveRun()
    expect(agents[0].cancelCalls).toBeGreaterThan(0)
    const cancellation = fetchCalls.find(call => call.url.endsWith('/cancellation'))
    expect(cancellation?.authorization).toBe('Bearer user-jwt')
    agents[0].settle('cancelled')
    await driving
  })
})

describe('harness status', () => {
  it('publishes a status a view can render, including a degraded context (§3.2, §6.3)', async () => {
    routes['POST /agent/runs'] = { body: grant({ context_degraded: true }) }
    mockRequest.mockImplementation((async (path: string) => {
      if (path === '/agent/tools') return { data: { tools: [] }, etag: '"etag-1"' }
      if (path === '/agent/runs') return { data: grant({ context_degraded: true }), etag: null }
      throw new Error(`unexpected ${path}`)
    }) as never)

    const seen: Array<Record<string, unknown>> = []
    const unsubscribe = subscribeHarnessStatus(status => seen.push({ ...status }))
    const driving = driveRun({ runId: 'run_1', forcedMode: 'wasm', text: '问题' })
    await waitFor(() => agents.length === 1)
    agents[0].settle('stop')
    await driving
    unsubscribe()

    expect(seen.some(entry => entry.running === true)).toBe(true)
    expect(seen.some(entry => entry.contextDegraded === true)).toBe(true)
    expect(seen[seen.length - 1].running).toBe(false)
  })
})
