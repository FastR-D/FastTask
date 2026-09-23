import type { HarnessMode } from '../harness'

// The user's choice of host (doc/harness.md §3.2, §15).
//
// This lives in the agent layer, NOT in web/src/harness/: §3.1 rule 2 says the host persists nothing, and
// harness/boundary.test.ts enforces that against every file in that directory. The rule is right — a probe
// answer written to storage would outlive the browser upgrade that invalidates it — and a user's setting is
// a different thing that happens to need the same medium. So the harness takes a forced mode as a
// parameter, and the setting that produces it is kept here.
//
// §3.2 forbids caching the PROBE result: a browser upgrade changes what the machine can do, and a stored
// answer would keep being wrong. A preference is the other thing entirely — an explicit instruction from
// the person using the app — so it is stored, and it survives a reload for the same reason a setting does.
//
// 'auto' means "probe, and show me what you picked", which is the default and what §15 records.

export type ModePreference = 'auto' | HarnessMode

const STORAGE_KEY = 'fasttask.harness.mode'

const VALUES: ModePreference[] = ['auto', 'wasm', 'sidecar']

/** readModePreference returns the stored choice, treating anything unrecognizable as 'auto'. A value this
 *  module did not write is not a preference, and guessing from it would pin a user to a host they never
 *  asked for. */
export function readModePreference(storage: Storage | null = safeStorage()): ModePreference {
  try {
    const stored = storage?.getItem(STORAGE_KEY)
    return VALUES.includes(stored as ModePreference) ? (stored as ModePreference) : 'auto'
  } catch {
    return 'auto'
  }
}

/** writeModePreference stores the choice. 'auto' removes the key, so "no preference" is represented by
 *  the absence of a setting rather than by a value that has to be kept in sync with the default. */
export function writeModePreference(value: ModePreference, storage: Storage | null = safeStorage()): void {
  if (!storage) return
  try {
    if (value === 'auto') storage.removeItem(STORAGE_KEY)
    else if (VALUES.includes(value)) storage.setItem(STORAGE_KEY, value)
  } catch {
    // A refused write costs the user their preference on the next load. That is a smaller harm than
    // failing the send path that called it, so the choice still applies to this session.
  }
}

/** forcedModeOf translates a preference into what selectMode and driveRun take: null means probe (§3.2). */
export function forcedModeOf(preference: ModePreference): HarnessMode | null {
  return preference === 'wasm' || preference === 'sidecar' ? preference : null
}

/** safeStorage is localStorage when it is usable. A browser configured to block storage, or a privacy mode
 *  that throws on access, must not stop the agent from working: the preference is a convenience, and
 *  without it the probe still picks a host. */
function safeStorage(): Storage | null {
  try {
    const storage = globalThis.localStorage
    storage?.getItem(STORAGE_KEY)
    return storage ?? null
  } catch {
    return null
  }
}
