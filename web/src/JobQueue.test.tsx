import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { JobQueue } from './App'
import type { Job } from './types'

const base: Job = {
  id: 'job_running_demo', type: 'conversation', status: 'running', subject_type: 'conversation', subject_id: 'conv_demo',
  base_revision: 0, attempt_count: 1, max_attempts: 3, cancel_requested: false, revision: 2,
  run_after: '2026-09-16T01:00:00Z', created_at: '2026-09-16T01:00:00Z', updated_at: '2026-09-16T01:00:01Z', started_at: '2026-09-16T01:00:01Z',
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

describe('JobQueue', () => {
  it('shows live status counts and failed job details', async () => {
    const failed: Job = { ...base, id: 'job_failed_demo', type: 'daily_plan_generation', status: 'failed', attempt_count: 3, revision: 5, error_code: 'MODEL_TIMEOUT', error_message: 'provider timed out', finished_at: '2026-09-16T01:02:00Z' }
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ items: [base, failed] }), { status: 200, headers: { 'Content-Type': 'application/json' } })))
    render(<JobQueue onNotice={vi.fn()} />)

    expect(await screen.findByText('生成对话回复')).toBeInTheDocument()
    expect(screen.getByText('运行中', { selector: '.job-state b' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /失败1/ })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: /失败1/ }))
    expect(await screen.findByText('生成每日计划')).toBeInTheDocument()
    expect(screen.getByText('MODEL_TIMEOUT')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
  })
})
