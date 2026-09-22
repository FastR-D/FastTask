import { readFileSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { resolve } from 'node:path'
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest'
import { createGatewayFetch, gatewayBaseURL, GATEWAY_ORIGIN, isGatewayRequest } from './shim'

// The gateway shim round trip (doc/harness.md §4.2, §14.1, §14.11).
//
// The fixtures are transcriptions of what the Phase 0 spike measured, not invented JSON (§14.1): the
// request libfx's shim path produces, and a qwen stream with reasoning_content, split tool-call
// arguments and the interleaved fragment order §16.3 records.

type Captured = { url: string; method: string; authorization: string; body: Record<string, unknown> }

// Fixtures are read from disk rather than imported: a JSON import would be transformed by the bundler
// under test, and the point is to feed the shim bytes exactly as they were recorded.
const fixturePath = (name: string) => resolve(process.cwd(), 'src/harness/__fixtures__', name)
const callFixture = JSON.parse(readFileSync(fixturePath('gateway-call.json'), 'utf8')) as Record<string, unknown>
const streamFixture = readFileSync(fixturePath('openai-stream.sse'), 'utf8')

let server: Server
let origin = ''
let captured: Captured[] = []
let respondWith = 200

beforeAll(async () => {
  server = createServer((request, response) => {
    const chunks: Buffer[] = []
    request.on('data', chunk => chunks.push(chunk as Buffer))
    request.on('end', () => {
      const raw = Buffer.concat(chunks).toString('utf8')
      captured.push({
        url: request.url ?? '',
        method: request.method ?? '',
        authorization: String(request.headers.authorization ?? ''),
        body: raw ? (JSON.parse(raw) as Record<string, unknown>) : {},
      })
      if (respondWith !== 200) {
        response.writeHead(respondWith, { 'content-type': 'application/json' })
        response.end(JSON.stringify({ error: { message: 'upstream refused' } }))
        return
      }
      response.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' })
      response.end(streamFixture)
    })
  })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  origin = `http://127.0.0.1:${typeof address === 'object' && address ? address.port : 0}`
})

afterAll(async () => {
  await new Promise<void>(resolve => server.close(() => resolve()))
})

/** parseParts decodes the V4 SSE the shim hands back to libfx. */
function parseParts(body: string): Array<Record<string, unknown>> {
  return body
    .split('\n')
    .filter(line => line.startsWith('data: '))
    .map(line => JSON.parse(line.slice('data: '.length)) as Record<string, unknown>)
}

function typesOf(parts: Array<Record<string, unknown>>): string[] {
  return parts.map(part => String(part.type))
}

describe('gateway shim', () => {
  it('translates one gateway call into an OpenAI-compatible request to our own proxy', async () => {
    captured = []
    respondWith = 200
    const shim = createGatewayFetch({ runId: 'run_1', harnessToken: 'fth_capability', serverOrigin: origin })

    const response = await shim(`${GATEWAY_ORIGIN}/v4/ai/language-model`, {
      method: 'POST',
      body: JSON.stringify(callFixture),
      headers: { 'content-type': 'application/json', authorization: 'Bearer ignored-by-the-shim' },
    })
    expect(response.status).toBe(200)
    const parts = parseParts(await response.text())

    // The request went to the run-scoped proxy, with the capability as a placeholder key and the
    // server's model — never to Vercel, and never with a provider credential (§4.3, §4.5).
    expect(captured).toHaveLength(1)
    expect(captured[0].url).toBe('/api/v1/agent/runs/run_1/openai/chat/completions')
    expect(captured[0].method).toBe('POST')
    expect(captured[0].authorization).toBe('Bearer fth_capability')
    expect(captured[0].body.model).toBe('qwen3.8-max')
    expect(captured[0].body.stream).toBe(true)
    const messages = captured[0].body.messages as Array<Record<string, unknown>>
    expect(messages[0].role).toBe('system')
    expect(messages[1].role).toBe('user')
    const tools = captured[0].body.tools as Array<Record<string, unknown>>
    expect((tools[0].function as Record<string, unknown>).name).toBe('list_goals')

    // Reasoning, text and the tool call all survive the translation (§16.2).
    const types = typesOf(parts)
    expect(types).toContain('reasoning-start')
    expect(types).toContain('reasoning-delta')
    expect(types).toContain('reasoning-end')
    expect(types).toContain('text-start')
    expect(types).toContain('text-delta')
    expect(types).toContain('text-end')
    expect(types).toContain('tool-input-start')
    expect(types).toContain('tool-call')
    expect(types).toContain('finish')

    const reasoning = parts
      .filter(part => part.type === 'reasoning-delta')
      .map(part => String(part.delta))
      .join('')
    expect(reasoning).toBe('先看看有没有活跃目标')
    const text = parts
      .filter(part => part.type === 'text-delta')
      .map(part => String(part.delta))
      .join('')
    expect(text).toBe('我来查一下')

    // The tool call carries its id and the arguments reassembled from two fragments — the property
    // §4.4.1's correlation depends on.
    const call = parts.find(part => part.type === 'tool-call') as Record<string, unknown>
    expect(call.toolCallId).toBe('call_abc123')
    expect(call.toolName).toBe('list_goals')
    expect(JSON.parse(String(call.input))).toEqual({ status: 'active' })

    const finish = parts.find(part => part.type === 'finish') as Record<string, unknown>
    expect((finish.finishReason as Record<string, unknown>).unified).toBe('tool-calls')
  })

  it('keeps the interleaved order the spike measured (§16.3)', async () => {
    captured = []
    const shim = createGatewayFetch({ runId: 'run_2', harnessToken: 'fth_capability', serverOrigin: origin })
    const response = await shim(`${GATEWAY_ORIGIN}/v4/ai/language-model`, {
      method: 'POST',
      body: JSON.stringify(callFixture),
    })
    const types = typesOf(parseParts(await response.text()))
    // Fragments are NOT strictly nested: text-end lands after the tool input opened. A translator that
    // assumed nesting would drop the tail of the answer.
    expect(types.indexOf('text-end')).toBeGreaterThan(types.indexOf('tool-input-start'))
    expect(types.indexOf('tool-call')).toBeGreaterThan(types.indexOf('tool-input-end'))
  })

  it('builds an absolute proxy URL, or refuses (§14.11)', () => {
    const base = gatewayBaseURL('run_3', 'http://127.0.0.1:8080/')
    expect(base).toBe('http://127.0.0.1:8080/api/v1/agent/runs/run_3/openai')
    // The SDK runs new URL() on the base; a relative one throws from inside it, where nothing can
    // attribute the failure. This is the spike's first recorded trap.
    expect(() => new URL(base)).not.toThrow()
    expect(() => gatewayBaseURL('run/with spaces', 'http://127.0.0.1:1')).not.toThrow()
  })

  it('only intercepts the gateway, and passes everything else through', async () => {
    expect(isGatewayRequest(`${GATEWAY_ORIGIN}/v4/ai/language-model`)).toBe(true)
    expect(isGatewayRequest(new URL(`${GATEWAY_ORIGIN}/v4/ai/language-model`))).toBe(true)
    expect(isGatewayRequest('https://example.com/other')).toBe(false)

    const realFetch = vi.fn(async () => new Response('passthrough', { status: 200 }))
    vi.stubGlobal('fetch', realFetch)
    try {
      const shim = createGatewayFetch({ runId: 'run_4', harnessToken: 'fth_capability', serverOrigin: origin })
      const response = await shim('https://example.com/other', { method: 'GET' })
      expect(await response.text()).toBe('passthrough')
      expect(realFetch).toHaveBeenCalledTimes(1)
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('answers an upstream failure with an error response so libfx can retry (§3.6)', async () => {
    captured = []
    respondWith = 502
    try {
      const shim = createGatewayFetch({ runId: 'run_5', harnessToken: 'fth_capability', serverOrigin: origin })
      const response = await shim(`${GATEWAY_ORIGIN}/v4/ai/language-model`, {
        method: 'POST',
        body: JSON.stringify(callFixture),
      })
      expect(response.status).toBe(502)
      const body = (await response.json()) as { error: { message: string } }
      expect(body.error.message.length).toBeGreaterThan(0)
    } finally {
      respondWith = 200
    }
  })

  it('refuses a gateway body that is not JSON', async () => {
    const shim = createGatewayFetch({ runId: 'run_6', harnessToken: 'fth_capability', serverOrigin: origin })
    const response = await shim(`${GATEWAY_ORIGIN}/v4/ai/language-model`, { method: 'POST', body: 'not json' })
    expect(response.status).toBe(502)
  })
})
