import { request } from './api'
import type { Job } from './types'

const TERMINAL = ['succeeded', 'failed', 'cancelled']

// jobPollDelay backs off from a quick first look (deterministic local jobs finish in well under a
// second) to a calm interval for a real model call, which routinely takes tens of seconds.
export function jobPollDelay(attempt: number): number {
  if (attempt < 8) return 250
  if (attempt < 20) return 1000
  return 3000
}

// waitJob polls an agent job until it reaches a terminal state. The budget covers a full attempt — the
// agent loop's three-minute wall clock — plus a retry, because giving up after a few seconds, as both
// earlier copies did, reported a job that was merely slow as stuck and never refreshed the view when it
// finished.
export async function waitJob(
  id: string,
  { budgetMs = 5 * 60_000, sleep = (ms: number) => new Promise<void>(resolve => setTimeout(resolve, ms)), now = () => Date.now(), readJob = async (jobID:string) => (await request<Job>(`/agent-jobs/${jobID}`)).data } = {},
): Promise<Job> {
  const deadline = now() + budgetMs
  for (let attempt = 0; ; attempt++) {
    await sleep(jobPollDelay(attempt))
    const data = await readJob(id)
    if (TERMINAL.includes(data.status)) return data
    if (now() >= deadline) throw new Error('作业仍在运行，完成后可在「任务队列」查看结果')
  }
}
