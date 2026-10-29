import { createFxAgent, supportsJspi } from '../src/harness/runtime'
import { createGatewayFetch } from '../src/harness/shim'
import { buildHostTools } from '../src/harness/tools'
import { ToolCallBindings } from '../src/harness/tool-identity'

const run = async () => {
  if (!supportsJspi()) throw new Error('JSPI unavailable in test browser')
  let count = 0
  const bindings = new ToolCallBindings()
  const agent = await createFxAgent({
    apiKey: 'fixture-cap', model: 'fixture-model', wasm: `${location.origin}/fx-core.wasm`, instructions: 'Look up once, then answer.',
    fetch: createGatewayFetch({ serverOrigin: 'http://fixture.test', runId: 'fixture', harnessToken: 'fixture-cap', model: 'fixture-model', bindings }),
    tools: buildHostTools([{ name: 'lookup', description: 'Read fixture value.', input_schema: { type: 'object', properties: { query: { type: 'string' } }, required: ['query'], additionalProperties: false } }], async (_name, input, _signal, id) => {
      if (id !== 'real-call' || (input as { query?: string }).query !== 'fixture') throw new Error('identity mismatch')
      count++
      return { ok: true, value: { value: 'fixture-value' } }
    }, { bindings }),
  })
  try {
    const turn = agent.prompt('Read the fixture.', { signal: AbortSignal.timeout(15_000) })
    for await (const _event of turn) { /* Consume exactly once. */ }
    const result = await turn.result
    const checkpoint = await agent.checkpoint()
    if (count !== 1 || !checkpoint.length) throw new Error(`tool or checkpoint contract failed: count=${count} bytes=${checkpoint.length} stop=${result.stopReason} result=${JSON.stringify(result)}`)
    return { pass: true, host: 'wasm', toolExecutions: count, checkpointBytes: checkpoint.length, stopReason: result.stopReason }
  } finally { await agent.close() }
}
;(globalThis as typeof globalThis & { contractResult: Promise<unknown> }).contractResult = run().catch(error => ({ pass: false, error: String(error) }))
