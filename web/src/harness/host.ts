import { createFxAgent, type FxAgent, type FxTurn, type FxTurnEvent } from './runtime'
import { ensureFreshAccessToken, request } from '../api'
import { absoluteAssetURL, cachedMode, selectMode, WASM_ASSET_PATH, type HarnessMode } from './backend'
import { createGatewayFetch } from './shim'
import { buildHostTools, type ToolCallResult, type ToolDef } from './tools'

// The browser harness host (doc/harness.md §3, §5, §6, §7, §10).
//
// What this file owns: one run at a time, driven from the capability the server issued, with the
// heartbeat that proves it is alive, the long poll that waits out an approval, and the checkpoint that
// makes the conversation portable. What it does NOT own: rendering (the UI reads the server's stream),
// persistence (nothing is written to localStorage or IndexedDB, §3.1 rule 2), and credentials (the token
// it holds is a capability, not a model key, §3.1 rule 4).
//
// Nothing outside web/src/harness/ may see a libfx type (§3.1 rule 1); the surface below is plain
// strings and a status object.

/** What the chat view is allowed to know about the harness. */
export type HarnessStatus = {
  /** 'idle' before the first run, 'unavailable' when no host can drive one (§3.2). */
  mode: HarnessMode | 'unavailable' | 'idle'
  /** Why that mode was chosen; shown verbatim when it is a degradation (§3.2: never silent). */
  reason?: string
  detail?: string
  running: boolean
  runId?: string
  /** Set once when a thread had no usable checkpoint and its history was summarized (§6.3). */
  contextDegraded?: boolean
  error?: string
}

type Listener = (status: HarnessStatus) => void

/** One driven run. Exactly one may exist at a time (§3.7). */
type ActiveRun = {
  runId: string
  agent: FxAgent | null
  turn: FxTurn | null
  token: string
  /** Filled in by the grant: the checkpoint endpoints are thread-scoped (§6.1). */
  threadId: string
  libfxVersion: string
  heartbeat: ReturnType<typeof setInterval> | null
  controller: AbortController
}

let active: ActiveRun | null = null
let status: HarnessStatus = { mode: cachedMode()?.mode ?? 'idle', reason: cachedMode()?.reason, running: false }
const listeners = new Set<Listener>()

/** subscribeHarnessStatus registers a UI listener and returns its unsubscribe. */
export function subscribeHarnessStatus(listener: Listener): () => void {
  listeners.add(listener)
  listener(status)
  return () => listeners.delete(listener)
}

/** harnessStatus is the current snapshot, for a component that mounts mid-run. */
export function harnessStatus(): HarnessStatus {
  return status
}

/** activeRunId is the run a host is driving, or null. */
export function activeRunId(): string | null {
  return active?.runId ?? null
}

function publish(patch: Partial<HarnessStatus>): void {
  status = { ...status, ...patch }
  for (const listener of listeners) listener(status)
}

/** diagnostics receives libfx events. They are logged, never rendered (§3.1 rule 3). */
function diagnostics(event: { type: string; [key: string]: unknown }): void {
  if (event.type === 'transport.error' || event.type === 'shim.error' || event.type === 'runtime.exit') {
    // eslint-disable-next-line no-console
    console.warn('[harness]', event.type, event.error ?? event.code ?? '')
  }
}

export type DriveRunOptions = {
  runId: string
  /** The user's message text. The server already stored it; the host needs it to prompt (§5.1). */
  text: string
  /** Forced mode from settings; null means probe (§3.2). */
  forcedMode?: HarnessMode | null
  /** Called when the run ends, whatever the outcome. */
  onDone?: (error?: string) => void
}

/**
 * driveRun drives one run to completion: grant → agent → prompt → checkpoint → completion. It is the
 * whole host loop, and it is the only place a libfx agent instance is created.
 *
 * One run at a time (§3.7, §15): a second call cancels and closes the first, because leaving a turn
 * running in the background is exactly what the instance budget forbids.
 */
export async function driveRun(options: DriveRunOptions): Promise<void> {
  const selection = await selectMode(options.forcedMode ?? null)
  publish({ mode: selection.mode, reason: selection.reason, detail: selection.detail })
  if (selection.mode === 'sidecar') {
    // The Worker drives a sidecar run; the browser only renders the stream (§1.2). Starting an agent
    // here would run the same turn twice.
    return
  }
  if (selection.mode === 'unavailable') {
    publish({ error: 'AGENT_UNAVAILABLE: no harness host can run in this browser' })
    options.onDone?.('unavailable')
    return
  }
  await teardownActive('a new run replaced it')

  const controller = new AbortController()
  const run: ActiveRun = {
    runId: options.runId, agent: null, turn: null, token: '',
    threadId: '', libfxVersion: '', heartbeat: null, controller,
  }
  active = run
  publish({ running: true, runId: run.runId, error: undefined })

  try {
    const manifest = await readManifest()
    const grant = await issueGrant(options.runId, manifest.etag)
    run.token = grant.harness_token
    run.threadId = grant.thread_id
    run.libfxVersion = grant.libfx_version
    publish({ contextDegraded: grant.context_degraded || undefined })

    const checkpoint = await readCheckpoint(run)
    const interval = Math.max(1, grant.heartbeat_interval_s || 10) * 1000
    // Beat once immediately, then on the interval the server asked for. The first beat is what tells
    // the server a host really arrived, so the loss detector's window starts from a known point
    // (§10.4) — and it is where a cancellation of an already-doomed run surfaces soonest (§5.2).
    void beat(run).catch(() => undefined)
    run.heartbeat = setInterval(() => { void beat(run).catch(() => undefined) }, interval)

    run.agent = await createFxAgent({
      apiKey: grant.harness_token,
      model: grant.model,
      wasm: absoluteAssetURL(WASM_ASSET_PATH),
      fetch: createGatewayFetch({
        runId: options.runId,
        harnessToken: () => run.token,
        model: grant.model,
        onEvent: diagnostics,
      }),
      instructions: grant.instructions,
      tools: buildHostTools(manifest.tools, (name, input, signal) => callTool(run, name, input, signal), {
        onWarn: message => diagnostics({ type: 'harness.warning', message }),
      }),
      ...(checkpoint ? { checkpoint } : {}),
      onEvent: diagnostics,
    })

    const turn = run.agent.prompt(promptBlocks(options.text), { signal: controller.signal })
    run.turn = turn
    // A turn has exactly one consumer and it must consume (§3.6); the events drive nothing but
    // diagnostics and cancellation, because the UI renders from the server's stream.
    for await (const event of turn as AsyncIterable<FxTurnEvent>) {
      if (event.type === 'tool_start') diagnostics({ type: 'harness.tool_start', name: event.name })
      if (controller.signal.aborted) break
    }
    const result = await turn.result
    await reportCompletion(run, result.stopReason)
    options.onDone?.()
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    publish({ error: message })
    await reportCompletion(run, 'error', message)
    options.onDone?.(message)
  } finally {
    await closeRun(run)
  }
}

/** promptBlocks carries the text, and any attachment the composer collected as a resource URI so the
 *  server can expand it after an ownership check (doc/chat-features.md §4.4). */
function promptBlocks(text: string): string {
  return text
}

/** cancelActiveRun is the user's stop button: it tells the server first — cancelling is the user's
 *  power, and a host may not cancel its own run (§5.2, §10.2) — then breaks the turn. */
export async function cancelActiveRun(): Promise<void> {
  const run = active
  if (!run) return
  try {
    await request(`/agent/runs/${run.runId}/cancellation`, { method: 'POST', body: JSON.stringify({}) })
  } catch {
    // The server may already have finished the run; the local cancel below still applies.
  }
  run.turn?.cancel()
  run.controller.abort()
}

/** teardownActive cancels and closes whatever is running, so at most one agent instance exists (§3.7). */
async function teardownActive(reason: string): Promise<void> {
  const previous = active
  if (!previous) return
  active = null
  previous.turn?.cancel()
  previous.controller.abort()
  diagnostics({ type: 'harness.teardown', reason })
  await closeRun(previous)
}

async function closeRun(run: ActiveRun): Promise<void> {
  if (run.heartbeat) clearInterval(run.heartbeat)
  run.heartbeat = null
  try {
    await run.agent?.close()
  } catch {
    // A close that throws has usually already been cancelled; nothing useful to do.
  }
  run.agent = null
  run.turn = null
  if (active === run) active = null
  publish({ running: active !== null, runId: active?.runId })
}

// --- the server surface a host uses ---

type RunGrant = {
  harness_token: string
  run_id: string
  thread_id: string
  model: string
  instructions: string
  tools_etag: string
  heartbeat_interval_s: number
  libfx_version: string
  reasoning: string
  context_degraded: boolean
}

type Manifest = { tools: ToolDef[]; etag: string }

let manifestCache: Manifest | null = null

/** readManifest fetches the tool projection. It is cached per session with its etag, which is what
 *  lets the server detect a manifest that changed mid-flight (§10.3). */
async function readManifest(): Promise<Manifest> {
  if (manifestCache) return manifestCache
  const { data, etag } = await request<{ tools: ToolDef[] }>('/agent/tools')
  manifestCache = { tools: data?.tools ?? [], etag: etag ?? '' }
  return manifestCache
}

/** forgetManifest drops the cache after a TOOLS_ETAG_STALE refusal (§10.3). */
export function forgetManifest(): void {
  manifestCache = null
}

async function issueGrant(runId: string, toolsEtag: string): Promise<RunGrant> {
  try {
    const { data } = await request<RunGrant>('/agent/runs', {
      method: 'POST',
      body: JSON.stringify({ run_id: runId, tools_etag: toolsEtag }),
    })
    return data
  } catch (error) {
    // The manifest changed under us: refetch it once and ask again (§10.3).
    if (isStaleManifest(error)) {
      forgetManifest()
      const manifest = await readManifest()
      const { data } = await request<RunGrant>('/agent/runs', {
        method: 'POST',
        body: JSON.stringify({ run_id: runId, tools_etag: manifest.etag }),
      })
      return data
    }
    throw error
  }
}

function isStaleManifest(error: unknown): boolean {
  const detail = error instanceof Error ? error.message : String(error)
  return detail.includes('TOOLS_ETAG_STALE')
}

/** readCheckpoint restores the thread's libfx history. A checkpoint written by another libfx version is
 *  refused by the server, which has already folded a summary into the instructions (§6.3). */
async function readCheckpoint(run: ActiveRun): Promise<Uint8Array | null> {
  try {
    const body = await hostFetch(run, 'GET', `/agent/threads/${encodeURIComponent(run.threadId)}/checkpoint`)
    if (body.skew || !body.checkpoint) return null
    return base64ToBytes(String(body.checkpoint))
  } catch {
    // A thread that has never run has no checkpoint; that is the normal first message.
    return null
  }
}

/**
 * reportCompletion stores the checkpoint and ends the run (§5.1: one turn, one checkpoint, written at
 * the end). It is also the failure path: a turn that threw reports stop_reason=error so the run does not
 * linger until the reaper notices. Without a grant there is nothing to report to, and §11 covers it.
 */
async function reportCompletion(run: ActiveRun, stopReason: string, errorMessage?: string): Promise<void> {
  if (!run.token || !run.threadId) return
  let checkpoint = ''
  try {
    // checkpoint() throws while a prompt is active, so this runs after turn.result settled.
    const bytes = await run.agent?.checkpoint()
    if (bytes) checkpoint = bytesToBase64(bytes)
  } catch {
    // Losing a checkpoint costs the next turn its history, not this run's transcript.
  }
  try {
    await hostFetch(run, 'PUT', `/agent/threads/${encodeURIComponent(run.threadId)}/checkpoint`, {
      checkpoint,
      libfx_version: run.libfxVersion,
      stop_reason: stopReason || 'stop',
      ...(errorMessage ? { error_message: errorMessage } : {}),
    })
  } catch {
    // The server's reaper ends a run whose host stopped reporting (§11).
  }
}

/** beat is the liveness channel. It also carries a cancellation to an idle host and hands back a
 *  rotated token (§5.2 channel 1, §10.4). */
async function beat(run: ActiveRun): Promise<void> {
  const body = await hostFetch(run, 'POST', `/agent/runs/${encodeURIComponent(run.runId)}/heartbeat`, {})
  if (typeof body.harness_token === 'string' && body.harness_token) run.token = body.harness_token
  if (body.cancel_requested === true) {
    run.turn?.cancel()
    run.controller.abort()
  }
}

/** callTool executes one tool through the server's surface, waiting out an approval inline so the same
 *  turn continues (§5, §7). */
async function callTool(run: ActiveRun, name: string, input: unknown, signal: AbortSignal): Promise<ToolCallResult> {
  let outcome
  try {
    outcome = await hostFetch(run, 'POST', `/agent/runs/${encodeURIComponent(run.runId)}/tools/${encodeURIComponent(name)}`, {
      // libfx does not hand a tool its call id (§4.4.1), so the server correlates by name.
      input: input ?? {},
    })
  } catch (error) {
    return { ok: false, message: error instanceof Error ? error.message : String(error) }
  }
  if (outcome.status === 'pending' && typeof outcome.proposal_id === 'string') {
    return waitForDecision(run, outcome.proposal_id, signal)
  }
  if (outcome.status === 'error' || outcome.is_error === true) {
    return { ok: false, message: String(outcome.error ?? 'the tool reported an error') }
  }
  return { ok: true, value: outcome.result ?? { status: 'ok' } }
}

/** waitForDecision holds the tool call open while the user decides. Each segment is at most 30 seconds,
 *  so an intermediate proxy cannot cut the wait, and the total is capped by the server (§7). */
async function waitForDecision(run: ActiveRun, proposalId: string, signal: AbortSignal): Promise<ToolCallResult> {
  for (;;) {
    if (signal.aborted) return { ok: false, message: 'cancelled' }
    let wait
    try {
      wait = await hostFetch(run, 'GET', `/agent/runs/${encodeURIComponent(run.runId)}/approvals/${encodeURIComponent(proposalId)}`)
    } catch (error) {
      return { ok: false, message: error instanceof Error ? error.message : String(error) }
    }
    switch (wait.status) {
      case 'pending':
        continue
      case 'approved':
      case 'rejected':
        return { ok: true, value: wait.result ?? { decision: wait.status } }
      case 'conflict':
      case 'timeout':
        return { ok: false, message: JSON.stringify(wait.result ?? { error: wait.status }) }
      case 'cancelled':
        return { ok: false, message: 'cancelled' }
      default:
        return { ok: false, message: `the run ended (${String(wait.status)}) before the decision arrived` }
    }
  }
}

/** hostFetch calls an endpoint with the run capability token. A user JWT would be refused here, and a
 *  harness token would be refused on the user endpoints — the two are deliberately not interchangeable
 *  (§10.2). */
async function hostFetch(run: ActiveRun, method: string, path: string, body?: unknown): Promise<Record<string, unknown>> {
  const response = await fetch(`/api/v1${path}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${run.token}`,
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  })
  if (!response.ok) {
    const problem = await response.json().catch(() => ({})) as { detail?: string; title?: string }
    throw new Error(problem.detail || problem.title || `${method} ${path} failed with ${response.status}`)
  }
  if (response.status === 204) return {}
  return (await response.json()) as Record<string, unknown>
}

// --- base64 helpers (the checkpoint travels as bytes, the wire carries base64) ---

function bytesToBase64(bytes: Uint8Array): string {
  let binary = ''
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000))
  }
  return btoa(binary)
}

function base64ToBytes(value: string): Uint8Array {
  const binary = atob(value)
  const bytes = new Uint8Array(binary.length)
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index)
  return bytes
}

/** ensureUserToken is exported for the sidecar entry, which authenticates the same way (§8). */
export async function ensureUserToken(): Promise<string | null> {
  return ensureFreshAccessToken()
}
