import { useEffect, useState } from 'react'
import { fieldValue } from './mdui-react'
import {
  announceThemeChange,
  isThemePreference,
  readThemePreference,
  setThemePreference,
  THEME_CHANGE_EVENT,
  type ThemePreference,
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
    <mdui-button-icon slot="trigger" icon={THEME_ICONS[preference]} aria-label={`主题：${label}`} title={`主题：${label}`}/>
    <mdui-menu aria-label="主题">
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
    label="主题"
    value={preference}
    onChange={event => {
      const next = fieldValue(event)
      if (isThemePreference(next) && next !== preference) choose(next)
    }}
  >
    {THEME_OPTIONS.map(value => <mdui-menu-item key={value} value={value}>{THEME_LABELS[value]}</mdui-menu-item>)}
  </mdui-select>
}
