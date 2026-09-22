// libfx@0.0.10 ships no type declarations at all: its package.json has neither "types" nor
// "typings" and the tarball contains no .d.ts (doc/harness.md §3.8). This file is our snapshot of
// the API FastTask actually uses, read off node_modules/libfx/fx-sdk.js at 0.0.10.
//
// It is NOT authoritative. Upgrading libfx means re-reading the source, and harness.contract.test.ts
// asserts the runtime shape we depend on so a silent change fails a test instead of a user's
// conversation.
//
// Two rules from §3.1 are enforced by harness.boundary.test.ts: nothing outside web/src/harness/
// may import libfx, and no type below may leak into the UI layer.

declare module 'libfx/wasm' {
  export * from 'libfx'
}

// The Node entry the sidecar loads (doc/harness.md §8.2). Same API, different backend: it prefers the
// native addon and falls back to WASM, which is why the sidecar reports which one it got.
declare module 'libfx/node' {
  export * from 'libfx'
}

declare module 'libfx' {
  /** What libfx hands a host tool besides its input. It carries an abort signal and NOTHING else —
   *  in particular no tool call id, which is why the server correlates a result by
   *  (run, tool name, newest part without one); see doc/harness.md §4.4.1. */
  export type HostToolContext = { signal: AbortSignal }

  /** A tool the host exposes to the model. libfx rejects more than 64 of them, a name outside
   *  /^[A-Za-z0-9_-]{1,64}$/, a missing description, or a non-object inputSchema. */
  export type HostTool = {
    name: string
    description: string
    inputSchema: Record<string, unknown>
    execute(input: unknown, context: HostToolContext): unknown | Promise<unknown>
  }

  /** A typed tool result. Returning this is the only way to tell libfx a call failed. */
  export type HostToolResult = {
    type: 'libfx.tool-result'
    text: string
    images: Array<{ type: 'image'; data: string; mimeType: string }>
    isError?: boolean
  }

  /** Diagnostic events from the runtime. A host consumes them for logging only and must not render
   *  from them (§3.1 rule 3). */
  export type FxAgentEvent = { type: string; timestamp: number } & Record<string, unknown>

  /** One normalized turn event. tool_start carries the call id, but it arrives on the event queue
   *  while execute() is called directly, so the two cannot be paired reliably. */
  export type FxTurnEvent =
    | { type: 'text_delta'; delta: string }
    | { type: 'reasoning_delta'; delta: string }
    | { type: 'tool_start'; id: string; name: string }
    | { type: 'tool_end'; id: string; name: string; content?: string; isError: boolean }

  export type FxTurnUsage = {
    inputTokens?: number
    outputTokens?: number
    cacheReadTokens?: number
    cacheWriteTokens?: number
    reasoningTokens?: number
  }

  export type FxTurnResult = { stopReason: string; usage?: FxTurnUsage }

  /** A turn has exactly ONE event consumer and it must consume: awaiting result alone waits for the
   *  queue to drain by itself. Breaking out of the iterator cancels the turn (§3.6). */
  export type FxTurn = {
    cancel(): void
    result: Promise<FxTurnResult>
    [Symbol.asyncIterator](): AsyncIterator<FxTurnEvent>
  }

  export type FxAgent = {
    prompt(input: string | FxPromptBlock[], options?: { signal?: AbortSignal }): FxTurn
    /** Only callable with no prompt in flight. The bytes are opaque and versioned by libfx. */
    checkpoint(): Promise<Uint8Array>
    close(): Promise<void>
  }

  /** prompt() input blocks. libfx rejects image blocks outright, so an attachment travels as a
   *  resource URI that the server expands after an ownership check (doc/chat-features.md §4.4). */
  export type FxPromptBlock =
    | { type: 'text'; text: string }
    | { type: 'resource'; resource: { uri: string; text?: string } }

  export type CreateFxAgentOptions = {
    /** Must be a non-empty string. FastTask passes the run capability token, never a model
     *  credential (§3.1 rule 4). */
    apiKey: string
    model?: string
    /** Absolute URL of the self-hosted fx-core.wasm (§9). */
    wasm?: string
    /** Overrides every network egress; this is where the gateway shim lives (§4.2). */
    fetch?: typeof fetch
    /** Capped at 64 KiB UTF-8 by libfx (§3.5). */
    instructions?: string | string[]
    tools?: HostTool[]
    checkpoint?: Uint8Array | ArrayBuffer
    gatewayChatUrl?: string
    onEvent?: (event: FxAgentEvent) => void
    onPermission?: (request: unknown) => Promise<string | null>
  }

  export function createFxAgent(options: CreateFxAgentOptions): Promise<FxAgent>

  /** Browser-side JSPI probe. libfx's browser entry exports this instead of getBackendInfo, which
   *  only exists in the Node entry (doc/harness.md §3.2 records the difference). */
  export function supportsJspi(): boolean

  export const libfxApiVersion: number
  export const fxSdkApiVersion: number
}
