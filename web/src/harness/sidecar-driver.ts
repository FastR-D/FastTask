import type { FxAgent, FxTurn, FxTurnEvent, HostTool } from './runtime'
import { createGatewayFetch } from './shim'
import { buildHostTools, type ToolCallResult, type ToolDef } from './tools'

// The sidecar's run driver (doc/harness.md §8).
//
// This module is isomorphic on purpose: it holds the loop a Node host runs, and nothing in it touches Node
// APIs, so the browser host and the sidecar host can be tested against the same code paths and the claim
// that the two hosts are interchangeable (§14.3) is a property of the server rather than of two
// implementations that happen to agree.
//
// What the sidecar does NOT do is written here as much as what it does: it holds no credentials (§8.1 — the
// token it drives with is a run capability, and the model key is injected by the Go proxy), it stores
// nothing, and it reports no transcript, because the proxy already wrote one (§8.3).

/** The pieces of a libfx agent this driver needs. Injected so the Node entry can hand it libfx/node and a
 *  test can hand it a fake. */
export type SidecarAgent = Pick<FxAgent, 'prompt' | 'checkpoint' | 'close'>

export type SidecarAgentFactory = (options: {
  apiKey: string
  model: string
  instructions: string
  tools: HostTool[]
  fetch: typeof fetch
  checkpoint?: Uint8Array
  onEvent?: (event: { type: string; [key: string]: unknown }) => void
}) => Promise<SidecarAgent>

/** One POST /run body (§8.3, extended with the grant fields a sidecar cannot fetch for itself). */
export type SidecarRunRequest = {
  run_id: string
  harness_token: string
  prompt: string
  checkpoint?: string | null
  libfx_version?: string
  model: string
  instructions: string
  thread_id: string
}

/** What POST /run answers with. No transcript: the model proxy wrote it (§8.3). */
export type SidecarRunReport = {
  stop_reason: string
  usage?: Record<string, unknown>
  checkpoint?: string
  libfx_version?: string
  error_message?: string
}

export type SidecarDeps = {
  /** Absolute origin of the FastTask server, so the shim can build an absolute proxy URL (§16.3). */
  serverOrigin: string
  createAgent: SidecarAgentFactory
  /** HTTP with the run capability token. Injected so a test can watch the calls. */
  call: (method: string, path: string, token: string, body?: unknown) => Promise<Record<string, unknown>>
  /** Aborted by the supervisor when Go asks for a cancellation (§5.2: the sidecar's cancel channel). */
  signal?: AbortSignal
  onEvent?: (event: { type: string; [key: string]: unknown }) => void
}

/** base64 decoding that works in Node and in a browser test environment. */
function decodeCheckpoint(value: string | null | undefined): Uint8Array | undefined {
  if (!value) return undefined
  const binary = atob(value)
  const bytes = new Uint8Array(binary.length)
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index)
  return bytes
}

function encodeCheckpoint(bytes: Uint8Array | undefined): string | undefined {
  if (!bytes || bytes.length === 0) return undefined
  let binary = ''
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000))
  }
  return btoa(binary)
}

/**
 * driveSidecarRun executes one run and reports how it ended.
 *
 * The shape is the browser host's shape: read the tool manifest, build the agent with the server's model and
 * instructions, prompt once, consume the turn's events (a turn has exactly one consumer and it must
 * consume, §3.6), keep heartbeating — including while an approval waits, or a 15 minute decision would be
 * cut short by the 45 second loss detector (§10.4) — then store the checkpoint, which is also what ends the
 * run (§5.1), and close the instance. No pooling: one agent per run, closed as soon as it is done (§3.7).
 */
export async function driveSidecarRun(deps: SidecarDeps, request: SidecarRunRequest): Promise<SidecarRunReport> {
  const token = { current: request.harness_token }
  let agent: SidecarAgent | null = null
  const controller = new AbortController()
  const diagnostics = (event: { type: string; [key: string]: unknown }) => deps.onEvent?.(event)

  const heartbeat = setInterval(() => {
    void beat().catch(() => undefined)
  }, 10_000)
  if (deps.signal) {
    if (deps.signal.aborted) controller.abort()
    else deps.signal.addEventListener('abort', () => controller.abort(), { once: true })
  }

  async function beat(): Promise<void> {
    const reply = await deps.call('POST', `/agent/runs/${encodeURIComponent(request.run_id)}/heartbeat`, token.current, {})
    const renewed = reply.harness_token
    if (typeof renewed === 'string' && renewed) token.current = renewed
    if (reply.cancel_requested === true) {
      controller.abort()
    }
  }

  try {
    await beat()
    if (controller.signal.aborted) throw new Error('cancelled before the run started')

    const manifest = (await deps.call('GET', '/agent/tools', token.current)) as { tools?: ToolDef[] }
    diagnostics({ type: 'sidecar.manifest', tools: manifest.tools?.length ?? 0 })
    const tools = buildHostTools(manifest.tools ?? [], (name, input, signal) => callTool(deps, token, request.run_id, name, input, signal, diagnostics), {
      onWarn: message => diagnostics({ type: 'sidecar.warning', message }),
    })

    agent = await deps.createAgent({
      apiKey: token.current,
      model: request.model,
      instructions: request.instructions,
      // libfx's own events (its transport, its retry decisions, its runtime) only arrive through this hook.
      // Without it a turn that the runtime keeps retrying is indistinguishable from one that is thinking.
      onEvent: diagnostics,
      tools,
      fetch: createGatewayFetch({
        runId: request.run_id,
        harnessToken: () => token.current,
        serverOrigin: deps.serverOrigin,
        model: request.model,
        onEvent: diagnostics,
      }),
      ...(decodeCheckpoint(request.checkpoint) ? { checkpoint: decodeCheckpoint(request.checkpoint) } : {}),
    })

    const turn: FxTurn = agent.prompt(request.prompt, { signal: controller.signal })
    for await (const event of turn as AsyncIterable<FxTurnEvent>) {
      if (event.type === 'tool_start') diagnostics({ type: 'sidecar.tool_start', name: event.name })
      reportRecovery(event as unknown as Record<string, unknown>, diagnostics)
      if (controller.signal.aborted) break
    }
    const result = await turn.result
    const checkpoint = encodeCheckpoint(await agent.checkpoint().catch(() => undefined))
    await saveCheckpoint(deps, request, token.current, checkpoint)
    return {
      stop_reason: result.stopReason ?? 'stop',
      usage: result.usage as Record<string, unknown> | undefined,
      checkpoint,
      libfx_version: request.libfx_version,
    }
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    diagnostics({ type: 'sidecar.error', error: message })
    // The failure travels back in the report below, not in a checkpoint write: in sidecar mode the server
    // records the outcome (§8.3), and if this process dies first the reaper collects the run (§11).
    return {
      stop_reason: controller.signal.aborted ? 'cancelled' : 'error',
      error_message: message,
      libfx_version: request.libfx_version,
    }
  } finally {
    clearInterval(heartbeat)
    try {
      await agent?.close()
    } catch {
      // A close that throws has usually already been cancelled.
    }
  }
}

/**
 * reportRecovery surfaces libfx's own retry decisions.
 *
 * The runtime retries a model response it considers failed, and when it does the turn looks identical from
 * the outside: no text, no error, and then "out of turns". The reason it gives is on the ACP session
 * update, so that is where it is read from (§3.1 rule 3 — a host that cannot explain a failure is a host
 * nobody can operate).
 */
function reportRecovery(event: Record<string, unknown>, diagnostics: (event: { type: string; [key: string]: unknown }) => void): void {
  const message = event.message as { params?: { update?: { _meta?: { fx?: { modelResponseRecovery?: Record<string, unknown> } } } } } | undefined
  const recovery = message?.params?.update?._meta?.fx?.modelResponseRecovery
  if (recovery) diagnostics({ type: 'sidecar.recovery', ...recovery })
}

/**
 * saveCheckpoint persists the thread's state WITHOUT a stop_reason.
 *
 * That distinction is the whole contract: a checkpoint write that carries a stop_reason is the run's
 * terminal signal (§5.1), and the browser host sends one because nothing else sees its turn end. Here the
 * turn ends inside a call the server is blocking on, so the server records the outcome from the report
 * (§8.3). Sending a stop_reason too would complete the same run twice from two processes.
 */
async function saveCheckpoint(deps: SidecarDeps, request: SidecarRunRequest, token: string, checkpoint: string | undefined): Promise<void> {
  await deps.call('PUT', `/agent/threads/${encodeURIComponent(request.thread_id)}/checkpoint`, token, {
    checkpoint: checkpoint ?? '',
    libfx_version: request.libfx_version ?? '',
  })
}

/** callTool executes one tool through the server's surface, waiting out an approval inline (§5, §7). */
async function callTool(
  deps: SidecarDeps,
  token: { current: string },
  runId: string,
  name: string,
  input: unknown,
  signal: AbortSignal,
  diagnostics?: (event: { type: string; [key: string]: unknown }) => void,
): Promise<ToolCallResult> {
  // A failed call goes back to the model as an ordinary tool error, so nothing else would ever record it —
  // and a host that keeps retrying a call the server refuses is indistinguishable from a slow model. That
  // is why every refusal is announced here (§3.1 rule 3).
  const fail = (message: string): ToolCallResult => {
    diagnostics?.({ type: 'sidecar.tool_error', name, error: message })
    return { ok: false, message }
  }
  diagnostics?.({ type: 'sidecar.tool_call', name })
  let outcome: Record<string, unknown>
  try {
    outcome = await deps.call('POST', `/agent/runs/${encodeURIComponent(runId)}/tools/${encodeURIComponent(name)}`, token.current, {
      // libfx hands execute() a signal and no call id, so the server correlates by name (§4.4.1).
      input: input ?? {},
    })
  } catch (error) {
    return fail(error instanceof Error ? error.message : String(error))
  }
  if (outcome.status === 'pending' && typeof outcome.proposal_id === 'string') {
    return waitForDecision(deps, token, runId, outcome.proposal_id, signal, fail)
  }
  if (outcome.status === 'error' || outcome.is_error === true) {
    return fail(String(outcome.error ?? 'the tool reported an error'))
  }
  diagnostics?.({ type: 'sidecar.tool_result', name })
  return { ok: true, value: outcome.result ?? { status: 'ok' } }
}

/** waitForDecision holds the call open while the user decides. A sidecar has no page, which is exactly why
 *  §7 chose a long poll over an in-page event: the same wait works in both hosts. */
async function waitForDecision(
  deps: SidecarDeps,
  token: { current: string },
  runId: string,
  proposalId: string,
  signal: AbortSignal,
  fail: (message: string) => ToolCallResult,
): Promise<ToolCallResult> {
  for (;;) {
    if (signal.aborted) return fail('cancelled')
    let wait: Record<string, unknown>
    try {
      wait = await deps.call('GET', `/agent/runs/${encodeURIComponent(runId)}/approvals/${encodeURIComponent(proposalId)}`, token.current)
    } catch (error) {
      return fail(error instanceof Error ? error.message : String(error))
    }
    switch (wait.status) {
      case 'pending':
        continue
      case 'approved':
      case 'rejected':
        return { ok: true, value: wait.result ?? { decision: wait.status } }
      case 'conflict':
      case 'timeout':
        return fail(JSON.stringify(wait.result ?? { error: wait.status }))
      case 'cancelled':
        return fail('cancelled')
      default:
        return fail(`the run ended (${String(wait.status)}) before the decision arrived`)
    }
  }
}
