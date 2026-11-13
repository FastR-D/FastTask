import type { ThreadRuntime } from '@assistant-ui/react'
import { useCallback, useRef, useSyncExternalStore } from 'react'
import { useComposerDraftUserId } from './ComposerDraft'
import { FAILED_MESSAGE_CHANGE_EVENT, readFailedMessages, removeFailedMessage } from './draft-storage'

const EMPTY: readonly string[] = []

/** A failed request must not replace the next message the person is composing. */
export function FailedMessageRecovery({ thread }: { thread: ThreadRuntime }) {
  const userId = useComposerDraftUserId()
  const owner = useRef(userId)
  owner.current = userId
  const subscribeThread = useCallback((listener: () => void) => thread.subscribe(listener), [thread])
  const currentThreadId = useCallback(() => {
    const state = thread.getState().state
    const id = state && typeof state === 'object' && 'threadId' in state ? state.threadId : undefined
    return typeof id === 'string' && !id.startsWith('__LOCALID_') ? id : undefined
  }, [thread])
  const threadId = useSyncExternalStore(subscribeThread, currentThreadId, currentThreadId)
  const subscribeMessages = useCallback((listener: () => void) => {
    window.addEventListener(FAILED_MESSAGE_CHANGE_EVENT, listener)
    return () => window.removeEventListener(FAILED_MESSAGE_CHANGE_EVENT, listener)
  }, [])
  const snapshot = useCallback(() => userId ? readFailedMessages(userId, threadId) : EMPTY, [userId, threadId])
  const messages = useSyncExternalStore(subscribeMessages, snapshot, snapshot)
  const subscribeComposer = useCallback((listener: () => void) => thread.composer.subscribe(listener), [thread])
  const composerText = useCallback(() => thread.composer.getState().text, [thread])
  const draft = useSyncExternalStore(subscribeComposer, composerText, composerText)
  if (!messages.length) return null

  function restore(text: string) {
    // Recheck live values, including clicks queued immediately before a rerender.
    if (!userId || owner.current !== userId || currentThreadId() !== threadId || thread.composer.getState().text) return
    thread.composer.setText(text)
    removeFailedMessage(userId, threadId, text)
  }

  return <details className="agent-failed-send" role="status">
    <summary>有 {messages.length} 条发送内容尚未确认，文本已保留</summary>
    <p className="meta">请先核对会话是否收到，避免重复发送。输入框中的新草稿会保留。</p>
    {messages.map(text => <div key={text}>
      <p style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{text}</p>
      <mdui-button variant="text" disabled={Boolean(draft)} onClick={() => restore(text)}>恢复到输入框</mdui-button>
    </div>)}
    {draft && <p className="meta">可先处理当前草稿，再恢复这段内容；也可以直接选择并复制上面的文本。</p>}
  </details>
}
