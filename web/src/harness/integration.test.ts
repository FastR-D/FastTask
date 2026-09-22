// @vitest-environment node
import { createServer, type Server } from 'node:http'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
// The REAL libfx, with the native addon (doc/harness.md §8.2). Every other harness test mocks
// createFxAgent, which is right for unit behaviour and useless for the one question that matters most:
// does the shim actually intercept what libfx really sends? This file is that check, and it is the reason
// a wrong option name cannot ship silently again.
import { createFxAgent } from 'libfx/node'
import { createGatewayFetch } from './shim'
import { buildHostTools } from './tools'

type Recorded = {
  url: string
  authorization: string
  body: Record<string, unknown>
}

/** stubModel is a minimal OpenAI-compatible /chat/completions endpoint: it records what it was asked and
 *  answers with a streamed completion, optionally with one tool call first. */
function stubModel(answer: { toolCall?: { name: string; args: Record<string, unknown> }; text?: string }) {
  const recorded: Recorded[] = []
  const server: Server = createServer((request, response) => {
    const chunks: Buffer[] = []
    request.on('data', chunk => chunks.push(chunk as Buffer))
    request.on('end', () => {
      const body = JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>
      recorded.push({
        url: request.url ?? '',
        authorization: String(request.headers.authorization ?? ''),
        body,
      })
      response.writeHead(200, { 'content-type': 'text/event-stream' })
      const send = (payload: Record<string, unknown>) => response.write(`data: ${JSON.stringify(payload)}\n\n`)
      const id = 'chatcmpl-test'
      const base = { id, object: 'chat.completion.chunk', created: 1, model: String(body.model ?? 'test') }
      const wantsTool = answer.toolCall !== undefined && recorded.length === 1
      if (wantsTool) {
        send({
          ...base,
          choices: [{
            index: 0,
            delta: {
              role: 'assistant',
              tool_calls: [{
                index: 0,
                id: 'call_test_1',
                type: 'function',
                function: { name: answer.toolCall!.name, arguments: JSON.stringify(answer.toolCall!.args) },
              }],
            },
            finish_reason: null,
          }],
        })
        send({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }] })
      } else {
        send({ ...base, choices: [{ index: 0, delta: { role: 'assistant', content: answer.text ?? 'hello' }, finish_reason: null }] })
        send({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] })
      }
      response.write('data: [DONE]\n\n')
      response.end()
    })
  })
  return { server, recorded }
}

async function listen(server: Server): Promise<number> {
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('the stub model did not bind')
  return address.port
}

describe('the harness against the real libfx runtime', () => {
  let server: Server
  let recorded: Recorded[]
  let origin: string

  beforeAll(async () => {
    const stub = stubModel({ text: 'the answer is 42' })
    server = stub.server
    recorded = stub.recorded
    origin = `http://127.0.0.1:${await listen(server)}`
  })

  afterAll(async () => {
    await new Promise<void>(resolve => server.close(() => resolve()))
  })

  it('routes the model call through the shim to the proxy, and reports the text', async () => {
    const events: Array<Record<string, unknown>> = []
    const agent = await createFxAgent({
      apiKey: 'hts_placeholder_token',
      model: 'qwen3.8-max',
      instructions: 'You are terse.',
      fetch: createGatewayFetch({
        runId: 'run_1',
        harnessToken: 'hts_placeholder_token',
        // The stub stands in for the Go proxy: same path shape, same OpenAI-compatible contract.
        serverOrigin: origin.replace(/^http/, 'http'),
        model: 'qwen3.8-max',
        onEvent: event => events.push(event as Record<string, unknown>),
      }),
      onEvent: (event: Record<string, unknown>) => events.push(event),
    } as never)

    const turn = agent.prompt('what is 6 times 7?')
    const seen: string[] = []
    for await (const event of turn as AsyncIterable<{ type: string; delta?: string }>) {
      if (event.type === 'text_delta') seen.push(String(event.delta ?? ''))
    }
    const result = await turn.result
    await agent.close()

    // The shim must have been what carried the call: if libfx reached its own gateway instead, nothing
    // would have been recorded and the turn would have no text.
    expect(recorded.length).toBeGreaterThan(0)
    expect(recorded[0].url).toContain('/api/v1/agent/runs/run_1/openai/chat/completions')
    // The capability token travels as the bearer placeholder the proxy replaces (§4.3); a provider key
    // must never be what the host sends (§10.2).
    expect(recorded[0].authorization).toBe('Bearer hts_placeholder_token')
    expect(recorded[0].body.model).toBe('qwen3.8-max')
    // The real runtime reports `end_turn`, not the AI SDK's `stop`; the server maps every reason that is
    // not cancelled or error to its own `stop` (internal/application/harnesslifecycle.go), so the wire
    // vocabulary stays the documented one whatever libfx calls it.
    expect(result.stopReason).toBe('end_turn')
    expect(seen.join('')).toContain('42')
    // The catalog request must have been answered inside the host: a real gateway call would break
    // ADR-0005 §3.3, and libfx treats a failed catalog as fatal, hanging the turn forever.
    expect(events.filter(event => (event as { type?: string }).type === 'shim.catalog').length).toBeGreaterThan(0)
    expect(events.some(event => (event as { type?: string }).type === 'shim.unhandled')).toBe(false)
  })

  it('executes a projected host tool and finishes the second turn', async () => {
    const stub = stubModel({ toolCall: { name: 'get_task_coords', args: { task_id: 'task_1' } }, text: 'coords read' })
    const toolServer = stub.server
    const toolOrigin = `http://127.0.0.1:${await listen(toolServer)}`
    const executed: string[] = []

    const tools = buildHostTools(
      [{
        name: 'get_task_coords',
        description: 'Read a task coordinate',
        input_schema: { type: 'object', properties: { task_id: { type: 'string' } }, required: ['task_id'] },
      }],
      async name => {
        executed.push(name)
        return { ok: true, value: { task_id: 'task_1', order: 3 } }
      },
    )

    const agent = await createFxAgent({
      apiKey: 'hts_placeholder_token',
      model: 'qwen3.8-max',
      tools,
      fetch: createGatewayFetch({ runId: 'run_2', harnessToken: 'hts_placeholder_token', serverOrigin: toolOrigin, model: 'qwen3.8-max' }),
    } as never)

    const turn = agent.prompt('read the coords')
    for await (const _event of turn as AsyncIterable<unknown>) {
      // drained
    }
    const result = await turn.result
    await agent.close()
    await new Promise<void>(resolve => toolServer.close(() => resolve()))

    expect(executed).toEqual(['get_task_coords'])
    expect(stub.recorded.length).toBe(2)
    expect(result.stopReason).toBe('end_turn')
  })
})
