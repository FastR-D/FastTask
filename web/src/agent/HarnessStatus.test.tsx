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

vi.mock('../harness', () => ({
  warmUpHarness: vi.fn(async () => undefined),
  subscribeHarnessStatus: (listener: (status: HarnessStatus) => void) => {
    listeners.push(listener)
    return () => {
      listeners = listeners.filter(registered => registered !== listener)
    }
  },
}))

const { HarnessStatusLine, useOnline } = await import('./HarnessStatus')

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
