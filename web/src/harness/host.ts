import { createFxAgent, type FxAgent, type FxTurn } from './runtime'
import { ensureFreshAccessToken, request, token, readOfflineUser } from '../api'
import { absoluteAssetURL, cachedMode, selectMode, WASM_ASSET_PATH, type HarnessMode } from './backend'
import type { ToolDef } from './tools'
import { driveSharedRun } from './run-driver'
import { assertHarnessOwner } from './owner'
import { requestRunCancellation } from './cancellation'

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
  ownerId: string
  getOwner: () => string | undefined
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
  ownerId?: string
  getOwner?: () => string | undefined
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
  const ownerId = options.ownerId ?? readOfflineUser()?.id ?? ''
  const getOwner = options.getOwner ?? (() => readOfflineUser()?.id)
  const checkOwner = () => assertHarnessOwner(ownerId, getOwner(), token.get())
  checkOwner()
  const selection = await selectMode(options.forcedMode ?? null)
  checkOwner()
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
  checkOwner()

  const controller = new AbortController()
  const run: ActiveRun = {
    runId: options.runId, ownerId, getOwner, agent: null, turn: null, token: '',
    threadId: '', libfxVersion: '', heartbeat: null, controller,
  }
  active = run
  publish({ running: true, runId: run.runId, error: undefined })

  try {
    const manifest = await readManifest(ownerId)
    checkOwner()
    const grant = await issueGrant(options.runId, manifest.etag, ownerId)
    checkOwner()
    controller.signal.throwIfAborted()
    run.token = grant.harness_token
    run.threadId = grant.thread_id
    run.libfxVersion = grant.libfx_version
    publish({ contextDegraded: grant.context_degraded || undefined })

    const checkpoint = await readCheckpoint(run)
    checkOwner()
    controller.signal.throwIfAborted()
    const report = await driveSharedRun({
      serverOrigin: globalThis.location.origin,
      createAgent: settings => createFxAgent({ ...settings, fetch: (input, init) => { checkOwner(); return settings.fetch(input, init) }, wasm: absoluteAssetURL(WASM_ASSET_PATH) }),
      call: (method, path, token, body, signal) => hostFetch({ ...run, token }, method, path, body, signal),
      signal: controller.signal,
      onToken: token => { run.token = token },
      onEvent: diagnostics,
    }, {
      run_id: run.runId, thread_id: run.threadId, harness_token: run.token,
      prompt: grant.prompt ?? options.text,
      model: grant.model, instructions: grant.instructions,
      libfx_version: grant.libfx_version, heartbeat_interval_s: grant.heartbeat_interval_s,
      checkpoint: checkpoint ? bytesToBase64(checkpoint) : undefined,
    })
    await hostFetch(run, 'PUT', `/agent/threads/${encodeURIComponent(run.threadId)}/checkpoint`, {
      ...report, checkpoint: report.checkpoint ?? '',
    })
    checkOwner()
    if (report.error_message) publish({ error: report.error_message })
    options.onDone?.()
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    if (getOwner() === ownerId) {
      publish({ error: message })
      await reportCompletion(run, 'error', message)
      options.onDone?.(message)
    }
    controller.abort()
  } finally {
    await closeRun(run)
  }
}

/** cancelActiveRun is the user's stop button: it tells the server first — cancelling is the user's
 *  power, and a host may not cancel its own run (§5.2, §10.2) — then breaks the turn. */
export async function cancelActiveRun(runId = active?.runId): Promise<void> {
  const run = active?.runId === runId ? active : null
  await requestRunCancellation(runId, id => request(`/agent/runs/${encodeURIComponent(id)}/cancellation`, {
    method: 'POST', body: JSON.stringify({}),
  }), () => {
    run?.turn?.cancel()
    run?.controller.abort()
  })
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
  if (run.getOwner() === run.ownerId) publish({ running: active !== null, runId: active?.runId })
}

// --- the server surface a host uses ---

type RunGrant = {
  harness_token: string
  prompt?: string
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
async function readManifest(ownerId: string): Promise<Manifest> {
  if (manifestCache) return manifestCache
  const { data, etag } = await request<{ tools: ToolDef[] }>('/agent/tools', {}, ownerId)
  manifestCache = { tools: data?.tools ?? [], etag: etag ?? '' }
  return manifestCache
}

/** forgetManifest drops the cache after a TOOLS_ETAG_STALE refusal (§10.3). */
export function forgetManifest(): void {
  manifestCache = null
}

async function issueGrant(runId: string, toolsEtag: string, ownerId: string): Promise<RunGrant> {
  try {
    const { data } = await request<RunGrant>('/agent/runs', {
      method: 'POST',
      body: JSON.stringify({ run_id: runId, tools_etag: toolsEtag }),
    }, ownerId)
    return data
  } catch (error) {
    // The manifest changed under us: refetch it once and ask again (§10.3).
    if (isStaleManifest(error)) {
      forgetManifest()
      const manifest = await readManifest(ownerId)
      const { data } = await request<RunGrant>('/agent/runs', {
        method: 'POST',
        body: JSON.stringify({ run_id: runId, tools_etag: manifest.etag }),
      }, ownerId)
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
  const body = await hostFetch(run, 'GET', `/agent/threads/${encodeURIComponent(run.threadId)}/checkpoint`)
  if (body.skew || !body.checkpoint) return null
  return base64ToBytes(String(body.checkpoint))
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

/** hostFetch calls an endpoint with the run capability token. A user JWT would be refused here, and a
 *  harness token would be refused on the user endpoints — the two are deliberately not interchangeable
 *  (§10.2). */
async function hostFetch(run: ActiveRun, method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<Record<string, unknown>> {
  assertHarnessOwner(run.ownerId, run.getOwner(), token.get())
  const response = await fetch(`/api/v1${path}`, {
    method,
    signal: signal ? AbortSignal.any([signal, AbortSignal.timeout(35_000)]) : AbortSignal.timeout(15_000),
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
