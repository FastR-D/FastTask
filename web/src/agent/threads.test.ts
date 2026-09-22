import { beforeEach, describe, expect, it, vi } from 'vitest'
import { request } from '../api'
import {
  createThread,
  deleteThread,
  generateTitle,
  listThreads,
  renameThread,
  setArchived,
  threadState,
} from './threads'

// The thread catalogue client (doc/chat-features.md §2.2).
//
// What is under test is the contract: each operation hits the documented endpoint with the documented body,
// and the snake_case the API speaks is translated exactly once, here. A wrong path or a dropped cursor does
// not fail loudly in a browser — it silently shows one conversation instead of many.

vi.mock('../api', () => ({
  request: vi.fn(),
  idem: vi.fn(() => 'idem-key-1'),
  ensureFreshAccessToken: vi.fn(async () => 'jwt'),
}))

const mockRequest = vi.mocked(request)

beforeEach(() => {
  mockRequest.mockReset()
})

describe('thread catalogue client', () => {
  it('lists threads with the page size and cursor the API expects', async () => {
    mockRequest.mockResolvedValueOnce({
      data: {
        items: [
          { id: 'thr_1', title: '论文拆解', status: 'regular', last_message_at: '2026-09-23T01:00:00Z', created_at: '2026-09-22T01:00:00Z', updated_at: '2026-09-23T01:00:00Z' },
          { id: 'thr_2', title: '', status: 'archived', custom: '{"pinned":true}', last_message_at: null, created_at: '2026-09-21T01:00:00Z', updated_at: '2026-09-21T01:00:00Z' },
        ],
        next_cursor: 'cursor-2',
        has_more: true,
      },
      etag: null,
    } as never)

    const page = await listThreads('cursor-1', 'archived')
    const [path] = mockRequest.mock.calls[0]
    expect(path).toContain('/agent/threads?')
    expect(path).toContain('limit=30')
    expect(path).toContain('cursor=cursor-1')
    expect(path).toContain('status=archived')

    expect(page.nextCursor).toBe('cursor-2')
    expect(page.threads).toHaveLength(2)
    expect(page.threads[0]).toMatchObject({ id: 'thr_1', title: '论文拆解', archived: false })
    expect(page.threads[0].lastMessageAt?.toISOString()).toBe('2026-09-23T01:00:00.000Z')
    // An untitled thread still renders something, and a custom blob is parsed, not guessed at.
    expect(page.threads[1].title).toBe('新对话')
    expect(page.threads[1].archived).toBe(true)
    expect(page.threads[1].custom).toEqual({ pinned: true })
  })

  it('survives a custom blob it cannot parse (§2.3: display data is untrusted)', async () => {
    mockRequest.mockResolvedValueOnce({
      data: { items: [{ id: 'thr_3', title: 'x', status: 'regular', custom: '{not json', created_at: '', updated_at: '' }], next_cursor: null, has_more: false },
      etag: null,
    } as never)
    const page = await listThreads()
    expect(page.threads[0].custom).toEqual({})
  })

  it('creates a thread idempotently and returns the server id', async () => {
    mockRequest.mockResolvedValueOnce({ data: { remote_id: 'thr_new' }, etag: null } as never)
    const id = await createThread(null)
    expect(id).toBe('thr_new')
    const [path, init] = mockRequest.mock.calls[0]
    expect(path).toBe('/agent/threads')
    expect((init as RequestInit).method).toBe('POST')
    // A retry of the same click must not create a second empty conversation (§1.6).
    expect(new Headers((init as RequestInit).headers).get('Idempotency-Key')).toBe('idem-key-1')
  })

  it('renames, archives, unarchives and deletes through the documented routes', async () => {
    mockRequest.mockResolvedValue({ data: {}, etag: null } as never)
    await renameThread('thr_1', '新标题')
    await setArchived('thr_1', true)
    await setArchived('thr_1', false)
    await deleteThread('thr_1')

    const calls = mockRequest.mock.calls.map(([path, init]) => `${(init as RequestInit)?.method ?? 'GET'} ${path}`)
    expect(calls).toEqual([
      'PATCH /agent/threads/thr_1',
      'POST /agent/threads/thr_1/archive',
      'POST /agent/threads/thr_1/unarchive',
      'DELETE /agent/threads/thr_1',
    ])
    const patchBody = JSON.parse(String((mockRequest.mock.calls[0][1] as RequestInit).body))
    expect(patchBody).toEqual({ title: '新标题' })
  })

  it('asks the server for a deterministic title instead of generating one locally (§2.4)', async () => {
    mockRequest.mockResolvedValueOnce({ data: { title: '帮我把这周的实验排一下顺序' }, etag: null } as never)
    const title = await generateTitle('thr_1')
    expect(title).toBe('帮我把这周的实验排一下顺序')
    expect(mockRequest.mock.calls[0][0]).toBe('/agent/threads/thr_1/title')
    expect((mockRequest.mock.calls[0][1] as RequestInit).method).toBe('POST')
  })

  it('reads a named thread’s transcript, not whichever one was last', async () => {
    const state = { messages: [], isRunning: false, fasttask: { threadId: 'thr_9', pendingProposals: [] } }
    mockRequest.mockResolvedValueOnce({ data: { state }, etag: null } as never)
    const loaded = await threadState('thr_9')
    expect(mockRequest.mock.calls[0][0]).toBe('/agent/threads/thr_9/state')
    expect(loaded.fasttask.threadId).toBe('thr_9')
  })
})
