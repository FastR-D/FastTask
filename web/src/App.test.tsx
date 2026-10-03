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
