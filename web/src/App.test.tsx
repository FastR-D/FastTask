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
