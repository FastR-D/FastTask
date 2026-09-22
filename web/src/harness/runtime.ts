export { createFxAgent, supportsJspi } from 'libfx/wasm'
// The types travel through here too, so this file stays the single place that names libfx. A type-only
// import from the package root would be erased at runtime, but it would also be one keystroke away from
// a value import that drags fx-term.wasm back into the bundle.
export type {
  CreateFxAgentOptions,
  FxAgent,
  FxAgentEvent,
  FxPromptBlock,
  FxTurn,
  FxTurnEvent,
  FxTurnResult,
  FxTurnUsage,
  HostTool,
  HostToolContext,
  HostToolResult,
} from 'libfx/wasm'

// The single place the browser host reaches libfx (doc/harness.md §3, §9.1).
//
// It imports the `libfx/wasm` entry rather than the package root on purpose. The root browser entry
// resolves BOTH default wasm assets at module scope — `new URL("./fx-core.wasm", …)` and
// `new URL("./fx-term.wasm", …)` — so importing it makes the bundler emit fx-term.wasm, the 4.7 MB
// interactive terminal this project never uses (§9.1: "浏览器产物不得包含它们"). The wasm entry has no
// default asset URLs, and this host always passes `wasm` explicitly (§9), so nothing is pulled in
// implicitly.
//
// The Node sidecar imports `libfx/node` in its own entry (doc/harness.md §8.2) and reuses shim.ts and
// tools.ts from here; it does not load this module.
