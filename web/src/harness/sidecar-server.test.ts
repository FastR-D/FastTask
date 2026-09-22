// @vitest-environment node
import type { AddressInfo } from 'node:net'
import type { Server } from 'node:http'
import { afterEach, describe, expect, it } from 'vitest'
import { createSidecarServer, parseEndpoint } from '../../../sidecar/src/main'
import type { FxTurnEvent, FxTurnResult } from 'libfx'
import type { SidecarAgent } from './sidecar-driver'

// The sidecar's HTTP surface (doc/harness.md §8.2, §8.3).
//
// What matters here is the boundary: every route is authenticated with the startup secret, the run body is
// validated before anything is created, and a cancellation actually reaches the turn. The driver itself is
// exercised against the real runtime in integration.test.ts; these tests use a fake agent so a routing or
// auth regression cannot hide behind a model that happens to work.

const config = {
  endpoint: 'unix:///tmp/unused.sock',
  secret: 'supervisor-secret',
  serverOrigin: 'http://127.0.0.1:10099',
  webRoot: '/unused',
  libfxVersion: '0.0.10',
}

let started: Server | null = null

afterEach(async () => {
  const server = started
  started = null
  if (server) await new Promise<void>(resolve => server.close(() => resolve()))
})

async function serve(options: Parameters<typeof createSidecarServer>[0]): Promise<string> {
  const server = createSidecarServer(options)
  started = server
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const address = server.address() as AddressInfo
  return `http://127.0.0.1:${address.port}`
}

async function post(origin: string, path: string, body: unknown, secret?: string): Promise<{ status: number; body: Record<string, unknown> }> {
  const response = await fetch(`${origin}${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...(secret === undefined ? {} : { authorization: `Bearer ${secret}` }) },
    body: JSON.stringify(body),
  })
  return { status: response.status, body: (await response.json()) as Record<string, unknown> }
}

/** fakeAgent is a libfx stand-in: it yields one text event and reports a finished turn. */
function fakeAgent(behaviour: { stopReason?: string; hang?: boolean; onPrompt?: () => void } = {}) {
  const agent: SidecarAgent = {
    prompt() {
      behaviour.onPrompt?.()
      const events = (async function* (): AsyncGenerator<FxTurnEvent> {
        if (behaviour.hang) {
          await new Promise<void>(() => undefined)
          return
        }
        yield { type: 'text_delta', delta: 'hi' }
      })()
      return Object.assign(events, {
        cancel() {
          /* a fake turn has nothing to cancel */
        },
        result: Promise.resolve({ stopReason: behaviour.stopReason ?? 'end_turn', usage: { inputTokens: 3 } }),
      })
    },
    checkpoint: async () => new Uint8Array([1, 2, 3]),
    close: async () => undefined,
  }
  return agent
}

describe('the sidecar HTTP surface', () => {
  it('answers nothing at all without the startup secret', async () => {
    const origin = await serve({ config, createAgent: async () => fakeAgent(), nativeAddon: true })
    const anonymous = await fetch(`${origin}/healthz`)
    expect(anonymous.status).toBe(401)
    const wrong = await fetch(`${origin}/healthz`, { headers: { authorization: 'Bearer not-the-secret' } })
    expect(wrong.status).toBe(401)
    const run = await post(origin, '/run', { run_id: 'run_1' })
    expect(run.status).toBe(401)
  })

  it('reports what it is running on /healthz', async () => {
    const origin = await serve({ config, createAgent: async () => fakeAgent(), nativeAddon: false })
    const response = await fetch(`${origin}/healthz`, { headers: { authorization: `Bearer ${config.secret}` } })
    expect(response.status).toBe(200)
    const body = (await response.json()) as Record<string, unknown>
    expect(body.ok).toBe(true)
    expect(body.node_version).toBe(process.version)
    // A supervisor logs this: without the addon libfx falls back to Node's WASM backend (§8.2).
    expect(body.native_addon).toBe(false)
    expect(body.libfx_version).toBe('0.0.10')
    expect(String(body.detail)).toContain('WASM')
  })

  it('refuses a run body it cannot drive', async () => {
    const origin = await serve({ config, createAgent: async () => fakeAgent(), nativeAddon: true })
    const missing = await post(origin, '/run', { run_id: 'run_1', harness_token: 'fth_x' }, config.secret)
    expect(missing.status).toBe(400)
    expect(String(missing.body.error)).toContain('thread_id')
    const unknown = await post(origin, '/nope', {}, config.secret)
    expect(unknown.status).toBe(404)
  })

  it('drives a run and reports how it ended, without completing it', async () => {
    const calls: Array<{ method: string; path: string; token: string; body: unknown }> = []
    const origin = await serve({
      config,
      nativeAddon: true,
      createAgent: async () => fakeAgent(),
      call: async (method, path, token, body) => {
        calls.push({ method, path, token, body })
        if (path === '/agent/tools') return { tools: [{ name: 'list_goals', description: 'list', input_schema: { type: 'object', properties: {} } }] }
        if (path.startsWith('/agent/runs/') && path.endsWith('/heartbeat')) return { cancel_requested: false }
        return {}
      },
    })

    const response = await post(
      origin,
      '/run',
      { run_id: 'run_9', harness_token: 'fth_secret', thread_id: 'thr_9', prompt: 'say hi', model: 'qwen3.8-max', instructions: 'be terse', libfx_version: '0.0.10' },
      config.secret,
    )
    expect(response.status).toBe(200)
    expect(response.body.stop_reason).toBe('end_turn')
    expect(response.body.usage).toEqual({ inputTokens: 3 })

    const paths = calls.map(call => `${call.method} ${call.path}`)
    expect(paths).toContain('GET /agent/tools')
    expect(paths.some(path => path.endsWith('/heartbeat'))).toBe(true)
    // Every call carries the run capability and nothing else: the sidecar holds no user credential (§8.1).
    expect(calls.every(call => call.token === 'fth_secret')).toBe(true)

    // The checkpoint write must NOT carry a stop_reason. In sidecar mode the server records the outcome
    // from this very response; a host that completed the run too would finish it twice (§8.3).
    const checkpoint = calls.find(call => call.path.includes('/checkpoint'))
    expect(checkpoint?.method).toBe('PUT')
    expect(checkpoint?.body).toMatchObject({ libfx_version: '0.0.10' })
    expect(checkpoint?.body as Record<string, unknown>).not.toHaveProperty('stop_reason')
    expect(String((checkpoint?.body as { checkpoint?: string }).checkpoint ?? '').length).toBeGreaterThan(0)
  })

  it('cancels a run that is still going', async () => {
    const controller = new AbortController()
    let prompted = false
    const origin = await serve({
      config,
      nativeAddon: true,
      createAgent: async () => ({
        prompt(_input: unknown, options?: { signal?: AbortSignal }) {
          prompted = true
          const events = (async function* (): AsyncGenerator<FxTurnEvent> {
            // A turn that only ends when it is aborted, which is what a long model call looks like.
            await new Promise<void>(resolve => {
              options?.signal?.addEventListener('abort', () => {
                controller.abort()
                resolve()
              }, { once: true })
            })
            yield { type: 'text_delta', delta: '' }
          })()
          return Object.assign(events, {
            cancel() {
              /* the abort above is the cancellation */
            },
            result: new Promise<FxTurnResult>(resolve => {
              controller.signal.addEventListener('abort', () => resolve({ stopReason: 'cancelled' }), { once: true })
            }),
          })
        },
        // Empty bytes, not undefined: the real runtime always answers with a buffer, and the driver
        // encodes an empty one as "no checkpoint" rather than storing nothing at all.
        checkpoint: async () => new Uint8Array(),
        close: async () => undefined,
      }),
      call: async (_method, path) => (path === '/agent/tools' ? { tools: [] } : {}),
    })

    const running = post(
      origin,
      '/run',
      { run_id: 'run_cancel', harness_token: 'fth_secret', thread_id: 'thr_1', prompt: 'think', model: 'm', instructions: 'i' },
      config.secret,
    )
    await new Promise(resolve => setTimeout(resolve, 50))
    expect(prompted).toBe(true)

    const cancel = await post(origin, '/run/run_cancel/cancel', {}, config.secret)
    expect(cancel.status).toBe(200)
    expect(cancel.body.cancelled).toBe(true)

    const report = await running
    expect(report.status).toBe(200)
    expect(report.body.stop_reason).toBe('cancelled')

    // Cancelling a run this process is not driving is not an error; it just has nothing to stop.
    const unknown = await post(origin, '/run/run_nobody/cancel', {}, config.secret)
    expect(unknown.body.cancelled).toBe(false)
  })

  it('only listens where the supervisor can reach it', () => {
    expect(parseEndpoint('unix:///tmp/s.sock')).toEqual({ socket: '/tmp/s.sock' })
    expect(parseEndpoint('http://127.0.0.1:9000')).toEqual({ host: '127.0.0.1', port: 9000 })
    expect(() => parseEndpoint('http://10.0.0.5:9000')).toThrow(/loopback/)
    expect(() => parseEndpoint('https://sidecar.example.com')).toThrow(/loopback/)
  })
})
