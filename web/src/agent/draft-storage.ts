// Composer text belongs to this tab's signed-in account and exact conversation.
// It carries neither credentials nor a serialized assistant runtime.
export const COMPOSER_DRAFT_PREFIX = 'fasttask.agent-draft.v1:'

export function composerDraftKey(userId: string, threadId?: string): string {
  const conversation = threadId === undefined ? 'new' : `thread:${encodeURIComponent(threadId)}`
  return `${COMPOSER_DRAFT_PREFIX}${encodeURIComponent(userId)}:${conversation}`
}

export function readComposerDraft(userId: string, threadId?: string): string | null {
  if (!userId) return null
  try { return sessionStorage.getItem(composerDraftKey(userId, threadId)) }
  catch { return null }
}

export function writeComposerDraft(userId: string, threadId: string | undefined, text: string): void {
  if (!userId) return
  try {
    const key = composerDraftKey(userId, threadId)
    if (text === '') sessionStorage.removeItem(key)
    else sessionStorage.setItem(key, text)
  } catch { /* Unavailable storage must not interrupt composing or sending. */ }
}

export function removeComposerDraft(userId: string, threadId?: string): void {
  if (!userId) return
  try { sessionStorage.removeItem(composerDraftKey(userId, threadId)) }
  catch { /* Storage unavailable. */ }
}

export function clearComposerDrafts(): void {
  try {
    const keys: string[] = []
    for (let index = 0; index < sessionStorage.length; index++) {
      const key = sessionStorage.key(index)
      if (key?.startsWith(COMPOSER_DRAFT_PREFIX)) keys.push(key)
    }
    for (const key of keys) sessionStorage.removeItem(key)
  } catch { /* Session cleanup remains available when storage is unavailable. */ }
}
