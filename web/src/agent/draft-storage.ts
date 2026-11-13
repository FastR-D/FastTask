// Composer text belongs to this tab's signed-in account and exact conversation.
// It carries neither credentials nor a serialized assistant runtime.
export const COMPOSER_DRAFT_PREFIX = 'fasttask.agent-draft.v1:'
export const FAILED_MESSAGE_CHANGE_EVENT = 'fasttask:failed-message'
const failedMessages = new Map<string, readonly string[]>()

function failedMessageKey(userId: string, threadId?: string): string {
  return `${composerDraftKey(userId, threadId)}:failed-messages`
}

export function readFailedMessages(userId: string, threadId?: string): readonly string[] {
  if (!userId) return []
  const key = failedMessageKey(userId, threadId)
  const cached = failedMessages.get(key)
  if (cached) return cached
  let values: string[] = []
  try {
    const parsed: unknown = JSON.parse(sessionStorage.getItem(key) || '[]')
    if (Array.isArray(parsed)) values = parsed.filter((value): value is string => typeof value === 'string' && Boolean(value))
  } catch { /* A blocked store still permits in-memory recovery in this tab. */ }
  failedMessages.set(key, values)
  return values
}

function announceFailedMessages(): void {
  if (typeof window !== 'undefined') window.dispatchEvent(new Event(FAILED_MESSAGE_CHANGE_EVENT))
}

function writeFailedMessages(userId: string, threadId: string | undefined, values: readonly string[]): void {
  const key = failedMessageKey(userId, threadId)
  failedMessages.set(key, values)
  try {
    if (values.length) sessionStorage.setItem(key, JSON.stringify(values))
    else sessionStorage.removeItem(key)
  } catch { /* Keep the text in this tab when storage is unavailable. */ }
  announceFailedMessages()
}

export function preserveFailedMessage(userId: string, threadId: string | undefined, text: string): void {
  if (!userId || !text) return
  const values = readFailedMessages(userId, threadId)
  if (!values.includes(text)) writeFailedMessages(userId, threadId, [...values, text])
}

export function removeFailedMessage(userId: string, threadId: string | undefined, text: string): void {
  if (!userId) return
  const values = readFailedMessages(userId, threadId)
  if (values.includes(text)) writeFailedMessages(userId, threadId, values.filter(value => value !== text))
}

export function moveFailedMessages(userId: string, threadId: string): void {
  const pending = readFailedMessages(userId)
  if (!pending.length) return
  writeFailedMessages(userId, threadId, [...new Set([...readFailedMessages(userId, threadId), ...pending])])
  writeFailedMessages(userId, undefined, [])
}

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
  failedMessages.clear()
  try {
    const keys: string[] = []
    for (let index = 0; index < sessionStorage.length; index++) {
      const key = sessionStorage.key(index)
      if (key?.startsWith(COMPOSER_DRAFT_PREFIX)) keys.push(key)
    }
    for (const key of keys) sessionStorage.removeItem(key)
  } catch { /* Session cleanup remains available when storage is unavailable. */ }
  announceFailedMessages()
}
