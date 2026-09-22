import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

export default defineConfig({
  plugins: [
    react(),
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
        // Precache the app shell plus the install icons so the PWA works offline.
        globPatterns: ['**/*.{js,css,html,woff2,woff,png,svg,ico}'],
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
