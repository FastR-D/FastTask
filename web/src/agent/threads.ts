import { idem, request } from '../api'
import type { ServerAgentState } from './state'

// The thread catalogue client (doc/chat-features.md §2).
//
// Every function here is one HTTP call against the endpoints in doc/interface.md §20.3, and none of them
// caches: the server owns the list, so a second device sees the same conversations and a rename on one
// tab is not contradicted by a stale copy on another.
//
// Field names cross a boundary in this file only — the API speaks snake_case (doc/interface.md §1.1), the
// UI speaks camelCase.

/** One thread as the API returns it. */
export type ThreadRow = {
  id: string
  title: string
  status: 'regular' | 'archived' | string
  goal_id?: string | null
  custom?: string
  last_message_at?: string | null
  created_at: string
  updated_at: string
}

/** A thread as the UI renders it. `custom` is parsed defensively: it is display data written by a client,
 *  so a blob this build cannot read must not break the list (§2.3). */
export type ThreadSummary = {
  id: string
  title: string
  archived: boolean
  goalId: string | null
  lastMessageAt: Date | null
  custom: Record<string, unknown>
}

type ThreadListPage = {
  items: ThreadRow[]
  next_cursor: string | null
  has_more: boolean
}

/** PAGE_SIZE is how many threads one call returns. */
export const PAGE_SIZE = 30

function summaryOf(row: ThreadRow): ThreadSummary {
  return {
    id: row.id,
    title: row.title || '新对话',
    archived: row.status === 'archived',
    goalId: row.goal_id ?? null,
    lastMessageAt: row.last_message_at ? safeDate(row.last_message_at) : null,
    custom: parseCustom(row.custom),
  }
}

function parseCustom(raw: string | undefined): Record<string, unknown> {
  if (!raw) return {}
  try {
    const parsed = JSON.parse(raw) as unknown
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) return parsed as Record<string, unknown>
  } catch {
    // Ignore a blob written by another client; it is display data, not a contract.
  }
  return {}
}

function safeDate(value: string): Date | null {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? null : date
}

function threadPath(threadId: string): string {
  return `/agent/threads/${encodeURIComponent(threadId)}`
}

/** listThreads returns one page, newest activity first, plus the cursor for the next one. */
export async function listThreads(cursor?: string, status?: 'regular' | 'archived'): Promise<{ threads: ThreadSummary[]; nextCursor: string | null }> {
  const query = new URLSearchParams({ limit: String(PAGE_SIZE) })
  if (cursor) query.set('cursor', cursor)
  if (status) query.set('status', status)
  const { data } = await request<ThreadListPage>(`/agent/threads?${query.toString()}`)
  return {
    threads: (data?.items ?? []).map(summaryOf),
    nextCursor: data?.next_cursor ?? null,
  }
}

/** createThread starts an empty conversation and returns its server id (§2.2). */
export async function createThread(goalId?: string | null): Promise<string> {
  const { data } = await request<{ remote_id: string }>('/agent/threads', {
    method: 'POST',
    headers: { 'Idempotency-Key': idem() },
    body: JSON.stringify(goalId ? { goal_id: goalId } : {}),
  })
  return data.remote_id
}

/** renameThread sets a user-chosen title. */
export async function renameThread(threadId: string, title: string): Promise<void> {
  await request(threadPath(threadId), { method: 'PATCH', body: JSON.stringify({ title }) })
}

/** generateTitle asks the server for the deterministic title (§2.4): no model call, no quota. */
export async function generateTitle(threadId: string): Promise<string> {
  const { data } = await request<{ title: string }>(`${threadPath(threadId)}/title`, { method: 'POST' })
  return data.title
}

/** setArchived archives or restores a thread. Archiving keeps every row; deleting does not (§2.5). */
export async function setArchived(threadId: string, archived: boolean): Promise<void> {
  await request(`${threadPath(threadId)}/${archived ? 'archive' : 'unarchive'}`, { method: 'POST' })
}

/** deleteThread removes a conversation and everything under it, including its attachments (§2.5). */
export async function deleteThread(threadId: string): Promise<void> {
  await request(threadPath(threadId), { method: 'DELETE' })
}

/** threadState reads one thread's transcript in the shape the SSE stream pushes (§2.7), which is what lets
 *  a switched-to or reloaded conversation render before its next run. */
export async function threadState(threadId: string): Promise<ServerAgentState> {
  const { data } = await request<{ state: ServerAgentState }>(`${threadPath(threadId)}/state`)
  return data.state
}
