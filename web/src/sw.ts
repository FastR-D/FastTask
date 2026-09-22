/// <reference lib="webworker" />
// FastTask service worker (pwa.md §3). Built by vite-plugin-pwa's injectManifest
// pipeline (esbuild), NOT by `tsc -b` — sw.ts is excluded from tsconfig.app.json
// because the WebWorker lib's `self` would clash with the app's DOM lib. The
// pure cache-key logic lives in ./pwa/cacheKey.ts, which IS typechecked and
// unit-tested. The type-only declarations below are stripped by esbuild and exist
// for editor correctness.
declare let self: ServiceWorkerGlobalScope

import { cleanupOutdatedCaches, createHandlerBoundToURL, precacheAndRoute } from 'workbox-precaching'
import { NavigationRoute, registerRoute } from 'workbox-routing'
import { NetworkFirst, NetworkOnly, StaleWhileRevalidate } from 'workbox-strategies'
import { CacheableResponsePlugin } from 'workbox-cacheable-response'
import { ExpirationPlugin } from 'workbox-expiration'
import { AGENT_RE, SNAPSHOT_RE, userScopedCacheKey } from './pwa/cacheKey'

// workbox-build replaces the literal token self.__WB_MANIFEST with the precache
// manifest (index.html + hashed /assets + fonts + icons) at build time. Precached
// entries are served CacheFirst with revisioning (pwa.md §3.2: /assets → CacheFirst).
precacheAndRoute(self.__WB_MANIFEST)
cleanupOutdatedCaches()

// Navigations → NetworkFirst (pwa.md §3.2: index.html → NetworkFirst), falling
// back to the precached index.html when the network and the HTML cache both miss
// (offline cold start). The server already sends no-store for index.html.
const offlineShell = createHandlerBoundToURL('index.html')
const htmlStrategy = new NetworkFirst({
  cacheName: 'ft-html',
  networkTimeoutSeconds: 3,
  plugins: [new CacheableResponsePlugin({ statuses: [200] })],
})
registerRoute(new NavigationRoute(async options => {
  try {
    return await htmlStrategy.handle(options)
  } catch {
    return offlineShell(options)
  }
}))

// Agent transport endpoints → NetworkOnly, explicitly and ahead of every other
// /api rule. These carry SSE streams; a stream must never enter a Workbox cache
// (pwa.md §3.2: "/api/v1/agent/** NetworkOnly，且必须显式排除").
registerRoute(({ url }) => AGENT_RE.test(url.pathname), new NetworkOnly())

// Per-user read snapshots → StaleWhileRevalidate (pwa.md §3.2). The cache key is
// namespaced by the authenticated user so a shared device never serves one
// user's stale plan/goals/task-tree to another. Only the cache KEY is scoped; the
// network request is sent unchanged. Bounded by entry count and age.
registerRoute(
  ({ request, url }) => request.method === 'GET' && SNAPSHOT_RE.test(url.pathname),
  new StaleWhileRevalidate({
    cacheName: 'ft-api-snapshots',
    plugins: [
      {
        cacheKeyWillBeUsed: async ({ request }) =>
          userScopedCacheKey(request.url, request.headers.get('Authorization')),
      },
      new CacheableResponsePlugin({ statuses: [200] }),
      new ExpirationPlugin({ maxEntries: 60, maxAgeSeconds: 60 * 60 * 24 }),
    ],
  }),
)

// Every other /api route → NetworkOnly. Offline writes are intentionally not
// queued (pwa.md §5); the UI surfaces "需要联网" instead of failing silently.
registerRoute(({ url }) => url.pathname.startsWith('/api/'), new NetworkOnly())

// registerType 'prompt' (pwa.md §3.2: autoUpdate semantics with an EXPLICIT
// "有新版本，点击刷新" prompt, never a silent force-reload — the user may be
// mid-conversation). The page posts SKIP_WAITING after the user accepts.
self.addEventListener('message', event => {
  if (event.data && (event.data as { type?: string }).type === 'SKIP_WAITING') {
    void self.skipWaiting()
  }
})
