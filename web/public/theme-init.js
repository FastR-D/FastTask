// Apply the saved choice before the first paint so a pinned light theme never flashes dark.
// Keep the key, class names and colours in sync with src/theme.ts.
;(() => {
  try {
    let stored
    try { stored = localStorage.getItem('fasttask.theme') } catch {}
    const preference = stored === 'light' || stored === 'dark' ? stored : 'system'
    const dark = preference === 'dark' ||
      (preference === 'system' && window.matchMedia?.('(prefers-color-scheme: dark)').matches)
    const root = document.documentElement
    root.classList.remove('mdui-theme-auto', 'mdui-theme-light', 'mdui-theme-dark')
    root.classList.add(preference === 'system' ? 'mdui-theme-auto' : `mdui-theme-${preference}`)
    const metas = [...document.querySelectorAll('meta[name="theme-color"]')]
    const target = metas.shift() || document.head.appendChild(Object.assign(document.createElement('meta'), { name: 'theme-color' }))
    target.removeAttribute('media')
    target.setAttribute('content', dark ? '#10161e' : '#f7f8fa')
    metas.forEach(meta => meta.remove())
  } catch {
    // Storage or matchMedia unavailable: the default mdui-theme-auto class still follows the system.
  }
})()
