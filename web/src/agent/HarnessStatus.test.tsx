import { act, cleanup, render, renderHook, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { HarnessStatus } from '../harness'

// The degradation status line (doc/harness.md §3.2, §9.3; test requirement §14.9, phase G's exit
// criterion).
//
// §14.9 asks for two things to be asserted together: which mode the agent is running in, and WHY, when
// that mode is a degradation. A silent fallback is the failure this component exists to prevent — a user on
// a browser without JSPI would otherwise have no idea the agent is running somewhere else, or nowhere.

let listeners: Array<(status: HarnessStatus) => void> = []

// Only the subscription and the probe are replaced; the preference module stays real, so these tests
// exercise the same storage round-trip the app does.
const { warmUp, resetProbe } = vi.hoisted(() => ({ warmUp: vi.fn(async () => undefined), resetProbe: vi.fn() }))

vi.mock('../harness', async (importOriginal) => {
  const actual = await importOriginal<Record<string, unknown>>()
  return {
    ...actual,
    warmUpHarness: warmUp,
    resetModeProbe: resetProbe,
    subscribeHarnessStatus: (listener: (status: HarnessStatus) => void) => {
      listeners.push(listener)
      return () => {
        listeners = listeners.filter(registered => registered !== listener)
      }
    },
  }
})

const { HarnessModeControl, HarnessStatusLine, useOnline } = await import('./HarnessStatus')

function publish(status: HarnessStatus) {
  act(() => {
    for (const listener of listeners) listener(status)
  })
}

function setOnline(online: boolean) {
  Object.defineProperty(navigator, 'onLine', { value: online, configurable: true })
  act(() => {
    window.dispatchEvent(new Event(online ? 'online' : 'offline'))
  })
}

function statusLine(): HTMLElement {
  return screen.getByTestId('harness-status')
}

beforeEach(() => {
  listeners = []
  warmUp.mockClear()
  resetProbe.mockClear()
  localStorage.clear()
  setOnline(true)
})

afterEach(cleanup)

describe('the harness status line', () => {
  it('says nothing before there is anything to say', () => {
    const { container } = render(<HarnessStatusLine />)
    expect(container).toBeEmptyDOMElement()
    publish({ mode: 'idle', running: false })
    expect(container).toBeEmptyDOMElement()
  })

  it('shows the local mode without marking it degraded', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'wasm', reason: 'LIBFX_JSPI_AVAILABLE', running: false })
    expect(statusLine()).toHaveTextContent('本机运行')
    expect(statusLine()).not.toHaveClass('degraded')
    expect(statusLine()).toHaveAttribute('data-mode', 'wasm')
    // An available-JSPI reason is not a degradation, so it must not be narrated as one.
    expect(statusLine().querySelector('.agent-harness-reason')).toBeNull()
  })

  it('shows the mode AND the reason when the browser cannot host the runtime (§14.9)', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'sidecar', reason: 'LIBFX_JSPI_UNAVAILABLE', running: false })
    expect(statusLine()).toHaveTextContent('服务端运行')
    expect(statusLine()).toHaveTextContent('当前浏览器不支持 WASM 线程，已改用服务端宿主')
    expect(statusLine()).toHaveClass('degraded')
  })

  it('shows the reason when the local host failed to load, which is a different story', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'sidecar', reason: 'LIBFX_WASM_LOAD_FAILED', running: false })
    expect(statusLine()).toHaveTextContent('本机宿主加载失败，已改用服务端宿主')
  })

  it('says the agent is unavailable rather than leaving the composer to fail on send', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'unavailable', reason: 'LIBFX_JSPI_UNAVAILABLE', detail: '服务端宿主未部署', running: false })
    expect(statusLine()).toHaveTextContent('不可用')
    expect(statusLine()).toHaveTextContent('服务端宿主未部署')
    expect(statusLine()).toHaveClass('degraded')
  })

  it('shows an unknown reason verbatim instead of dropping it', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'sidecar', reason: 'SOMETHING_NEW_FROM_A_FUTURE_LIBFX', running: false })
    expect(statusLine()).toHaveTextContent('SOMETHING_NEW_FROM_A_FUTURE_LIBFX')
  })

  it('carries the one-time notice that a thread’s history was compressed (§6.3)', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'wasm', running: true, contextDegraded: true })
    expect(statusLine()).toHaveTextContent('本会话早期上下文已压缩为摘要')
  })

  it('surfaces a host error', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'wasm', running: false, error: 'transport retry exhausted' })
    expect(statusLine()).toHaveTextContent('transport retry exhausted')
  })

  it('outranks the mode line when the device is offline (phase G)', () => {
    render(<HarnessStatusLine />)
    publish({ mode: 'wasm', running: false })
    expect(statusLine()).toHaveTextContent('本机运行')

    setOnline(false)
    expect(statusLine()).toHaveAttribute('data-mode', 'offline')
    expect(statusLine()).toHaveTextContent('离线')
    // The promise the offline line makes: cached history is readable, a new run is not.
    expect(statusLine()).toHaveTextContent('可以查看已缓存的对话，发起新对话需要联网')
    expect(statusLine()).toHaveClass('degraded')

    setOnline(true)
    expect(statusLine()).toHaveAttribute('data-mode', 'wasm')
  })

  it('unsubscribes on unmount, so a closed view cannot be published to', () => {
    const { unmount } = render(<HarnessStatusLine />)
    expect(listeners.length).toBe(1)
    unmount()
    expect(listeners.length).toBe(0)
  })
})

describe('useOnline', () => {
  it('follows the browser’s connectivity in both directions', () => {
    const { result } = renderHook(() => useOnline())
    expect(result.current).toBe(true)
    act(() => {
      setOnline(false)
    })
    expect(result.current).toBe(false)
    act(() => {
      setOnline(true)
    })
    expect(result.current).toBe(true)
  })
})

// §3.2 lets an admin or a user force a host, and §15 decides that the switch is exposed to ordinary users.
// What makes it worth a test is the part that is easy to get wrong: choosing a host has to take effect
// without a reload, which means the session's probe answer is dropped at the same moment.
describe('the host switch', () => {
  function select(): HTMLElement {
    const element = document.querySelector('.agent-harness-choice')
    if (!element) throw new Error('the host switch did not render')
    return element as HTMLElement
  }

  // React sets a custom element's prop as an attribute until the property exists on the instance, and as
  // the property afterwards, so the current value has to be read from whichever one holds it.
  function selectedValue(): string {
    const element = select() as HTMLElement & { value?: string }
    return String(element.value ?? element.getAttribute('value') ?? '')
  }

  // mdui-select is a custom element, so it has no native value setter for fireEvent to use. Setting the
  // property and dispatching the same 'change' event the component fires is what the app sees.
  function choose(value: string) {
    const element = select() as HTMLElement & { value?: string }
    element.value = value
    act(() => {
      element.dispatchEvent(new Event('change', { bubbles: true }))
    })
  }

  it('shows auto when the user has never chosen', () => {
    render(<HarnessModeControl />)
    expect(selectedValue()).toBe('auto')
  })

  it('shows the choice a previous session stored', () => {
    localStorage.setItem('fasttask.harness.mode', 'sidecar')
    render(<HarnessModeControl />)
    expect(selectedValue()).toBe('sidecar')
  })

  it('stores the choice, drops the cached probe and re-warms it', () => {
    render(<HarnessModeControl />)
    choose('wasm')

    expect(localStorage.getItem('fasttask.harness.mode')).toBe('wasm')
    expect(selectedValue()).toBe('wasm')
    // Without this the switch would only apply after a reload, which is not what "force" means.
    expect(resetProbe).toHaveBeenCalled()
    expect(warmUp).toHaveBeenCalled()
  })

  it('clears the setting when the user goes back to auto', () => {
    localStorage.setItem('fasttask.harness.mode', 'wasm')
    render(<HarnessModeControl />)
    choose('auto')
    expect(localStorage.getItem('fasttask.harness.mode')).toBeNull()
  })
})
