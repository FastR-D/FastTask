export {
  driveRun,
  cancelActiveRun,
  activeRunId,
  harnessStatus,
  subscribeHarnessStatus,
  forgetManifest,
  type DriveRunOptions,
  type HarnessStatus,
} from './host'
export { selectMode, warmUpHarness, cachedMode, resetModeProbe, type HarnessMode, type ModeSelection } from './backend'
export { createGatewayFetch, gatewayBaseURL, isGatewayRequest, GATEWAY_ORIGIN } from './shim'
export { buildHostTools, errorResult, manifestNames, TOOL_LIMIT, TOOL_WARN_AT, type ToolDef } from './tools'

// The harness boundary (doc/harness.md §3.1).
//
// This module is the ONLY surface the UI may import. No libfx type appears in it, so the rule "libfx
// types do not exist outside web/src/harness/" is checkable by a test rather than by review, and the UI
// cannot accidentally start depending on turn events or HostTool shapes.
