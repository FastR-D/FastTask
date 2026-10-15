import type { HostTool, HostToolResult } from './runtime'
import { ToolCallBindings, envelopeSchema } from './tool-identity'

// Tool descriptors → host tools (doc/harness.md §3.4).
//
// The server's manifest is the only source of truth: nothing here declares a tool, a schema or a
// default. A tool the registry does not list cannot be called, and a schema the registry changed is
// picked up on the next run because the manifest is read per grant.
//
// libfx calls execute(input, { signal }) with NO tool call id. The shared shim therefore wraps the
// exact input with its actual model identity, verified here before any host request can be made.

/** One entry of GET /agent/tools. Field names are the server's (§3.4). */
export type ToolDef = {
  name: string
  description: string
  input_schema: Record<string, unknown>
}

/** What a tool call ended with. The host turns the server's outcome into this before returning. */
export type ToolCallResult = { ok: true; value: unknown } | { ok: false; message: string }

/** Executes one call against the server's tool surface. */
export type ToolExecution = (name: string, input: unknown, signal: AbortSignal, toolCallId: string) => Promise<ToolCallResult>

export type BuildHostToolsOptions = {
  /** One per run, shared with the gateway shim in both browser and sidecar hosts. */
  bindings?: ToolCallBindings
  /** libfx rejects more than 64 tools; §3.4 asks for a warning before that cliff. */
  onWarn?: (message: string) => void
}

/** TOOL_LIMIT is libfx's own cap, mirrored so the warning threshold has a name. */
export const TOOL_LIMIT = 64
/** TOOL_WARN_AT is where the manifest starts being close enough to the cap to say so (§3.4). */
export const TOOL_WARN_AT = 60

/**
 * buildHostTools wraps each server descriptor in a HostTool. The wrapper does three things: keep
 * same-name calls sequential, translate a failed call into the typed result libfx needs to tell the
 * model it failed, and turn an abort into a rejected call rather than a hung one.
 */
export function buildHostTools(defs: ToolDef[], execute: ToolExecution, options: BuildHostToolsOptions = {}): HostTool[] {
  if (!Array.isArray(defs)) throw new TypeError('the tool manifest must be an array')
  if (defs.length > TOOL_LIMIT) {
    throw new RangeError(`the server advertised ${defs.length} tools; libfx accepts at most ${TOOL_LIMIT}`)
  }
  if (defs.length > TOOL_WARN_AT) {
    options.onWarn?.(`the agent tool manifest holds ${defs.length} tools; libfx accepts at most ${TOOL_LIMIT}`)
  }
  const bindings = options.bindings ?? new ToolCallBindings()
  // Retain conservative same-name scheduling, but identity never depends on execution order.
  const tails = new Map<string, Promise<unknown>>()
  const executions = new Map<string, Promise<unknown>>()
  return defs.map(def => ({
    name: def.name,
    description: def.description,
    inputSchema: envelopeSchema(def.input_schema ?? { type: 'object', properties: {} }),
    execute(input: unknown, context: { signal: AbortSignal }) {
      if (context.signal?.aborted) return Promise.resolve(errorResult('cancelled'))
      let call
      try {
        call = bindings.resolve(def.name, input)
      } catch (error) {
        return Promise.resolve(errorResult(error instanceof Error ? error.message : String(error)))
      }
      const retried = executions.get(call.tool_call_id)
      if (retried) return retried
      const previous = tails.get(def.name) ?? Promise.resolve()
      const current = previous.then(
        () => runTool(def.name, call.input, context.signal, call.tool_call_id, execute),
        () => runTool(def.name, call.input, context.signal, call.tool_call_id, execute),
      )
      executions.set(call.tool_call_id, current)
      tails.set(def.name, current.then(noop, noop))
      return current
    },
  }))
}

function noop() {
  /* the chain must not reject into the next call */
}

async function runTool(name: string, input: unknown, signal: AbortSignal, toolCallId: string, execute: ToolExecution): Promise<unknown> {
  if (signal?.aborted) return errorResult('cancelled')
  try {
    const result = await execute(name, input, signal, toolCallId)
    return result.ok ? normalize(result.value) : errorResult(result.message)
  } catch (error) {
    // A thrown error is fed back to the model as a failed call, not as a crashed turn (§5 item 6).
    return errorResult(error instanceof Error ? error.message : String(error))
  }
}

/** normalize keeps a JSON-serializable value as-is; libfx stringifies anything that is not a string. */
function normalize(value: unknown): unknown {
  if (value === undefined) return 'null'
  return value
}

/** errorResult is the typed shape libfx requires in order to mark a call failed. */
export function errorResult(message: string): HostToolResult {
  return { type: 'libfx.tool-result', text: message, images: [], isError: true }
}

/** manifestNames is a diagnostic helper: the names a host will register, in server order. */
export function manifestNames(defs: ToolDef[]): string[] {
  return defs.map(def => def.name)
}
