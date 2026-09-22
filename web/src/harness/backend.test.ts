import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createFxAgent, supportsJspi } from 'libfx/wasm'
import { cachedMode, resetModeProbe, selectMode, warmUpHarness, WASM_ASSET_PATH, absoluteAssetURL } from './backend'

// Mode selection (doc/harness.md §3.2).
//
// libfx is mocked: the real probe compiles 2 MB of wasm, and what is under test here is the decision —
// a forced mode is adopted without probing, a missing capability degrades to the sidecar with a reason,
// and the answer is remembered in memory only.

vi.mock('libfx/wasm', () => {
  const close = vi.fn(async () => undefined)
  return {
    supportsJspi: vi.fn(() => true),
    createFxAgent: vi.fn(async () => ({ prompt: vi.fn(), checkpoint: vi.fn(), close })),
    libfxApiVersion: 2,
  }
})

const mockSupportsJspi = vi.mocked(supportsJspi)
const mockCreateAgent = vi.mocked(createFxAgent)

beforeEach(() => {
  resetModeProbe()
  localStorage.clear()
  mockSupportsJspi.mockReset().mockReturnValue(true)
  mockCreateAgent.mockReset().mockResolvedValue({ prompt: vi.fn(), checkpoint: vi.fn(), close: vi.fn(async () => undefined) } as never)
})

describe('selectMode', () => {
  it('adopts a forced mode without probing', async () => {
    const selection = await selectMode('sidecar')
    expect(selection).toEqual({ mode: 'sidecar', reason: 'FORCED' })
    expect(mockSupportsJspi).not.toHaveBeenCalled()
    expect(mockCreateAgent).not.toHaveBeenCalled()
  })

  it('selects wasm when the browser can run it', async () => {
    const selection = await selectMode(null)
    expect(selection.mode).toBe('wasm')
    expect(selection.reason).toBe('LIBFX_JSPI_AVAILABLE')
    // The probe really creates an agent, so "the core compiles here" is measured rather than inferred.
    expect(mockCreateAgent).toHaveBeenCalledTimes(1)
    const options = mockCreateAgent.mock.calls[0][0]
    expect(options.wasm).toBe(absoluteAssetURL(WASM_ASSET_PATH))
    // …and does not leave a 2 MB instance behind.
    const agent = await mockCreateAgent.mock.results[0].value
    expect(agent.close).toHaveBeenCalled()
  })

  it('degrades to the sidecar when JSPI is missing, with the reason (§3.2)', async () => {
    mockSupportsJspi.mockReturnValue(false)
    const selection = await selectMode(null)
    expect(selection.mode).toBe('sidecar')
    expect(selection.reason).toBe('LIBFX_JSPI_UNAVAILABLE')
    expect(mockCreateAgent).not.toHaveBeenCalled()
  })

  it('degrades to the sidecar when the wasm will not load', async () => {
    mockCreateAgent.mockRejectedValue(new Error('WebAssembly.instantiate: JSPI unsupported') as never)
    const selection = await selectMode(null)
    expect(selection.mode).toBe('sidecar')
    expect(selection.reason).toBe('LIBFX_WASM_LOAD_FAILED')
    expect(selection.detail).toContain('JSPI')
  })

  it('probes once per session and keeps the answer in memory only', async () => {
    await warmUpHarness()
    const first = cachedMode()
    const second = await selectMode(null)
    expect(mockCreateAgent).toHaveBeenCalledTimes(1)
    expect(second).toEqual(first)
    // A cached probe in storage would outlive the browser that produced it, and a browser upgrade
    // changes the answer (§3.2).
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
  })

  it('shares one probe between concurrent callers', async () => {
    const [a, b] = await Promise.all([selectMode(null), selectMode(null)])
    expect(a).toEqual(b)
    expect(mockCreateAgent).toHaveBeenCalledTimes(1)
  })
})
