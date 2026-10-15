export const SESSION_OWNER_CHANGED = '登录账号已变化，原操作未发送。请回到原账号继续，或重新载入当前账号的工作台。'

// The unverified subject is only a client-side guard against accidental sends
// from an old screen. Authentication remains the server's responsibility.
export function assertAccessOwner(access: string | null, expectedUserId: string): asserts access is string {
  if (!access) throw new Error('authentication required')
  let subject: unknown
  try {
    const part = access.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')
    subject = JSON.parse(atob(part + '='.repeat((4 - part.length % 4) % 4))).sub
  } catch { /* Missing identity must fail closed for an account-bound request. */ }
  if (!expectedUserId || subject !== expectedUserId) throw new Error(SESSION_OWNER_CHANGED)
}
