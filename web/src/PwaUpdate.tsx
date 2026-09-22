import { useRegisterSW } from 'virtual:pwa-register/react'

// PwaUpdate registers the service worker (injectRegister is disabled in
// vite.config.ts) and surfaces the "new version available" prompt required by
// pwa.md §3.2: autoUpdate semantics with an EXPLICIT, user-triggered refresh —
// never a silent force-reload, because the user may be mid-conversation.
export function PwaUpdate() {
  const {
    needRefresh: [needRefresh, setNeedRefresh],
    updateServiceWorker,
  } = useRegisterSW()
  if (!needRefresh) return null
  return (
    <div className="pwa-update" role="status">
      <mdui-icon name="system_update" />
      <span>有新版本可用</span>
      <mdui-button variant="filled" onClick={() => { void updateServiceWorker(true) }}>点击刷新</mdui-button>
      <mdui-button-icon icon="close" aria-label="稍后再说" onClick={() => setNeedRefresh(false)} />
    </div>
  )
}
