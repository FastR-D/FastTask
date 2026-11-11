import { initColorMode } from '@fastrd/ui-foundation'
import { applyVisualTheme, syncTheme, THEME_STORAGE_KEY } from './theme'

// Bundled as a blocking classic script: the installed foundation controller exists before paint.
window.fastTaskColorMode = initColorMode({ storageKey: THEME_STORAGE_KEY })
applyVisualTheme()
window.fastTaskColorMode.subscribe(syncTheme)
