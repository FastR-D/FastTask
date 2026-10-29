import { assertAccessOwner, SESSION_OWNER_CHANGED } from '../session-owner'

// Check both the current UI owner and access subject after every asynchronous
// handoff. A grant belongs to the account that started the request.
export function assertHarnessOwner(expected: string, current: string | undefined, access: string | null): void {
  if (!expected || current !== expected) throw new Error(SESSION_OWNER_CHANGED)
  assertAccessOwner(access, expected)
}
