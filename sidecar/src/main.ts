import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { createRequire } from 'node:module'
import { existsSync } from 'node:fs'
import { dirname, isAbsolute, join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { driveSidecarRun, type SidecarAgentFactory, type SidecarRunRequest } from '../../web/src/harness/sidecar-driver'

// The Node sidecar (doc/harness.md §8).
//
// Its whole job is to be a libfx host for browsers that cannot run one: it takes a run capability from the
// Go worker, drives one turn, and reports how it ended. It holds no model credentials (§8.1 — the shim
// points at the same Go proxy the browser uses, and Go injects the key), touches no database, stores nothing
// on disk, and returns no transcript, because the proxy already wrote one (§8.3).
//
// It listens on loopback only, and authenticates with a secret generated when the supervisor started it
// (§8.2): a component that can drive a run must not be reachable from the network.

type Config = {
  endpoint: string
  secret: string
  serverOrigin: string
  webRoot: string
  libfxVersion: string
}

function readConfig(env: NodeJS.ProcessEnv): Config {
  return {
    endpoint: env.FASTTASK_SIDECAR_ENDPOINT ?? 'unix:///tmp/fasttask-sidecar.sock',
    secret: env.FASTTASK_SIDECAR_SECRET ?? '',
    serverOrigin: env.FASTTASK_SERVER_ORIGIN ?? 'http://127.0.0.1:10000',
    webRoot: env.FASTTASK_WEB_ROOT ?? join(dirname(process.cwd()), 'web'),
    libfxVersion: env.FASTTASK_LIBFX_VERSION ?? '0.0.10',
  }
}

/** loadLibfx resolves libfx's Node entry out of the web workspace, so the native addons are not installed
 *  twice (§8.2). The path is turned into a file URL because import() of a bare absolute path is ambiguous
 *  on Windows and unreliable for CJS interop. */
export async function loadLibfx(webRoot: string): Promise<{ createFxAgent: unknown; nativeAddon: boolean }> {
  // Node's createRequire needs an absolute path, and the supervisor may hand over a relative one that is
  // relative to ITS working directory rather than this process's.
  const root = isAbsolute(webRoot) ? webRoot : join(process.cwd(), webRoot)
  const require = createRequire(join(root, 'package.json'))
  const entry = require.resolve('libfx/node')
  // libfx's exports map does not expose package.json, so the package directory is derived from a module it
  // does expose. The native addon sits next to it, and its presence is what the health check reports
  // (§8.2: without it libfx falls back to Node's WASM backend, which on some versions needs a flag).
  const packageDir = dirname(require.resolve('libfx/wasm'))
  const nativeAddon = existsSync(join(packageDir, `libfx.${process.platform}-${process.arch}.node`))
  // libfx's Node entry is CommonJS, so import() nests its exports under `default`; the ESM entry would not.
  // Accepting either shape is what keeps this working across a libfx that changes its packaging.
  const loaded = (await import(pathToFileURL(entry).href)) as Record<string, unknown>
  const module = pickExports(loaded)
  if (typeof module.createFxAgent !== 'function') {
    throw new Error('libfx/node did not export createFxAgent')
  }
  return { createFxAgent: module.createFxAgent, nativeAddon }
}

/** pickExports unwraps the CommonJS interop shapes Node's import() produces. */
function pickExports(loaded: Record<string, unknown>): Record<string, unknown> {
  if (typeof loaded.createFxAgent === 'function') return loaded
  for (const key of ['default', 'module.exports']) {
    const candidate = loaded[key]
    if (candidate && typeof candidate === 'object' && typeof (candidate as Record<string, unknown>).createFxAgent === 'function') {
      return candidate as Record<string, unknown>
    }
  }
  return loaded
}

/** parseEndpoint turns the configured endpoint into a listen target, refusing anything that is not this
 *  machine. A sidecar bound to a routable interface would be a remote agent runtime. */
export function parseEndpoint(endpoint: string): { socket?: string; host?: string; port?: number } {
  if (endpoint.startsWith('unix://')) return { socket: endpoint.slice('unix://'.length) }
  const url = new URL(endpoint)
  if (url.protocol !== 'http:') throw new Error('a TCP sidecar endpoint must be plain http on loopback')
  const host = url.hostname.replace(/^\[|\]$/g, '')
  const loopback = host === 'localhost' || host === '127.0.0.1' || host === '::1'
  if (!loopback) throw new Error(`sidecar endpoint ${endpoint} is not loopback`)
  return { host, port: Number(url.port || 0) }
}

type Json = Record<string, unknown>

function json(response: ServerResponse, status: number, body: Json): void {
  const payload = JSON.stringify(body)
  response.writeHead(status, { 'content-type': 'application/json', 'content-length': String(Buffer.byteLength(payload)) })
  response.end(payload)
}

async function readBody(request: IncomingMessage, limit = 8 << 20): Promise<Json> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const chunk of request) {
    const buffer = chunk as Buffer
    size += buffer.length
    if (size > limit) throw new Error('request body too large')
    chunks.push(buffer)
  }
  if (chunks.length === 0) return {}
  return JSON.parse(Buffer.concat(chunks).toString('utf8')) as Json
}

export type SidecarServerOptions = {
  config: Config
  createAgent: SidecarAgentFactory
  nativeAddon: boolean
  /** Injected in tests so a fake call function can stand in for the FastTask API. */
  call?: (method: string, path: string, token: string, body?: unknown) => Promise<Json>
  onEvent?: (event: { type: string; [key: string]: unknown }) => void
}

/** createSidecarServer builds the request handler. It is exported so the routing and auth can be tested
 *  without binding a socket or loading libfx. */
export function createSidecarServer(options: SidecarServerOptions) {
  const running = new Map<string, AbortController>()

  const call = options.call ??
    (async (method: string, path: string, token: string, body?: unknown): Promise<Json> => {
      const response = await fetch(`${options.config.serverOrigin.replace(/\/+$/, '')}/api/v1${path}`, {
        method,
        headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      })
      const text = await response.text()
      const parsed = text ? (JSON.parse(text) as Json) : {}
      if (!response.ok) {
        throw new Error(String(parsed.detail ?? parsed.title ?? `${method} ${path} failed with ${response.status}`))
      }
      return parsed
    })

  return createServer((request, response) => {
    void handle(request, response).catch(error => {
      const message = error instanceof Error ? error.message : String(error)
      if (!response.headersSent) json(response, 500, { error: message })
      else response.end()
    })
  })

  async function handle(request: IncomingMessage, response: ServerResponse): Promise<void> {
    const url = new URL(request.url ?? '/', 'http://sidecar')
    // Every route is authenticated, including the health check: the supervisor presents the secret, and an
    // unauthenticated probe should not learn whether a host is running here (§8.2).
    if (!authorized(request, options.config.secret)) {
      json(response, 401, { error: 'unauthorized' })
      return
    }

    if (request.method === 'GET' && url.pathname === '/healthz') {
      json(response, 200, {
        ok: true,
        node_version: process.version,
        native_addon: options.nativeAddon,
        libfx_version: options.config.libfxVersion,
        // A Node whose WASM fallback needs a flag is a known trap (§8.2); the supervisor logs it.
        detail: options.nativeAddon ? '' : 'running libfx on the Node WASM backend',
      })
      return
    }

    if (request.method === 'POST' && url.pathname === '/run') {
      const body = (await readBody(request)) as unknown as SidecarRunRequest
      if (!body.run_id || !body.harness_token || !body.thread_id) {
        json(response, 400, { error: 'run_id, harness_token and thread_id are required' })
        return
      }
      const controller = new AbortController()
      running.set(body.run_id, controller)
      try {
        const report = await driveSidecarRun(
          { serverOrigin: options.config.serverOrigin, createAgent: options.createAgent, call, signal: controller.signal, onEvent: options.onEvent },
          body,
        )
        json(response, 200, report as unknown as Json)
      } finally {
        running.delete(body.run_id)
      }
      return
    }

    const cancel = url.pathname.match(/^\/run\/([^/]+)\/cancel$/)
    if (request.method === 'POST' && cancel) {
      const controller = running.get(decodeURIComponent(cancel[1]))
      controller?.abort()
      json(response, 200, { cancelled: controller !== undefined })
      return
    }

    json(response, 404, { error: 'not found' })
  }
}

function authorized(request: IncomingMessage, secret: string): boolean {
  if (!secret) return false
  const header = request.headers.authorization ?? ''
  return header === `Bearer ${secret}`
}

/** listen binds the server to the configured endpoint. */
export async function listen(server: ReturnType<typeof createServer>, endpoint: string): Promise<void> {
  const target = parseEndpoint(endpoint)
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    if (target.socket) server.listen(target.socket, () => resolve())
    else server.listen(target.port ?? 0, target.host ?? '127.0.0.1', () => resolve())
  })
}

async function main(): Promise<void> {
  const config = readConfig(process.env)
  if (!config.secret) {
    // Without a secret anything local could drive a run. Refusing to start is the only safe answer.
    throw new Error('FASTTASK_SIDECAR_SECRET is required')
  }
  const libfx = await loadLibfx(config.webRoot)
  const server = createSidecarServer({
    config,
    createAgent: libfx.createFxAgent as SidecarAgentFactory,
    nativeAddon: libfx.nativeAddon,
    onEvent: event => {
      // Diagnostics go to stderr, which the supervisor forwards to the server's log (§8.4). Everything a
      // run did that is not the transcript belongs here: the transcript is the server's, and a host that
      // cannot explain a refusal leaves an operator with nothing but a run that ran out of turns.
      const type = String(event.type ?? '')
      if (type.startsWith('sidecar.') || type === 'transport.error' || type === 'shim.error' || type === 'shim.unhandled') {
        const detail = [event.error, event.message, event.name, event.tools, event.path].filter(v => v !== undefined && v !== '').join(' ')
        process.stderr.write(`[sidecar] ${type}${detail ? ' ' + detail : ''}\n`)
      }
    },
  })
  await listen(server, config.endpoint)
  process.stdout.write(`[sidecar] listening on ${config.endpoint} (libfx ${config.libfxVersion}, native addon: ${libfx.nativeAddon})\n`)

  // §8.4: SIGTERM first, and the process must not leave a run driving itself after the supervisor is gone.
  let stopping = false
  const stop = (signal: string) => {
    if (stopping) return
    stopping = true
    process.stdout.write(`[sidecar] ${signal}; shutting down\n`)
    server.close(() => process.exit(0))
    setTimeout(() => process.exit(0), 3000).unref()
  }
  process.on('SIGTERM', () => stop('SIGTERM'))
  process.on('SIGINT', () => stop('SIGINT'))
}

// The bundle is imported by Node directly; a test that imports this module for its exports must not start a
// server, so main() only runs when Node executed the file itself.
const isEntryPoint = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href
if (isEntryPoint) {
  main().catch(error => {
    process.stderr.write(`[sidecar] fatal: ${error instanceof Error ? error.stack ?? error.message : String(error)}\n`)
    process.exit(1)
  })
}
