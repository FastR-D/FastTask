import type { initColorMode } from '@fastrd/ui-foundation'

export type ThemePreference = 'system' | 'light' | 'dark'
export type ResolvedTheme = 'light' | 'dark'
export type VisualTheme = 'workbench' | 'classic'
export const THEME_STORAGE_KEY = 'fasttask.theme'
export const VISUAL_THEME_STORAGE_KEY = 'fasttask.visual-theme'
export const THEME_CHANGE_EVENT = 'fasttask:theme'
export const VISUAL_THEME_CHANGE_EVENT = 'fasttask:visual-theme'

declare global {
  interface Window { fastTaskColorMode: ReturnType<typeof initColorMode> }
}

export function isThemePreference(value: unknown): value is ThemePreference {
  return value === 'light' || value === 'dark' || value === 'system'
}
export function isVisualTheme(value: unknown): value is VisualTheme {
  return value === 'workbench' || value === 'classic'
}
export function readThemePreference(): ThemePreference { return window.fastTaskColorMode.mode }
export function readVisualTheme(storage: Storage | null = safeStorage()): VisualTheme {
  try {
    const value = storage?.getItem(VISUAL_THEME_STORAGE_KEY)
    return isVisualTheme(value) ? value : 'workbench'
  } catch { return 'workbench' }
}
export function writeVisualTheme(value: VisualTheme, storage: Storage | null = safeStorage()): void {
  if (!isVisualTheme(value)) return
  try { storage?.setItem(VISUAL_THEME_STORAGE_KEY, value) } catch { /* Session-only preference. */ }
}
export function applyVisualTheme(value: VisualTheme = readVisualTheme(), doc: Document = document): VisualTheme {
  doc.documentElement.dataset.ftTheme = value
  return value
}
export function setVisualTheme(value: VisualTheme, doc: Document = document, storage: Storage | null = safeStorage()): VisualTheme {
  writeVisualTheme(value, storage)
  return applyVisualTheme(value, doc)
}

// MDUI classes and PWA chrome are host integration; foundation owns resolution and listeners.
export function syncTheme(): void {
  const colorMode = window.fastTaskColorMode
  const root = document.documentElement
  root.classList.remove('mdui-theme-auto', 'mdui-theme-light', 'mdui-theme-dark')
  root.classList.add(colorMode.mode === 'system' ? 'mdui-theme-auto' : `mdui-theme-${colorMode.resolved}`)
  root.style.colorScheme = colorMode.resolved
  const metas = [...document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')]
  const initial = metas.find(meta => meta.media.includes(colorMode.resolved))?.content
  const target = metas.shift() ?? document.head.appendChild(Object.assign(document.createElement('meta'), { name: 'theme-color' }))
  target.removeAttribute('media')
  target.content = getComputedStyle(root).getPropertyValue('--fastui-page').trim() || initial || target.content
  for (const extra of metas) extra.remove()
  announceThemeChange(colorMode.mode)
}

export function setThemePreference(value: ThemePreference): ResolvedTheme {
  const colorMode = window.fastTaskColorMode
  colorMode.setMode(value)
  return colorMode.resolved
}
export function announceThemeChange(value: ThemePreference): void {
  window.dispatchEvent(new CustomEvent(THEME_CHANGE_EVENT, { detail: value }))
}
export function announceVisualThemeChange(value: VisualTheme): void {
  window.dispatchEvent(new CustomEvent(VISUAL_THEME_CHANGE_EVENT, { detail: value }))
}
export function watchTheme(): () => void {
  syncTheme()
  applyVisualTheme()
  const onStorage = (event: StorageEvent) => {
    if (event.key !== null && event.key !== VISUAL_THEME_STORAGE_KEY) return
    announceVisualThemeChange(applyVisualTheme())
  }
  window.addEventListener('storage', onStorage)
  return () => window.removeEventListener('storage', onStorage)
}
function safeStorage(): Storage | null {
  try { return localStorage } catch { return null }
}
