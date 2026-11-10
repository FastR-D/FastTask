// Theme preference for the web client.
//
// The app follows the operating system by default. A pin to light or dark is expressed the way
// MDUI expects it: a class on <html> that rebinds its semantic colour tokens. The boot script in
// index.html applies the same rule before React mounts, so a pinned light theme never starts with a
// dark first paint. Keep the storage key, the class names and the palette colours in sync with it.

export type ThemePreference = 'system' | 'light' | 'dark'
export type ResolvedTheme = 'light' | 'dark'
export type VisualTheme = 'workbench' | 'classic'

export const THEME_STORAGE_KEY = 'fasttask.theme'
export const VISUAL_THEME_STORAGE_KEY = 'fasttask.visual-theme'

// ThemeControl listens for this event so the rail and the drawer controls cannot disagree after one
// of them changes the setting. It also carries the new value to every mounted control.
export const THEME_CHANGE_EVENT = 'fasttask:theme'
export const VISUAL_THEME_CHANGE_EVENT = 'fasttask:visual-theme'

const PREFERENCES: readonly ThemePreference[] = ['system', 'light', 'dark']
const VISUAL_THEMES: readonly VisualTheme[] = ['workbench', 'classic']
const THEME_CLASSES = ['mdui-theme-auto', 'mdui-theme-light', 'mdui-theme-dark']

// Background colours of the two palettes in styles.css. The browser paints its address bar and the
// installed app's status bar with this value, so it has to follow the resolved theme, not the stored
// preference: "system + dark OS" is a dark chrome and a dark page.
export const THEME_COLORS: Record<ResolvedTheme, string> = { light: '#f7f8fa', dark: '#10161e' }

/** isThemePreference narrows a value that came out of storage or an event payload. */
export function isThemePreference(value: unknown): value is ThemePreference {
  return PREFERENCES.includes(value as ThemePreference)
}

/** isVisualTheme narrows a persisted or event-provided visual theme. */
export function isVisualTheme(value: unknown): value is VisualTheme {
  return VISUAL_THEMES.includes(value as VisualTheme)
}

/** The new workbench visual language is the default. Classic remains available as an explicit choice. */
export function readVisualTheme(storage: Storage | null = safeStorage()): VisualTheme {
  try {
    const stored = storage?.getItem(VISUAL_THEME_STORAGE_KEY)
    return isVisualTheme(stored) ? stored : 'workbench'
  } catch {
    return 'workbench'
  }
}

export function writeVisualTheme(value: VisualTheme, storage: Storage | null = safeStorage()): void {
  if (!storage || !isVisualTheme(value)) return
  try { storage.setItem(VISUAL_THEME_STORAGE_KEY, value) } catch {
    // The theme still applies for this session when storage is blocked.
  }
}

/** readThemePreference treats a missing or unrecognizable value as "follow the system". A value this
 *  module did not write is not a preference. */
export function readThemePreference(storage: Storage | null = safeStorage()): ThemePreference {
  try {
    const stored = storage?.getItem(THEME_STORAGE_KEY)
    return isThemePreference(stored) ? stored : 'system'
  } catch {
    return 'system'
  }
}

/** writeThemePreference stores the choice. "system" removes the key so the default is represented by
 *  the absence of a setting, matching the harness mode preference. */
export function writeThemePreference(value: ThemePreference, storage: Storage | null = safeStorage()): void {
  if (!storage) return
  try {
    if (value === 'system') storage.removeItem(THEME_STORAGE_KEY)
    else if (isThemePreference(value)) storage.setItem(THEME_STORAGE_KEY, value)
  } catch {
    // Blocked storage costs the choice on the next load. The theme still applies to this session.
  }
}

/** prefersDarkTheme reads the operating system preference. A missing matchMedia is treated as light,
 *  which is also what MDUI's default palette assumes. */
export function prefersDarkTheme(doc: Document | undefined = globalThis.document): boolean {
  return Boolean(doc?.defaultView?.matchMedia?.('(prefers-color-scheme: dark)').matches)
}

export function resolveTheme(preference: ThemePreference, prefersDark: boolean): ResolvedTheme {
  return preference === 'system' ? (prefersDark ? 'dark' : 'light') : preference
}

/** applyTheme sets the MDUI theme class and the browser chrome colour, and returns the palette that
 *  is actually on screen. */
export function applyTheme(
  preference: ThemePreference = readThemePreference(),
  doc: Document | undefined = globalThis.document,
  prefersDark: boolean = prefersDarkTheme(doc),
): ResolvedTheme {
  const resolved = resolveTheme(preference, prefersDark)
  if (!doc) return resolved
  const root = doc.documentElement
  root.classList.remove(...THEME_CLASSES)
  root.classList.add(preference === 'system' ? 'mdui-theme-auto' : `mdui-theme-${preference}`)
  applyThemeColor(doc, resolved)
  return resolved
}

/** applyVisualTheme changes only the visual language, leaving light/dark mode untouched. */
export function applyVisualTheme(
  visualTheme: VisualTheme = readVisualTheme(),
  doc: Document | undefined = globalThis.document,
): VisualTheme {
  if (!doc) return visualTheme
  doc.documentElement.dataset.ftTheme = visualTheme
  return visualTheme
}

/** setThemePreference persists the choice and applies it immediately. */
export function setThemePreference(
  value: ThemePreference,
  doc: Document | undefined = globalThis.document,
  storage: Storage | null = safeStorage(),
): ResolvedTheme {
  writeThemePreference(value, storage)
  return applyTheme(value, doc)
}

export function setVisualTheme(
  value: VisualTheme,
  doc: Document | undefined = globalThis.document,
  storage: Storage | null = safeStorage(),
): VisualTheme {
  writeVisualTheme(value, storage)
  return applyVisualTheme(value, doc)
}

/** announceThemeChange tells the other mounted controls about a change made by one of them. */
export function announceThemeChange(value: ThemePreference): void {
  if (typeof window === 'undefined') return
  window.dispatchEvent(new CustomEvent<ThemePreference>(THEME_CHANGE_EVENT, { detail: value }))
}

export function announceVisualThemeChange(value: VisualTheme): void {
  if (typeof window === 'undefined') return
  window.dispatchEvent(new CustomEvent<VisualTheme>(VISUAL_THEME_CHANGE_EVENT, { detail: value }))
}

/** watchTheme keeps the browser chrome in step with the system while the preference is "system",
 *  and picks up a choice made in another tab. Returns the cleanup for the effect that owns it. */
export function watchTheme(): () => void {
  if (typeof window === 'undefined') return () => undefined
  applyTheme()
  applyVisualTheme()
  const media = window.matchMedia?.('(prefers-color-scheme: dark)')
  const onSystemChange = () => {
    // The applied class also works when storage is blocked and the choice only lasts this session.
    if (!document.documentElement.classList.contains('mdui-theme-auto')) return
    applyTheme('system')
    announceThemeChange('system')
  }
  const onStorage = (event: StorageEvent) => {
    if (event.key !== null && event.key !== THEME_STORAGE_KEY && event.key !== VISUAL_THEME_STORAGE_KEY) return
    if (event.key === null || event.key === VISUAL_THEME_STORAGE_KEY) {
      const visualTheme = readVisualTheme()
      applyVisualTheme(visualTheme)
      announceVisualThemeChange(visualTheme)
      if (event.key !== null) return
    }
    const preference = readThemePreference()
    applyTheme(preference)
    announceThemeChange(preference)
  }
  media?.addEventListener?.('change', onSystemChange)
  window.addEventListener('storage', onStorage)
  return () => {
    media?.removeEventListener?.('change', onSystemChange)
    window.removeEventListener('storage', onStorage)
  }
}

function applyThemeColor(doc: Document, resolved: ResolvedTheme): void {
  const color = THEME_COLORS[resolved]
  const metas = Array.from(doc.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]'))
  const target = metas.shift() ?? doc.createElement('meta')
  if (!target.parentNode) {
    target.setAttribute('name', 'theme-color')
    doc.head.appendChild(target)
  }
  target.removeAttribute('media')
  target.setAttribute('content', color)
  // index.html ships one meta per system palette so the no-script fallback stays honest. Once the
  // resolved theme is known, a single meta is the only one that can be correct.
  for (const extra of metas) extra.remove()
}

function safeStorage(): Storage | null {
  try {
    const storage = globalThis.localStorage
    storage?.getItem(THEME_STORAGE_KEY)
    return storage ?? null
  } catch {
    return null
  }
}
