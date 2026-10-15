import { useCallback, useEffect, useRef, useState } from 'react'
import { fieldValue } from '../mdui-react'
import { ApiError } from '../api'
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

// The row actions are bare icons so they can sit over the row's end without a button's 40px box. They
// still have to be reachable and operable from the keyboard: the CSS already reveals them on
// focus-within, which only helps if they can take focus.
function actionProps(label: string, run: () => void) {
  return {
    title: label,
    'aria-label': label,
    role: 'button',
    tabIndex: 0,
    onClick: run,
    onKeyDown: (event: { key: string; preventDefault: () => void }) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault()
        run()
      }
    },
  }
}

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
  const [loadingMore,setLoadingMore]=useState(false)
  const [loadError,setLoadError]=useState('')
  const scope=useRef<'regular'|'archived'>('regular')
  const loadSequence=useRef(0)
  const loadingRef=useRef(false)
  const readsInFlight=useRef(0)
  const readFailed=useRef(false)
  const notifiedError=useRef('')
  const cursorRef=useRef<string|null>(null)
  const consumedCursors=useRef(new Set<string>())
  const retryCursor=useRef<string|undefined>(undefined)
  // On a phone the list is a disclosure above the conversation (agent.css): closed by default and closed
  // again once a conversation is picked, so the thread — not the index — gets the screen.
  const [open, setOpen] = useState(false)
  const openThread = (threadId: string | null) => { setOpen(false); onOpen(threadId) }
  const activeTitle = threads.find(thread => thread.id === activeId)?.title

  const load = useCallback(async (cursor?: string) => {
    // A page can append only to its current catalogue, once. Refreshes replace
    // the snapshot and invalidate any older page before its response arrives.
    if(cursor&&(loadingRef.current||cursorRef.current!==cursor||consumedCursors.current.has(cursor)))return
    const selected=scope.current,sequence=++loadSequence.current
    if(!cursor)consumedCursors.current.clear()
    loadingRef.current=true;readsInFlight.current++;setLoading(true);setLoadingMore(Boolean(cursor))
    try {
      const page = await listThreads(cursor, selected)
      if(sequence!==loadSequence.current||selected!==scope.current)return
      readFailed.current=false;notifiedError.current='';setLoadError('')
      setThreads(current => [...new Map([...(cursor?current:[]),...page.threads].map(thread=>[thread.id,thread])).values()])
      if(cursor)consumedCursors.current.add(cursor)
      const next=page.nextCursor&&!consumedCursors.current.has(page.nextCursor)?page.nextCursor:null
      cursorRef.current=next;setNextCursor(next)
    } catch (error) {
      if(sequence!==loadSequence.current||selected!==scope.current)return
      const detail=error instanceof ApiError&&error.status===0?'暂时无法连接FastTask服务':error instanceof Error?error.message:'请稍后重试'
      const message=`会话列表读取失败：${detail}`
      readFailed.current=true;retryCursor.current=cursor;setLoadError(message)
      if(notifiedError.current!==message){notifiedError.current=message;onNotice(message)}
    } finally {
      readsInFlight.current--
      if(sequence===loadSequence.current){loadingRef.current=false;setLoading(false);setLoadingMore(false)}
    }
  }, [onNotice])

  useEffect(() => {
    void load()
    return()=>{loadSequence.current++}
  }, [load])

  function switchArchived(){
    const next=scope.current==='regular'?'archived':'regular'
    scope.current=next;loadSequence.current++;cursorRef.current=null;consumedCursors.current.clear()
    readFailed.current=false
    loadingRef.current=true;setThreads([]);setNextCursor(null);setEditing(null);setDraft('');setLoadError('');setLoading(true);setLoadingMore(false);setShowArchived(next==='archived')
    void load()
  }

  // Messages change fallback titles and recency, so refresh when the window regains focus —
  // cheap, and it keeps a second tab from showing a stale order.
  useEffect(() => {
    const refresh = () => void load()
    const reconnect = () => {
      // Connectivity is only a retry signal. The local service may be reachable
      // without internet, and only this failed readonly request is retried.
      if(readFailed.current&&!loadingRef.current&&readsInFlight.current===0)void load(retryCursor.current)
    }
    window.addEventListener('focus', refresh)
    window.addEventListener('online', reconnect)
    return () => {window.removeEventListener('focus', refresh);window.removeEventListener('online', reconnect)}
  }, [load])

  const fail = (action: string) => (error: unknown) => {
    onNotice(`${action}失败：${error instanceof Error ? error.message : '请稍后重试'}`)
  }

  async function startNew() {
    try {
      const id = await createThread(null)
      await load()
      openThread(id)
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
    <nav className={`agent-threads${open ? ' open' : ''}`} aria-label="会话列表" aria-busy={loading} data-testid="thread-list">
      <header className="agent-threads-head">
        <mdui-button variant="filled" icon="add" onClick={() => void startNew()}>新对话</mdui-button>
        <mdui-button
          variant="text"
          className="agent-threads-toggle"
          end-icon={open ? 'expand_less' : 'expand_more'}
          aria-expanded={open}
          onClick={() => setOpen(value => !value)}
        >
          <span className="agent-threads-toggle-label">{activeTitle ?? '全部会话'}</span>
        </mdui-button>
        <mdui-button
          variant="text"
          className="agent-threads-archived"
          icon={showArchived ? 'unarchive' : 'archive'}
          onClick={switchArchived}
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
                onInput={event => setDraft(fieldValue(event))}
                onBlur={() => void rename(thread)}
                onKeyDown={event => {
                  if (event.key === 'Enter') void rename(thread)
                }}
              />
            ) : (
              // No timestamp in the row: the list is already ordered by recency, and the actions
              // overlay the row's end — on touch they are permanently opaque, so a date there is
              // invisible space the title pays for (measured: 56px reserved, title clipped to 115px).
              <button type="button" className="agent-thread-open" onClick={() => openThread(thread.id)}>
                <mdui-icon name="forum" />
                <span className="agent-thread-title">{thread.title}</span>
              </button>
            )}
            <span className="agent-thread-actions">
              <mdui-icon name="edit" {...actionProps('重命名', () => { setEditing(thread.id); setDraft(thread.title) })} />
              <mdui-icon name="auto_awesome" {...actionProps('按首条消息生成标题', () => void autoTitle(thread))} />
              <mdui-icon
                name={thread.archived ? 'unarchive' : 'archive'}
                {...actionProps(thread.archived ? '取消归档' : '归档', () => void toggleArchived(thread))}
              />
              <mdui-icon name="delete" {...actionProps('删除（不可撤销）', () => void remove(thread))} />
            </span>
          </li>
        ))}
        {loading&&<li className="agent-thread-empty" role="status">{loadingMore?'正在加载更多会话…':threads.length?'正在更新会话…':'正在读取会话…'}</li>}
        {!loading && !loadError && threads.length === 0 && (
          <li className="agent-thread-empty">{showArchived ? '没有归档的会话' : '还没有会话，点「新对话」开始'}</li>
        )}
      </ul>
      {loadError&&<div role="alert"><p className="agent-thread-empty">{loadError}</p><mdui-button variant="text" disabled={loading} onClick={()=>void load(retryCursor.current)}>重试读取</mdui-button></div>}
      {nextCursor && (
        <mdui-button variant="text" disabled={loading||Boolean(loadError)} loading={loading&&loadingMore} onClick={() => void load(nextCursor)}>加载更多</mdui-button>
      )}
    </nav>
  )
}
