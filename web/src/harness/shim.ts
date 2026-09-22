import { createOpenAICompatible } from '@ai-sdk/openai-compatible'
import type { LanguageModelV4CallOptions, LanguageModelV4StreamPart } from '@ai-sdk/provider'

// The gateway shim (doc/harness.md §4.2).
//
// libfx speaks LanguageModelV4 to https://ai-gateway.vercel.sh. FastTask runs no such gateway and
// lets no runtime reach Vercel (ADR-0005 §3.3), so the shim intercepts that one URL inside the host
// process, translates the call with @ai-sdk/openai-compatible, and sends OpenAI-compatible requests
// to our own proxy — which is where the real credentials are injected. The V4 protocol never crosses
// a process boundary, and the shim never sees a model key.
//
// The same file is used by the browser host and by the Node sidecar; the only difference is where
// serverOrigin comes from (§4.2).

/** The URL libfx calls by default. Only requests to it are intercepted. */
export const GATEWAY_ORIGIN = 'https://ai-gateway.vercel.sh'

export type GatewayFetchOptions = {
  /** The run this host is driving. It scopes the proxy endpoint, so a token cannot be replayed. */
  runId: string
  /** The run capability token, or a getter for it. A getter is what lets a heartbeat rotate the token
   *  mid-run without rebuilding the agent (§10.4). It is a placeholder for the provider key, not one
   *  (§10.2). */
  harnessToken: string | (() => string)
  /** Absolute origin of the FastTask server. Required in Node, inferred in a browser. */
  serverOrigin?: string
  /** The model id the server handed out. libfx puts it in the request body; this is the fallback. */
  model?: string
  /** Diagnostics hook; the shim reports what it did and never the credentials (§3.1 rule 3). */
  onEvent?: (event: { type: string; [key: string]: unknown }) => void
}

/**
 * gatewayBaseURL builds the proxy base the OpenAI-compatible SDK is pointed at.
 *
 * It MUST be absolute: the SDK runs `new URL()` on it, and a relative path throws
 * "Failed to construct 'URL'" — the trap the Phase 0 spike hit (§16.3). An absolute same-origin URL
 * is still same-origin, so no CORS preflight is involved.
 */
export function gatewayBaseURL(runId: string, serverOrigin?: string): string {
  const origin = (serverOrigin ?? globalThis.location?.origin ?? '').replace(/\/+$/, '')
  if (!origin) {
    // No origin and no explicit server: refuse loudly rather than build a relative URL that would
    // throw from inside the SDK, where nothing can attribute the failure.
    throw new TypeError('the gateway shim needs an absolute server origin')
  }
  return `${origin}/api/v1/agent/runs/${encodeURIComponent(runId)}/openai`
}

/** isGatewayRequest reports whether a fetch target is the AI Gateway call the shim owns. */
export function isGatewayRequest(input: RequestInfo | URL): boolean {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url
  return url.startsWith(GATEWAY_ORIGIN)
}

/**
 * createGatewayFetch returns the fetch override handed to createFxAgent. Everything that is not the
 * gateway call passes through untouched, so a future libfx that fetches something else still works —
 * and a bug in the shim cannot silently swallow an unrelated request.
 */
export function createGatewayFetch(options: GatewayFetchOptions): typeof fetch {
  const passthrough = globalThis.fetch?.bind(globalThis)
  const baseURL = gatewayBaseURL(options.runId, options.serverOrigin)

  return async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    if (!isGatewayRequest(input)) {
      if (!passthrough) throw new TypeError('fetch is unavailable')
      return passthrough(input, init)
    }
    try {
      // Inside the try on purpose: a body this shim cannot read must surface as an error response, so
      // libfx fails the turn cleanly instead of seeing its fetch throw.
      const callOptions = parseCallOptions(init?.body)
      const modelId = bodyModel(callOptions) ?? options.model ?? 'server-decides'
      options.onEvent?.({ type: 'shim.request', model: modelId, endpoint: maskOrigin(baseURL) })
      const model = createOpenAICompatible({
        name: 'fasttask',
        baseURL,
        // A placeholder: the server replaces it with the provider key (§4.3).
        apiKey: currentToken(options.harnessToken),
      }).languageModel(modelId)
      const { stream } = await model.doStream({
        ...callOptions,
        // Cancellation must reach the upstream call, or a cancelled turn keeps generating.
        abortSignal: init?.signal ?? undefined,
      } as LanguageModelV4CallOptions)
      return streamResponseOf(stream)
    } catch (error) {
      // libfx retries once on a transport failure (§3.6); answering with an error response is what
      // makes that path run instead of leaving the turn hanging.
      options.onEvent?.({ type: 'shim.error', error: messageOf(error) })
      return errorResponseOf(error)
    }
  }
}

function currentToken(token: string | (() => string)): string {
  return typeof token === 'function' ? token() : token
}

/** parseCallOptions reads the LanguageModelV4CallOptions libfx serializes into the request body. */
export function parseCallOptions(body: BodyInit | null | undefined): LanguageModelV4CallOptions {
  if (typeof body !== 'string') {
    throw new TypeError('the gateway shim expects a JSON request body')
  }
  const parsed = JSON.parse(body) as LanguageModelV4CallOptions & { model?: string }
  if (!parsed || typeof parsed !== 'object') {
    throw new TypeError('the gateway request body is not an object')
  }
  return parsed
}

function bodyModel(callOptions: unknown): string | undefined {
  const model = (callOptions as { model?: unknown }).model
  return typeof model === 'string' && model.trim() ? model : undefined
}

/** streamResponseOf re-encodes V4 stream parts as the SSE the gateway would have returned. */
export function streamResponseOf(stream: ReadableStream<LanguageModelV4StreamPart>): Response {
  const encoder = new TextEncoder()
  const body = new ReadableStream<Uint8Array>({
    async start(controller) {
      const reader = stream.getReader()
      try {
        for (;;) {
          const { value, done } = await reader.read()
          if (done) break
          controller.enqueue(encoder.encode(`data: ${JSON.stringify(value)}\n\n`))
        }
        controller.close()
      } catch (error) {
        // An error mid-stream becomes a V4 error part, which is how the SDK reports one.
        controller.enqueue(encoder.encode(`data: ${JSON.stringify({ type: 'error', error: messageOf(error) })}\n\n`))
        controller.close()
      } finally {
        reader.releaseLock()
      }
    },
  })
  return new Response(body, {
    status: 200,
    headers: { 'content-type': 'text/event-stream; charset=utf-8', 'cache-control': 'no-cache' },
  })
}

function errorResponseOf(error: unknown): Response {
  return new Response(JSON.stringify({ error: { message: messageOf(error) } }), {
    status: 502,
    headers: { 'content-type': 'application/json' },
  })
}

function messageOf(error: unknown): string {
  if (error instanceof Error) return error.message
  return String(error)
}

/** maskOrigin keeps a diagnostic line from carrying a URL a log reader could mistake for an endpoint
 *  we call. The shim talks to one place and says so without spelling it out (§4.5). */
function maskOrigin(baseURL: string): string {
  try {
    return new URL(baseURL).pathname
  } catch {
    return 'proxy'
  }
}
