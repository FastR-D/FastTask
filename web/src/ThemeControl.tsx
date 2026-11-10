import { useEffect, useState } from 'react'
import { fieldValue } from './mdui-react'
import {
  announceThemeChange,
  announceVisualThemeChange,
  isVisualTheme,
  isThemePreference,
  readThemePreference,
  readVisualTheme,
  setThemePreference,
  setVisualTheme,
  THEME_CHANGE_EVENT,
  VISUAL_THEME_CHANGE_EVENT,
  type ThemePreference,
  type VisualTheme,
} from './theme'

export const THEME_LABELS: Record<ThemePreference, string> = {
  system: '跟随系统',
  light: '浅色',
  dark: '深色',
}

const THEME_ICONS: Record<ThemePreference, string> = {
  system: 'brightness_auto',
  light: 'light_mode',
  dark: 'dark_mode',
}

const THEME_OPTIONS = (['system', 'light', 'dark'] as ThemePreference[])
const VISUAL_THEME_OPTIONS = (['workbench', 'classic'] as VisualTheme[])

export const VISUAL_THEME_LABELS: Record<VisualTheme, string> = {
  workbench: '工作台',
  classic: '经典',
}

const VISUAL_THEME_ICONS: Record<VisualTheme, string> = {
  workbench: 'dashboard',
  classic: 'view_quilt',
}

// The rail and the drawer keep a control each, and both stay mounted across breakpoints, so the
// preference lives in a small shared event rather than in one parent's state: whichever control the
// person used, the other one updates with it.
function useThemePreference(): [ThemePreference, (value: ThemePreference) => void] {
  const [preference, setPreference] = useState<ThemePreference>(() => readThemePreference())
  useEffect(() => {
    const sync = (event: Event) => {
      const detail = (event as CustomEvent<unknown>).detail
      setPreference(isThemePreference(detail) ? detail : readThemePreference())
    }
    window.addEventListener(THEME_CHANGE_EVENT, sync)
    return () => window.removeEventListener(THEME_CHANGE_EVENT, sync)
  }, [])
  function choose(value: ThemePreference) {
    setThemePreference(value)
    setPreference(value)
    announceThemeChange(value)
  }
  return [preference, choose]
}

/** Desktop control: an icon in the navigation rail that opens the three choices. */
export function ThemeMenu() {
  const [preference, choose] = useThemePreference()
  const label = THEME_LABELS[preference]
  return <mdui-dropdown className="theme-menu" placement="right-end">
    <mdui-button-icon slot="trigger" icon={THEME_ICONS[preference]} aria-label={`颜色模式：${label}`} title={`颜色模式：${label}`}/>
    <mdui-menu aria-label="颜色模式">
      {THEME_OPTIONS.map(value => <mdui-menu-item key={value} value={value} role="menuitemradio" aria-checked={preference === value} onClick={() => choose(value)}>
        <mdui-icon slot="icon" name={preference === value ? 'check' : undefined}/>{THEME_LABELS[value]}
      </mdui-menu-item>)}
    </mdui-menu>
  </mdui-dropdown>
}

/** Drawer control: the mobile navigation has room for a labelled field, so the current choice is
 *  readable without opening anything. */
export function ThemeSelect() {
  const [preference, choose] = useThemePreference()
  return <mdui-select
    className="theme-select"
    variant="outlined"
    label="颜色模式"
    value={preference}
    onChange={event => {
      const next = fieldValue(event)
      if (isThemePreference(next) && next !== preference) choose(next)
    }}
  >
    {THEME_OPTIONS.map(value => <mdui-menu-item key={value} value={value}>{THEME_LABELS[value]}</mdui-menu-item>)}
  </mdui-select>
}

function useVisualTheme(): [VisualTheme, (value: VisualTheme) => void] {
  const [theme, setTheme] = useState<VisualTheme>(() => readVisualTheme())
  useEffect(() => {
    const sync = (event: Event) => {
      const detail = (event as CustomEvent<unknown>).detail
      setTheme(isVisualTheme(detail) ? detail : readVisualTheme())
    }
    window.addEventListener(VISUAL_THEME_CHANGE_EVENT, sync)
    return () => window.removeEventListener(VISUAL_THEME_CHANGE_EVENT, sync)
  }, [])
  function choose(value: VisualTheme) {
    setVisualTheme(value)
    setTheme(value)
    announceVisualThemeChange(value)
  }
  return [theme, choose]
}

/** Desktop control for switching the visual language independently from light/dark mode. */
export function VisualThemeMenu() {
  const [theme, choose] = useVisualTheme()
  const label = VISUAL_THEME_LABELS[theme]
  return <mdui-dropdown className="visual-theme-menu" placement="right-end">
    <mdui-button-icon slot="trigger" icon={VISUAL_THEME_ICONS[theme]} aria-label={`界面主题：${label}`} title={`界面主题：${label}`}/>
    <mdui-menu aria-label="界面主题">
      {VISUAL_THEME_OPTIONS.map(value => <mdui-menu-item key={value} value={value} role="menuitemradio" aria-checked={theme === value} onClick={() => choose(value)}>
        <mdui-icon slot="icon" name={theme === value ? 'check' : undefined}/>{VISUAL_THEME_LABELS[value]}
      </mdui-menu-item>)}
    </mdui-menu>
  </mdui-dropdown>
}

/** Drawer control for switching the visual language independently from light/dark mode. */
export function VisualThemeSelect() {
  const [theme, choose] = useVisualTheme()
  return <mdui-select
    className="visual-theme-select"
    variant="outlined"
    label="界面主题"
    value={theme}
    onChange={event => {
      const next = fieldValue(event)
      if (isVisualTheme(next) && next !== theme) choose(next)
    }}
  >
    {VISUAL_THEME_OPTIONS.map(value => <mdui-menu-item key={value} value={value}>{VISUAL_THEME_LABELS[value]}</mdui-menu-item>)}
  </mdui-select>
}
