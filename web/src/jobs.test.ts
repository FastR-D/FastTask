import { describe, expect, it, vi } from 'vitest'

const { requestMock } = vi.hoisted(() => ({ requestMock: vi.fn() }))
vi.mock('./api', () => ({ request: requestMock }))

import { jobPollDelay, waitJob } from './jobs'

describe('waitJob', () => {
  it('keeps waiting for a slow model job instead of giving up after a few seconds', async () => {
    let clock = 0
    const statuses = [...Array(40).fill('running'), 'succeeded']
    requestMock.mockImplementation(async () => ({ data: { id: 'job_1', status: statuses.shift() } }))
    const job = await waitJob('job_1', { sleep: async ms => { clock += ms }, now: () => clock })
    expect(job.status).toBe('succeeded')
    // 40 polls is past a minute of simulated time — the old helper stopped at 7.5 seconds.
    expect(clock).toBeGreaterThan(60_000)
  })

  it('gives up with a pointer to the queue once the budget is spent', async () => {
    let clock = 0
    requestMock.mockImplementation(async () => ({ data: { id: 'job_2', status: 'running' } }))
    await expect(waitJob('job_2', { budgetMs: 10_000, sleep: async ms => { clock += ms }, now: () => clock })).rejects.toThrow('任务队列')
  })

  it('backs off from quick looks to a calm interval', () => {
    expect(jobPollDelay(0)).toBe(250)
    expect(jobPollDelay(10)).toBe(1000)
    expect(jobPollDelay(50)).toBe(3000)
  })
})
