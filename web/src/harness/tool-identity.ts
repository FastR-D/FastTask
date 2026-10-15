// libfx 0.0.10 gives execute() only input and signal. Carry the actual model call identity inside
// its private input instead of pairing an asynchronous diagnostic event or guessing by tool name.
// The shim removes this envelope from model requests, including restored checkpoint history.
export const TOOL_INPUT_MARKER = 'fasttask.host-tool-input/v1'

export type ToolInputEnvelope = {
  $fasttask: typeof TOOL_INPUT_MARKER
  tool_call_id: string
  model_turn: number
  input: unknown
}

type RegisteredCall = { name: string; envelope: ToolInputEnvelope; signature: string; claimed?: boolean }

export class ToolCallBindings {
  private turn = 0
  private ready = false
  private staged = new Map<string, RegisteredCall>()
  private calls = new Map<string, RegisteredCall>()

  beginTurn(): number {
    if (!this.ready && this.turn !== 0) throw new Error('TOOL_CALL_TURN_OVERLAP: a model response is still open')
    this.turn += 1
    this.ready = false
    this.staged.clear()
    return this.turn
  }

  bind(turn: number, id: string, name: string, rawInput: string): ToolInputEnvelope {
    if (turn !== this.turn || this.ready) throw new Error('TOOL_CALL_TURN_INVALID: the model response is no longer open')
    if (!id || !name) throw new Error('TOOL_CALL_ID_REQUIRED: the model response omitted its call identity')
    if (this.calls.has(id) || this.staged.has(id)) throw new Error('TOOL_CALL_ID_REUSED: distinct model calls reused an identity')
    const envelope: ToolInputEnvelope = { $fasttask: TOOL_INPUT_MARKER, tool_call_id: id, model_turn: turn, input: JSON.parse(rawInput) }
    this.staged.set(id, { name, envelope, signature: canonical(envelope) })
    return envelope
  }

  finishTurn(turn: number): void {
    if (turn !== this.turn || this.ready) throw new Error('TOOL_CALL_TURN_INVALID: cannot finish this model response')
    for (const [id, call] of this.staged) this.calls.set(id, call)
    this.staged.clear()
    this.ready = true
  }

  failTurn(turn: number): void {
    if (turn !== this.turn) return
    this.staged.clear()
    this.ready = true
  }

  resolve(name: string, input: unknown): ToolInputEnvelope {
    if (!isToolInputEnvelope(input)) throw new Error('TOOL_CALL_ID_REQUIRED: the tool input has no verified model identity')
    const call = this.calls.get(input.tool_call_id)
    if (!call || call.name !== name || call.signature !== canonical(input)) {
      throw new Error('TOOL_CALL_ID_MISMATCH: the tool identity, name or complete input does not match the model response')
    }
    // A claimed execution can only replay its existing promise in buildHostTools. An unclaimed old
    // call cannot be borrowed by a later model response, even when its name and arguments are equal.
    if ((!this.ready || input.model_turn !== this.turn) && !call.claimed) throw new Error('TOOL_CALL_TURN_INVALID: the tool belongs to a different model response')
    call.claimed = true
    return call.envelope
  }
}

export function isToolInputEnvelope(value: unknown): value is ToolInputEnvelope {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false
  const object = value as Record<string, unknown>
  return object.$fasttask === TOOL_INPUT_MARKER && typeof object.tool_call_id === 'string' && !!object.tool_call_id &&
    Number.isSafeInteger(object.model_turn) && Object.hasOwn(object, 'input') && Object.keys(object).length === 4
}

/** The schema is private to libfx; the shim presents its original input schema to the model. */
export function envelopeSchema(inputSchema: Record<string, unknown>): Record<string, unknown> {
  return {
    type: 'object', additionalProperties: false,
    properties: {
      $fasttask: { const: TOOL_INPUT_MARKER }, tool_call_id: { type: 'string' }, model_turn: { type: 'integer' }, input: inputSchema,
    },
    required: ['$fasttask', 'tool_call_id', 'model_turn', 'input'],
  }
}

export function originalSchema(schema: unknown): unknown {
  const properties = (schema as { properties?: Record<string, unknown> } | undefined)?.properties
  return (properties?.$fasttask as { const?: string } | undefined)?.const === TOOL_INPUT_MARKER ? properties?.input : schema
}

export function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  if (value && typeof value === 'object') {
    return `{${Object.keys(value).sort().map(key => `${JSON.stringify(key)}:${canonical((value as Record<string, unknown>)[key])}`).join(',')}}`
  }
  return JSON.stringify(value)
}
