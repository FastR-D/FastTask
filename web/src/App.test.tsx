import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

describe('App', () => {
  beforeEach(() => { localStorage.clear(); vi.restoreAllMocks() })
  it('shows the local login experience for signed-out users', () => {
    render(<App />)
    expect(screen.getByRole('heading', { name: '进入工作台' })).toBeInTheDocument()
    expect(screen.getByText(/压缩成今天能动手的三件事/)).toBeInTheDocument()
  })
})

describe('loginErrorText', () => {
  it('turns statuses into messages for the person signing in', async () => {
    const { loginErrorText } = await import('./App')
    const { ApiError } = await import('./api')
    expect(loginErrorText(new ApiError(401, 'invalid credentials'))).toBe('账号或密码不正确')
    expect(loginErrorText(new ApiError(429, 'too many failed sign-in attempts'))).toBe('失败次数过多，请几分钟后再试')
    expect(loginErrorText(new ApiError(503, 'x'))).toBe('服务暂时不可用，请稍后再试')
    expect(loginErrorText(new TypeError('Failed to fetch'))).toBe('无法连接服务器，请检查网络')
  })
})

describe('Login', () => {
  beforeEach(() => { localStorage.clear(); vi.restoreAllMocks() })
  it('signs in with Enter right after typing, before any field loses focus', async () => {
    const { fireEvent, act } = await import('@testing-library/react')
    const fetchMock = vi.fn(async (url: string) => {
      if (url.endsWith('/auth/fastcas/available')) return new Response(JSON.stringify({ enabled: false }), { status: 200 })
      return new Response(JSON.stringify({ detail: 'invalid credentials' }), { status: 401 })
    })
    vi.stubGlobal('fetch', fetchMock)
    const { container } = render(<App />)
    const typeInto = (label: string, value: string) => {
      const element = container.querySelector(`mdui-text-field[label="${label}"]`) as HTMLElement & { value?: string }
      element.value = value
      act(() => { element.dispatchEvent(new Event('input', { bubbles: true })) })
    }
    typeInto('账号', 'admin')
    typeInto('密码', 'a-password-typed-just-now')
    fireEvent.keyDown(container.querySelector('form')!, { key: 'Enter' })
    await screen.findByText('账号或密码不正确')
    const loginCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/auth/login')) as unknown as [string, RequestInit] | undefined
    expect(loginCall).toBeDefined()
    expect(JSON.parse(String(loginCall![1].body))).toEqual({ identifier: 'admin', password: 'a-password-typed-just-now' })
    vi.unstubAllGlobals()
  })
})
