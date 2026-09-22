import { createFxAgent, supportsJspi } from './runtime'

// Mode selection (doc/harness.md §3.2).
//
// WASM is the default and the sidecar is a fallback for browsers without JSPI. The probe runs once
// per session and its result lives in memory only: a browser upgrade changes what is available, and a
// cached answer would keep misjudging it for as long as the storage lives. Degradation is reported,
// never silent — the chat view shows the mode and the reason.
//
// libfx's browser entry exports supportsJspi() rather than the getBackendInfo() the spec names; that
// function only exists in the Node entry the sidecar uses. The probe therefore combines the flag with
// a real createFxAgent, which is what actually proves the 2 MB core compiles here.

export type HarnessMode = 'wasm' | 'sidecar'

/** Reason codes, matching the ones §3.2 lists so an operator can grep for them. */
export const REASON_FORCED = 'FORCED'
export const REASON_JSPI = 'LIBFX_JSPI_AVAILABLE'
export const REASON_NO_JSPI = 'LIBFX_JSPI_UNAVAILABLE'
export const REASON_WASM_FAILED = 'LIBFX_WASM_LOAD_FAILED'

export type ModeSelection = {
  /** 'unavailable' means neither host can run, and the agent must be presented as unavailable rather
   *  than degraded into a single-turn reply (§3.2). */
  mode: HarnessMode | 'unavailable'
  reason: string
  detail?: string
}

/** Where the self-hosted fx-core.wasm is served from (§9). It is copied there at build time. */
export const WASM_ASSET_PATH = '/fx-core.wasm'

let cached: ModeSelection | null = null
let probing: Promise<ModeSelection> | null = null

/** cachedMode exposes the in-memory probe result without triggering a probe. */
export function cachedMode(): ModeSelection | null {
  return cached
}

/** resetModeProbe clears the session's answer. Tests use it; nothing in the app does. */
export function resetModeProbe(): void {
  cached = null
  probing = null
}

/**
 * selectMode returns the host to use. A forced mode is adopted without probing (§3.2: an admin or a
 * user who knows their browser beats a probe), and the result is shared by concurrent callers so the
 * wasm is compiled once.
 */
export async function selectMode(forced?: HarnessMode | null): Promise<ModeSelection> {
  if (forced === 'wasm' || forced === 'sidecar') {
    return { mode: forced, reason: REASON_FORCED }
  }
  if (cached) return cached
  if (!probing) {
    probing = probe().then(selection => {
      cached = selection
      probing = null
      return selection
    })
  }
  return probing
}

/** warmUpHarness starts the probe before the first message needs it (§3.2). The 2 MB compile must not
 *  land on the send path, and a failure here is not an error: the probe result is read later. */
export function warmUpHarness(): Promise<ModeSelection> {
  return selectMode(null)
}

async function probe(): Promise<ModeSelection> {
  if (!jspiAvailable()) {
    return { mode: 'sidecar', reason: REASON_NO_JSPI }
  }
  try {
    await compileProbe()
    return { mode: 'wasm', reason: REASON_JSPI }
  } catch (error) {
    return {
      mode: 'sidecar',
      reason: REASON_WASM_FAILED,
      detail: error instanceof Error ? error.message : String(error),
    }
  }
}

function jspiAvailable(): boolean {
  try {
    return supportsJspi() === true
  } catch {
    // A probe that throws tells us the same thing as one that returns false.
    return false
  }
}

/** compileProbe really creates an agent and closes it, so "the wasm loads" is a measured fact rather
 *  than an inference from a feature flag. No network is involved: the gateway is only called during a
 *  prompt, and this never prompts. */
async function compileProbe(): Promise<void> {
  const absoluteWasm = absoluteAssetURL(WASM_ASSET_PATH)
  const agent = await createFxAgent({
    apiKey: 'probe',
    wasm: absoluteWasm,
    tools: [],
    instructions: '',
  })
  await agent.close()
}

/** absoluteAssetURL resolves a public asset against the page origin. libfx resolves a missing wasm
 *  option against its own module URL, which in a bundle points somewhere the asset is not (§9). */
export function absoluteAssetURL(path: string): string {
  const origin = globalThis.location?.origin
  if (!origin) return path
  return new URL(path, origin).href
}
