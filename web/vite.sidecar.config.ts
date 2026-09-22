import { defineConfig } from 'vitest/config'

// The sidecar build (doc/harness.md §8.2).
//
// It lives in web/ rather than sidecar/ for one reason: the sidecar reuses web/src/harness/ — the same
// gateway shim and the same tool projection the browser host uses — and resolving @ai-sdk/* and libfx has
// to happen against web/node_modules. Bundling those keeps the emitted file self-contained; only libfx stays
// external, because its native addon is loaded from disk at runtime and cannot be inlined.
export default defineConfig({
  // No public directory: this bundle is a Node process, and copying the PWA icons into it would put the
  // app shell in the sidecar's dist for no reason.
  publicDir: false,
  build: {
    target: 'node20',
    outDir: '../sidecar/dist',
    emptyOutDir: true,
    // A supervisor reads this process's stderr; minified stack traces are not worth the bytes.
    minify: false,
    sourcemap: true,
    lib: {
      entry: '../sidecar/src/main.ts',
      formats: ['es'],
      fileName: () => 'host.mjs',
    },
    rollupOptions: {
      external: [/^node:/, 'libfx', 'libfx/node'],
      output: { inlineDynamicImports: true },
    },
  },
})
