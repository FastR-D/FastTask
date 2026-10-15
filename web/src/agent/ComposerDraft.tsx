import type { ThreadRuntime } from '@assistant-ui/react'
import { createContext, useCallback, useContext, useEffect, useRef, useSyncExternalStore, type ReactNode } from 'react'
import { readComposerDraft, removeComposerDraft, writeComposerDraft } from './draft-storage'

const DraftUserContext = createContext<string | undefined>(undefined)

// The account ID comes from /me through Workspace, never a model, token claim
// or local assistant-ui thread ID. Updating it does not remount the runtime.
export function ComposerDraftUserProvider({ userId, children }: { userId?: string; children: ReactNode }) {
  return <DraftUserContext.Provider value={userId || undefined}>{children}</DraftUserContext.Provider>
}

export function useComposerDraftUserId() { return useContext(DraftUserContext) }

// Only composer text is retained in this tab. Attachment objects, pending
// commands and credentials are intentionally outside this persistence boundary.
export function ComposerDraftPersistence({ thread }: { thread: ThreadRuntime }) {
  const userId = useContext(DraftUserContext)
  const composer = thread.composer
  const subscribe = useCallback((listener: () => void) => thread.subscribe(listener), [thread])
  const snapshot = useCallback(() => {
    // The remote runtime briefly exposes an inert thread while initializing.
    // Its null state is not a new conversation and must not receive a draft.
    const state = thread.getState().state
    if (!state || typeof state !== 'object' || Array.isArray(state)) return null
    const id = 'threadId' in state ? state.threadId : undefined
    return typeof id === 'string' && id && !id.startsWith('__LOCALID_') ? id : ''
  }, [thread])
  const scope = useSyncExternalStore(subscribe, snapshot, snapshot)
  const threadId = scope || undefined
  const previous = useRef<{ userId?: string; threadId?: string } | null>(null)

  useEffect(() => {
    if (scope === null) return
    const before = previous.current
    previous.current = { userId, threadId }
    if (!userId) {
      if (before?.userId) composer.setText('')
      return
    }
    const current = composer.getState().text
    const ownerChanged = Boolean(before?.userId && before.userId !== userId)
    const newThreadCreated = before?.userId === userId && !before.threadId && Boolean(threadId)
    if (newThreadCreated) {
      // A first send establishes the server thread. Move any *next* unsent
      // text typed while it was starting, rather than restoring the sent text.
      writeComposerDraft(userId, threadId, current)
      removeComposerDraft(userId)
    } else {
      const saved = readComposerDraft(userId, threadId)
      if (before && !before.userId && current) writeComposerDraft(userId, threadId, current)
      else if (saved !== null) composer.setText(saved)
      else if (ownerChanged || (before?.threadId && before.threadId !== threadId)) composer.setText('')
      else if (current) writeComposerDraft(userId, threadId, current)
    }
    let lastText = composer.getState().text
    return composer.subscribe(() => {
      const text = composer.getState().text
      if (text === lastText) return
      lastText = text
      writeComposerDraft(userId, threadId, text)
    })
    // Each edit is saved synchronously. Cleanup only unsubscribes, so logging
    // out cannot restore text after the session-end handler has cleared it.
  }, [composer, userId, threadId, scope])
  return null
}
