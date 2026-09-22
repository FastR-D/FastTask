import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { brotliCompressSync, constants as zlibConstants, gzipSync } from 'node:zlib'
import { defineConfig, type Plugin } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// The libfx runtime the browser host compiles (doc/harness.md §9.1). It is resolved from the installed
// package rather than copied into the repository, so the asset and the SDK that reads it can never
// drift apart. libfx's exports map does not expose the .wasm file, so the path is derived from the
// sibling module it does expose.
const require = createRequire(import.meta.url)

function fxCoreWasmPath(): string {
  return join(dirname(require.resolve('libfx/wasm')), 'fx-core.wasm')
}

/**
 * selfHostFxCore emits fx-core.wasm — plus brotli and gzip variants — into the bundle.
 *
 * Self-hosting is a hard requirement, not an optimization: the runtime must not reach a CDN
 * (ADR-0005 §3.3 "运行时零 Vercel 访问"), and the PWA has to be able to load the host offline
 * (doc/pwa.md). The precompressed siblings exist because a 2 MB download on a research-group wifi is
 * the difference between the agent starting and the user giving up; Go serves whichever the client
 * accepts (doc/harness.md §9.2).
 */
function selfHostFxCore(): Plugin {
  return {
    name: 'fasttask:self-host-fx-core',
    apply: 'build',
    generateBundle() {
      const bytes = readFileSync(fxCoreWasmPath())
      this.emitFile({ type: 'asset', fileName: 'fx-core.wasm', source: bytes })
      this.emitFile({
        type: 'asset',
        fileName: 'fx-core.wasm.br',
        source: brotliCompressSync(bytes, { params: { [zlibConstants.BROTLI_PARAM_QUALITY]: zlibConstants.BROTLI_MAX_QUALITY } }),
      })
      this.emitFile({ type: 'asset', fileName: 'fx-core.wasm.gz', source: gzipSync(bytes, { level: 9 }) })
    },
  }
}

export default defineConfig({
  plugins: [
    react(),
    selfHostFxCore(),
    // pwa.md §3: an installable, offline-readable PWA. injectManifest (a custom
    // src/sw.ts) is required because generateSW cannot express the per-user cache
    // key the spec mandates for the snapshot routes (§3.2). registerType 'prompt'
    // gives the explicit "有新版本，点击刷新" UX with no silent force-reload; the
    // page drives it through useRegisterSW, so injectRegister is disabled.
    VitePWA({
      strategies: 'injectManifest',
      srcDir: 'src',
      filename: 'sw.ts',
      registerType: 'prompt',
      injectRegister: null,
      manifest: {
        name: 'FastTask · 3Signals',
        short_name: 'FastTask',
        description: 'FastTask · 3Signals：把漫长的研究，压缩成今天能动手的三件事。',
        lang: 'zh-CN',
        start_url: '/',
        scope: '/',
        display: 'standalone',
        orientation: 'portrait',
        // §3.1: a single theme_color (the light value); the dark value stays in
        // index.html's prefers-color-scheme media-query meta.
        theme_color: '#fef7ff',
        background_color: '#fef7ff',
        icons: [
          { src: '/pwa-192x192.png', sizes: '192x192', type: 'image/png', purpose: 'any' },
          { src: '/pwa-512x512.png', sizes: '512x512', type: 'image/png', purpose: 'any' },
          { src: '/maskable-icon-512x512.png', sizes: '512x512', type: 'image/png', purpose: 'maskable' },
        ],
      },
      injectManifest: {
        // Precache the app shell, the install icons and the libfx core so the PWA can start offline.
        // wasm was missing from this list, which meant the host could not load without a network
        // (doc/harness.md §9.3). Offline still cannot run a conversation — the model call needs the
        // network — and the chat view says so instead of failing on send.
        //
        // The .br/.gz siblings are deliberately absent: they are encodings of fx-core.wasm, not
        // separate resources, and precaching all three would store the runtime three times.
        globPatterns: ['**/*.{js,css,html,woff2,woff,png,svg,ico,wasm}'],
        maximumFileSizeToCacheInBytes: 4 * 1024 * 1024,
      },
    }),
  ],
  server: { proxy: { '/api': 'http://127.0.0.1:10000', '/health': 'http://127.0.0.1:10000' } },
  test: {
    environment: 'jsdom',
    environmentOptions: { jsdom: { url: 'http://localhost/' } },
    setupFiles: './src/test-setup.ts',
  },
})
