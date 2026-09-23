import { useCallback, useEffect, useState } from 'react'
import { fieldValue } from '../mdui-react'
import {
  createThread,
  deleteThread,
  generateTitle,
  listThreads,
  renameThread,
  setArchived,
  type ThreadSummary,
} from './threads'

// The conversation list (doc/chat-features.md §2).
//
// It is deliberately plain: the server owns the list, so this component reads it, mutates it through the
// endpoints, and re-reads. Nothing is cached here, because a rename on another device has to show up
// without a reload. Archive and delete are visually distinct on purpose — one keeps every row and the other
// takes the runs, messages, checkpoint and attachments with it (§2.5).

type Props = {
  activeId: string | null
  onOpen: (threadId: string | null) => void
  onNotice: (message: string) => void
}

export function ThreadList({ activeId, onOpen, onNotice }: Props) {
  const [threads, setThreads] = useState<ThreadSummary[]>([])
  const [showArchived, setShowArchived] = useState(false)
  const [nextCursor, setNextCursor] = useState<string | null>(null)
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  const [loading, setLoading] = useState(false)

  const load = useCallback(async (cursor?: string) => {
    setLoading(true)
    try {
      const page = await listThreads(cursor, showArchived ? 'archived' : 'regular')
      setThreads(current => (cursor ? [...current, ...page.threads] : page.threads))
      setNextCursor(page.nextCursor)
    } catch (error) {
      onNotice(`会话列表读取失败：${error instanceof Error ? error.message : '请稍后重试'}`)
    } finally {
      setLoading(false)
    }
  }, [onNotice, showArchived])

  useEffect(() => {
    void load()
  }, [load])

  // A finished run changes titles and recency, so the list is refreshed when the window regains focus —
  // cheap, and it keeps a second tab from showing a stale order.
  useEffect(() => {
    const refresh = () => void load()
    window.addEventListener('focus', refresh)
    return () => window.removeEventListener('focus', refresh)
  }, [load])

  const fail = (action: string) => (error: unknown) => {
    onNotice(`${action}失败：${error instanceof Error ? error.message : '请稍后重试'}`)
  }

  async function startNew() {
    try {
      const id = await createThread(null)
      await load()
      onOpen(id)
    } catch (error) {
      fail('新建会话')(error)
    }
  }

  async function rename(thread: ThreadSummary) {
    const title = draft.trim()
    setEditing(null)
    if (!title || title === thread.title) return
    try {
      await renameThread(thread.id, title)
      await load()
    } catch (error) {
      fail('重命名')(error)
    }
  }

  async function autoTitle(thread: ThreadSummary) {
    try {
      await generateTitle(thread.id)
      await load()
    } catch (error) {
      fail('生成标题')(error)
    }
  }

  async function toggleArchived(thread: ThreadSummary) {
    try {
      await setArchived(thread.id, !thread.archived)
      await load()
    } catch (error) {
      fail(thread.archived ? '取消归档' : '归档')(error)
    }
  }

  async function remove(thread: ThreadSummary) {
    // Deleting is irreversible and takes the attachments with it, so it asks first (§2.5).
    if (!window.confirm(`删除「${thread.title}」？该会话的消息、附件与恢复点都会一并删除，且无法撤销。`)) return
    try {
      await deleteThread(thread.id)
      if (activeId === thread.id) onOpen(null)
      await load()
    } catch (error) {
      fail('删除')(error)
    }
  }

  return (
    <nav className="agent-threads" aria-label="会话列表" data-testid="thread-list">
      <header className="agent-threads-head">
        <mdui-button variant="filled" icon="add" onClick={() => void startNew()}>新对话</mdui-button>
        <mdui-button
          variant="text"
          icon={showArchived ? 'unarchive' : 'archive'}
          onClick={() => setShowArchived(value => !value)}
        >
          {showArchived ? '进行中' : '已归档'}
        </mdui-button>
      </header>
      <ul className="agent-threads-items">
        {threads.map(thread => (
          <li key={thread.id} className={`agent-thread${thread.id === activeId ? ' active' : ''}`}>
            {editing === thread.id ? (
              <mdui-text-field
                autoFocus
                value={draft}
                onChange={event => setDraft(fieldValue(event))}
                onBlur={() => void rename(thread)}
                onKeyDown={event => {
                  if (event.key === 'Enter') void rename(thread)
                }}
              />
            ) : (
              // No timestamp in the row: the list is already ordered by recency, and the actions
              // overlay the row's end — on touch they are permanently opaque, so a date there is
              // invisible space the title pays for (measured: 56px reserved, title clipped to 115px).
              <button type="button" className="agent-thread-open" onClick={() => onOpen(thread.id)}>
                <mdui-icon name="forum" />
                <span className="agent-thread-title">{thread.title}</span>
              </button>
            )}
            <span className="agent-thread-actions">
              <mdui-icon name="edit" title="重命名" onClick={() => { setEditing(thread.id); setDraft(thread.title) }} />
              <mdui-icon name="auto_awesome" title="按首条消息生成标题" onClick={() => void autoTitle(thread)} />
              <mdui-icon
                name={thread.archived ? 'unarchive' : 'archive'}
                title={thread.archived ? '取消归档' : '归档'}
                onClick={() => void toggleArchived(thread)}
              />
              <mdui-icon name="delete" title="删除（不可撤销）" onClick={() => void remove(thread)} />
            </span>
          </li>
        ))}
        {!loading && threads.length === 0 && (
          <li className="agent-thread-empty">{showArchived ? '没有归档的会话' : '还没有会话，点「新对话」开始'}</li>
        )}
      </ul>
      {nextCursor && (
        <mdui-button variant="text" onClick={() => void load(nextCursor)}>加载更多</mdui-button>
      )}
    </nav>
  )
}
