import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { ApiError, idem, request, token } from './api'
import { localDateInTimezone, timezoneOrDefault } from './date'
import { assertAccessOwner } from './session-owner'
import { fieldValue } from './mdui-react'
import type { PlanItem, ScheduledDay } from './types'
import { getBluetoothCapability, WolinkPanelClient, type SearchProgress, type TransferProgress } from './panel/bluetooth'
import { DISPLAY_HEIGHT, DISPLAY_WIDTH } from './panel/protocol'
import { drawBadge, EMPTY_PROFILE, paginateBadge, prepareAvatar, type PanelProfile, type PanelWeather } from './panel/badge'
import { isLoopbackPanelPage, localNetworkPermission, nativeConfigMatchesOrigin, NativePanelError, nativePanel, nativeStartCommand, type NativeDevice, type NativeHealth } from './panel/native'
import { matchingPanelPairing, type PanelPairing } from './panel/pairing'
import { loadPanelDeviceHistory, panelDeviceKey, rememberPanelDevice, type RememberedPanelDevice } from './panel/history'
import './panel.css'

type Props = { active: boolean; userId: string; timezone?: string; refreshVersion: number; onNotice: (message: string) => void }
type SourceItem = PlanItem & { task_status?: string }
type WrittenScreen = { signature: string; device: string; page: number; pages: number; time: string }
type NativeConnection = 'idle' | 'needs-permission' | 'denied' | 'checking' | 'connected' | 'unavailable' | 'origin-mismatch' | 'insecure'
const PHASE_LABELS: Record<TransferProgress['phase'], string> = { preparing: '正在准备画面', authenticating: '正在连接墨水屏', sending: '正在传输画面', waiting: '等待设备刷新', done: '设备已确认写入' }
function taskKey(item: PlanItem) { return item.logical_id || item.id }
function taskState(item: SourceItem) { return item.status === 'satisfied' ? '已完成' : item.status === 'skipped' ? '已跳过' : item.status === 'not_completed' ? '未完成' : item.status === 'in_progress' || item.task_status === 'in_progress' ? '进行中' : '待完成' }
function errorMessage(error: unknown) { return error instanceof Error ? error.message : '操作失败，请重试' }
function bluetoothError(error: unknown) {
  if (error instanceof DOMException) {
    if (error.name === 'NotFoundError') return '未选中墨水屏，请核对蓝牙 ID、电源和距离。'
    if (error.name === 'NotAllowedError' || error.name === 'SecurityError') return '请允许浏览器访问蓝牙。'
    if (error.name === 'NetworkError') return '蓝牙连接中断，请靠近墨水屏后重试。'
  }
  return errorMessage(error)
}
function profileFields(profile: PanelProfile) { return { display_name: profile.display_name, seat: profile.seat, title: profile.title, avatar_data_url: profile.avatar_data_url, weather_override: profile.weather_override, task_overrides: profile.task_overrides || {} } }

export function FastPanel({ active, userId, timezone, refreshVersion, onNotice }: Props) {
  const zone = timezoneOrDefault(timezone || '')
  const today = () => localDateInTimezone(new Date(), zone)
  const [date, setDate] = useState(today), [loadedDate, setLoadedDate] = useState('')
  const [items, setItems] = useState<SourceItem[]>([]), [loading, setLoading] = useState(false), [loadError, setLoadError] = useState(''), [loadedAt, setLoadedAt] = useState('')
  const [pageIndex, setPageIndex] = useState(0), [profile, setProfile] = useState<PanelProfile>({ ...EMPTY_PROFILE })
  const [savedProfile, setSavedProfile] = useState<PanelProfile | null>(null), [profileETag, setProfileETag] = useState(''), [profileError, setProfileError] = useState(''), [profileSaving, setProfileSaving] = useState(false)
  const [avatar, setAvatar] = useState<HTMLImageElement | null>(null), [weather, setWeather] = useState<PanelWeather | null>(null), [weatherError, setWeatherError] = useState('')
  const [displayId, setDisplayId] = useState(''), [transport, setTransport] = useState<'native' | 'browser'>('native')
  const [nativeHealth, setNativeHealth] = useState<NativeHealth | null>(null), [nativeError, setNativeError] = useState(''), [nativeDevices, setNativeDevices] = useState<NativeDevice[]>([]), [nativeDevice, setNativeDevice] = useState<NativeDevice | null>(null)
  const [nativeConnection, setNativeConnection] = useState<NativeConnection>('idle'), [commandCopied, setCommandCopied] = useState(''), [choosingDevice, setChoosingDevice] = useState(false)
  const [connecting, setConnecting] = useState(false), [device, setDevice] = useState<{ id: string; name: string } | null>(null), [connectionError, setConnectionError] = useState('')
  const [sending, setSending] = useState(false), [progress, setProgress] = useState<TransferProgress | null>(null), [writeError, setWriteError] = useState(''), [written, setWritten] = useState<WrittenScreen | null>(null), [renderError, setRenderError] = useState('')
  const [pairings, setPairings] = useState<PanelPairing[]>([])
  const [deviceHistory, setDeviceHistory] = useState<RememberedPanelDevice[]>([]), [rememberedBrowserId, setRememberedBrowserId] = useState(''), [searchProgress, setSearchProgress] = useState<SearchProgress | null>(null)
  const client = useRef<WolinkPanelClient | null>(null), owner = useRef(userId), loadSequence = useRef(0), operation = useRef(0), loadAbort = useRef<AbortController | null>(null), transferAbort = useRef<AbortController | null>(null)
  const canvas = useRef<HTMLCanvasElement>(null), busy = useRef(false), notice = useRef(onNotice), currentDate = useRef(date)
  const nativeApproved = useRef(false), nativeChecking = useRef(false), pairingsOwner = useRef('')
  const connectionAbort = useRef<AbortController | null>(null)
  owner.current = userId; notice.current = onNotice
  const capability = getBluetoothCapability()
  const pageOrigin = window.location.origin, cloudPage = !isLoopbackPanelPage(pageOrigin), startCommand = nativeStartCommand(pageOrigin)
  const nativeOriginMatches = Boolean(nativeHealth && nativeConfigMatchesOrigin(nativeHealth.config, pageOrigin))
  const ownPairing = pairingsOwner.current === userId ? matchingPanelPairing(nativeHealth?.config, pairings, pageOrigin) : undefined

  useLayoutEffect(() => {
    const account = userId, panel = new WolinkPanelClient({ onDisconnected: () => { if (client.current === panel && owner.current === account) { setDevice(null); setConnectionError('蓝牙已断开，请重新连接') } } })
    client.current = panel; currentDate.current = localDateInTimezone(new Date(), zone); operation.current++
    pairingsOwner.current = ''
    setDate(currentDate.current); setLoadedDate(''); setItems([]); setPageIndex(0); setProfile({ ...EMPTY_PROFILE }); setSavedProfile(null); setProfileETag(''); setProfileError(''); setAvatar(null)
    const history = loadPanelDeviceHistory(pageOrigin, account), recent = history[0]
    setDeviceHistory(history); setDisplayId(recent?.name || ''); setTransport(recent?.transport || 'native'); setRememberedBrowserId(recent?.transport === 'browser' ? recent.id || '' : '')
    setDevice(null); setNativeHealth(null); setNativeDevice(recent?.transport === 'native' ? { name: recent.name, address: recent.address! } : null); setNativeDevices([]); setConnectionError(''); setWriteError(''); setProgress(null); setWritten(null); setConnecting(false); setSending(false); setSearchProgress(null); setLoadError(''); setLoadedAt(''); setPairings([]); setChoosingDevice(false); busy.current = false
    return () => { operation.current++; loadSequence.current++; loadAbort.current?.abort(); connectionAbort.current?.abort(); transferAbort.current?.abort(); if (client.current === panel) client.current = null; panel.disconnect() }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [userId])
  useEffect(() => {
    let cancelled = false
    if (!userId) return
    const account = userId
    void (async () => {
      try {
        const response = await request<PanelProfile>('/panel/profile', {}, account)
        if (!cancelled && owner.current === account) { const value = { ...response.data, task_overrides: response.data.task_overrides || {} }; setProfile(value); setSavedProfile(value); setProfileETag(response.etag || ''); setProfileError('') }
      } catch (error) { if (!cancelled && owner.current === account) setProfileError(`资料加载失败：${errorMessage(error)}`) }
    })()
    return () => { cancelled = true }
  }, [userId])
  useEffect(() => {
    let cancelled = false
    setAvatar(null)
    if (!profile.avatar_data_url) return
    const image = new Image(); image.onload = () => { if (!cancelled) setAvatar(image) }; image.onerror = () => { if (!cancelled) setProfileError('头像无法读取，请重新上传') }; image.src = profile.avatar_data_url
    return () => { cancelled = true; image.onload = null; image.onerror = null }
  }, [profile.avatar_data_url])

  const load = useCallback(async () => {
    if (!userId) return
    const account = userId, requestedDate = localDateInTimezone(new Date(), zone)
    if (currentDate.current !== requestedDate) { transferAbort.current?.abort(); currentDate.current = requestedDate; setDate(requestedDate); setLoadedDate(''); setItems([]); setPageIndex(0); setWritten(null) }
    const sequence = ++loadSequence.current; loadAbort.current?.abort()
    const controller = new AbortController(); loadAbort.current = controller; setLoading(true)
    try {
      const data = (await request<ScheduledDay>('/daily-plans/current', { signal: controller.signal }, account)).data
      if (sequence !== loadSequence.current || owner.current !== account || requestedDate !== localDateInTimezone(new Date(), zone)) return
      if (data.plan.local_date !== requestedDate) throw new Error('计划日期与今天不一致，请重新读取')
      setItems((data.today_items ?? data.items ?? []).filter(item => item.status !== 'superseded').sort((left, right) => (left.position ?? 0) - (right.position ?? 0))); setDate(requestedDate); setLoadedDate(requestedDate); setLoadError(''); setLoadedAt(new Date().toLocaleTimeString('zh-CN', { timeZone: zone, hour: '2-digit', minute: '2-digit' }))
    } catch (error) {
      if (sequence !== loadSequence.current || owner.current !== account || controller.signal.aborted) return
      if (error instanceof ApiError && error.status === 404) { setItems([]); setLoadedDate(requestedDate); setLoadError(''); setLoadedAt('') } else setLoadError(errorMessage(error))
    } finally { if (sequence === loadSequence.current && owner.current === account) setLoading(false) }
  }, [userId, zone])
  const loadWeather = useCallback(async () => {
    try { const response = await fetch('/api/v1/panel/weather', { cache: 'no-store', signal: AbortSignal.timeout(15_000) }); if (!response.ok) throw new Error('天气服务暂不可用'); setWeather(await response.json() as PanelWeather); setWeatherError('') }
    catch (error) { setWeatherError(errorMessage(error)) }
  }, [])
  const loadNative = useCallback(async (interactive = false) => {
    if (!owner.current || nativeChecking.current) return
    if (!window.isSecureContext || cloudPage && window.location.protocol !== 'https:') { setNativeHealth(null); setNativeConnection('insecure'); setNativeError('云端 FastTask 请使用 HTTPS。'); return }
    nativeChecking.current = true
    const account = owner.current
    try {
      if (cloudPage && !interactive) {
        const permission = await localNetworkPermission()
        if (owner.current !== account) return
        if (permission === 'denied') { setNativeHealth(null); setNativeConnection('denied'); setNativeError('请在站点设置中允许“本地网络访问”。'); return }
        if (permission !== 'granted' && !nativeApproved.current) { setNativeConnection(previous => previous === 'unavailable' || previous === 'origin-mismatch' ? previous : 'needs-permission'); return }
      }
      setNativeConnection('checking')
      // First access from a cloud page only occurs during an explicit click.
      const response = await nativePanel<NativeHealth>('/health')
      if (owner.current !== account) return
      if (!response.config || typeof response.ble_supported !== 'boolean') throw new NativePanelError('请重新启动本项目的 Windows 助手。', 'invalid_health')
      if (response.sync_mode !== 'on_change' || !response.capabilities?.guarded_refresh) throw new NativePanelError('请先更新本机 Windows 助手代码并重新启动，以启用任务变化同步。', 'upgrade_required')
      setNativeHealth(response); nativeApproved.current = true
      if (!nativeConfigMatchesOrigin(response.config, pageOrigin)) { setNativeConnection('origin-mismatch'); setNativeError('助手地址与当前页面不匹配，请按连接帮助重新启动。'); setNativeDevice(null); return }
      setNativeConnection('connected'); setNativeError(''); return response
    } catch (error) {
      if (owner.current !== account) return
      setNativeHealth(null)
      const permission = cloudPage ? await localNetworkPermission() : 'unknown'
      if (owner.current !== account) return
      setNativeConnection(permission === 'denied' ? 'denied' : error instanceof NativePanelError && error.code === 'invalid_origin' ? 'origin-mismatch' : 'unavailable')
      setNativeError(permission === 'denied' ? '请在站点设置中允许“本地网络访问”。' : errorMessage(error))
    } finally { nativeChecking.current = false }
  }, [cloudPage, pageOrigin])
  const loadPairings = useCallback(async () => {
    if (!userId) return
    const account = userId
    try { const { data } = await request<{ items: PanelPairing[] }>('/panel/pairings', {}, account); if (owner.current === account) { pairingsOwner.current = account; setPairings(data.items || []) } }
    catch { if (owner.current === account) { pairingsOwner.current = ''; setPairings([]) } }
  }, [userId])
  useEffect(() => { if (active && userId) { void load(); void loadWeather(); void loadNative(); void loadPairings() } }, [active, userId, load, loadWeather, loadNative, loadPairings, refreshVersion])
  useEffect(() => {
    if (!active || !userId) return
    // The page only reads state. The Windows process monitors changes and writes.
    const healthTimer = window.setInterval(() => { void loadNative() }, 30_000)
    const stateTimer = window.setInterval(() => { void load(); void loadWeather(); void loadPairings() }, 60_000)
    return () => { window.clearInterval(healthTimer); window.clearInterval(stateTimer) }
  }, [active, userId, load, loadWeather, loadNative, loadPairings])
  useEffect(() => {
    function checkDay() {
      const next = localDateInTimezone(new Date(), zone)
      if (next !== currentDate.current) { currentDate.current = next; loadSequence.current++; loadAbort.current?.abort(); transferAbort.current?.abort(); setDate(next); setLoadedDate(''); setItems([]); setPageIndex(0); setWritten(null); setLoadedAt(''); setLoadError(''); if (active) { void load(); void loadWeather() } }
    }
    function foreground() { checkDay(); if (active && document.visibilityState === 'visible') { void load(); void loadNative(); void loadPairings() } }
    const timer = window.setInterval(checkDay, 30_000); document.addEventListener('visibilitychange', foreground); window.addEventListener('focus', foreground); checkDay()
    return () => { window.clearInterval(timer); document.removeEventListener('visibilitychange', foreground); window.removeEventListener('focus', foreground) }
  }, [active, load, loadNative, loadPairings, loadWeather, zone])
  useEffect(() => {
    // Only a current account's verified pairing can restore a saved device.
    if (transport !== 'native' || choosingDevice || !ownPairing || !nativeHealth?.config.address) return
    setNativeDevice({ name: nativeHealth.config.device_name, address: nativeHealth.config.address }); setDisplayId(nativeHealth.config.device_name)
    setDeviceHistory(rememberPanelDevice(pageOrigin, userId, { transport: 'native', name: nativeHealth.config.device_name, address: nativeHealth.config.address }))
  }, [ownPairing?.id, nativeHealth?.config.address, nativeHealth?.config.device_name, choosingDevice, transport])

  // Legacy text corrections remain readable; selection/order no longer hide or
  // rearrange today's automatically synchronized tasks.
  const tasks = useMemo(() => items.map(source => { const key = taskKey(source), override = profile.task_overrides[key] || {}; return { key, source, title: override.title ?? source.title, detail: override.detail || '' } }), [items, profile.task_overrides])
  const pages = useMemo(() => paginateBadge(tasks.map(task => ({ key: task.key, title: [task.title.trim() || task.source.title, task.detail.trim()].filter(Boolean).join('\n'), status: taskState(task.source) }))), [tasks])
  const previewPage = Math.max(0, Math.min(pageIndex, pages.length - 1))
  const weatherText = profile.weather_override.trim() ? `${profile.weather_override.trim()}（手动）` : weather && weather.status !== 'unavailable' && weather.local_date === date ? `东西湖 · ${weather.summary}${weather.status === 'stale' || weatherError ? '（缓存）' : ''}` : '东西湖 · 天气暂不可用'
  const signature = JSON.stringify([date, profileFields(profile), weatherText, previewPage, tasks.map(task => [task.key, task.title, task.detail, taskState(task.source)])])
  const profileDirty = Boolean(savedProfile && JSON.stringify(profileFields(profile)) !== JSON.stringify(profileFields(savedProfile)))
  useEffect(() => { setPageIndex(previous => previous < pages.length ? previous : 0) }, [pages.length])
  useLayoutEffect(() => { if (!canvas.current) return; try { drawBadge(canvas.current, pages[previewPage], profile, weatherText, date, previewPage, pages.length, avatar); setRenderError('') } catch (error) { setRenderError(errorMessage(error)) } }, [pages, previewPage, profile, weatherText, date, avatar])

  async function uploadAvatar(file?: File) { if (!file) return; const account = userId; try { const avatarURL = await prepareAvatar(file); if (owner.current === account) { setProfile(previous => ({ ...previous, avatar_data_url: avatarURL })); setProfileError('') } } catch (error) { if (owner.current === account) setProfileError(errorMessage(error)) } }
  async function saveProfile() {
    if (!userId || !savedProfile || profileSaving) return
    const account = userId; setProfileSaving(true); setProfileError('')
    try {
      const liveKeys = new Set(items.map(taskKey))
      const saving = { ...profile, task_overrides: loadedDate === date && !loadError ? Object.fromEntries(Object.entries(profile.task_overrides).filter(([key]) => liveKeys.has(key))) : profile.task_overrides }
      if (!profileETag) throw new Error('请重新加载资料后保存')
      const response = await request<PanelProfile>('/panel/profile', { method: 'PUT', headers: { 'If-Match': profileETag }, body: JSON.stringify(profileFields(saving)) }, account)
      if (owner.current !== account) return
      setProfile(response.data); setSavedProfile(response.data); setProfileETag(response.etag || ''); notice.current('显示资料已保存')
    } catch (error) {
      if (owner.current !== account) return
      if (error instanceof ApiError && error.status === 412) {
        setProfileError('资料已在其他页面修改。当前编辑已保留；还原后可载入最新版本。')
        try { const response = await request<PanelProfile>('/panel/profile', {}, account); if (owner.current === account) { setSavedProfile(response.data); setProfileETag(response.etag || '') } } catch { /* Preserve draft. */ }
      } else setProfileError(errorMessage(error))
    } finally { if (owner.current === account) setProfileSaving(false) }
  }
  async function pairDevice(target: NativeDevice, account: string, currentOperation: number) {
    if (!account) return
    assertAccessOwner(token.get(), account)
    if (owner.current !== account || operation.current !== currentOperation) return
    // Recheck account ownership at connection time, never just has_token.
    const [health, response] = await Promise.all([nativePanel<NativeHealth>('/health'), request<{ items: PanelPairing[] }>('/panel/pairings', {}, account)])
    if (owner.current !== account || operation.current !== currentOperation) return
    if (health.sync_mode !== 'on_change' || !health.capabilities?.guarded_refresh) throw new NativePanelError('请先更新本机 Windows 助手代码并重新启动，以启用任务变化同步。', 'upgrade_required')
    if (!nativeConfigMatchesOrigin(health.config, pageOrigin)) throw new Error('助手地址与当前页面不匹配，请按连接帮助重新启动。')
    const currentPairings = response.data.items || [], pairing = matchingPanelPairing(health.config, currentPairings, pageOrigin)
    const reusable = pairing && health.config.device_name === target.name && health.config.address?.toUpperCase() === target.address.toUpperCase()
    let issued: PanelPairing | undefined, installed = false
    try {
      if (!reusable) {
        issued = (await request<PanelPairing>('/panel/pairings', { method: 'POST', headers: { 'Idempotency-Key': idem() }, body: JSON.stringify({ device_name: target.name }) }, account)).data
        if (!issued.token) throw new Error('未收到设备授权，请重试连接')
      }
      if (owner.current !== account || operation.current !== currentOperation) return
      assertAccessOwner(token.get(), account)
      if (issued || !health.config.enabled) {
        await nativePanel('/config', { device_name: target.name, address: target.address, fasttask_url: pageOrigin, pairing_id: issued?.id || pairing!.id, ...(issued ? { panel_token: issued.token } : {}), page: 0 }, 'PUT')
        installed = true
      }
      if (owner.current !== account || operation.current !== currentOperation) return
      pairingsOwner.current = account
      setPairings(issued ? [...currentPairings, { id: issued.id, device_name: issued.device_name, expires_at: issued.expires_at }] : currentPairings); await loadNative()
    } finally {
      if (issued && (!installed || owner.current !== account || operation.current !== currentOperation)) {
        try { await request(`/panel/pairings/${issued.id}`, { method: 'DELETE' }, account) } catch { /* Never persist the token in UI storage. */ }
      }
    }
  }
  async function connect(chooseNew = false) {
    const panel = client.current, account = userId
    if (!account || !panel || busy.current || transport === 'native' && !displayId.trim() || nativeHealth?.busy) return
    const currentOperation = ++operation.current
    const controller = new AbortController(); connectionAbort.current = controller
    let progressTimer: number | undefined
    busy.current = true; setConnecting(true); setSearchProgress(null); setConnectionError(''); setWriteError('')
    try {
      assertAccessOwner(token.get(), account)
      if (transport === 'native') {
        const health = nativeOriginMatches ? nativeHealth : await loadNative(true)
        if (owner.current !== account || operation.current !== currentOperation) return
        controller.signal.throwIfAborted()
        if (!health?.ble_supported || !nativeConfigMatchesOrigin(health.config, pageOrigin)) return
        let target = nativeDevice
        if (!target) {
          setSearchProgress({ phase: 'scanning', attempt: 1, maxAttempts: 4 })
          let polling = false
          progressTimer = window.setInterval(() => {
            if (polling || owner.current !== account || operation.current !== currentOperation) return
            polling = true
            void nativePanel<NativeHealth>('/health').then(state => {
              if (owner.current !== account || operation.current !== currentOperation) return
              setNativeHealth(state)
              const status = state.last_status
              if (status?.phase && status.phase !== 'sending' && status.attempt && status.max_attempts) setSearchProgress({ phase: status.phase, attempt: status.attempt, maxAttempts: status.max_attempts })
            }).catch(() => {}).finally(() => { polling = false })
          }, 1_000)
          const response = await nativePanel<{ devices: NativeDevice[] }>('/scan', { device_name: displayId.trim() })
          if (owner.current !== account || operation.current !== currentOperation) return
          controller.signal.throwIfAborted()
          setNativeDevices(response.devices || [])
          const exact = response.devices.filter(value => value.name.toUpperCase() === displayId.trim().toUpperCase())
          if (exact.length === 1) target = exact[0]
          if (!response.devices.length) throw new Error(`未扫描到 ${displayId}，请确认已开机并靠近电脑`)
          if (!target) return
          setNativeDevice(target)
        }
        setSearchProgress({ phase: 'connecting', attempt: 1, maxAttempts: 1 })
        controller.signal.throwIfAborted()
        await pairDevice(target, account, currentOperation)
        if (owner.current !== account || operation.current !== currentOperation) return
        controller.signal.throwIfAborted()
        setDeviceHistory(rememberPanelDevice(pageOrigin, account, { transport: 'native', name: target.name, address: target.address }))
        setChoosingDevice(false); notice.current(`已连接 ${target.name}`)
      } else {
        const options = { signal: controller.signal, onSearchProgress: (value: SearchProgress) => { if (owner.current === account && operation.current === currentOperation) setSearchProgress(value) } }
        // The first chooser is still invoked directly in the click stack.
        const connected = rememberedBrowserId && !chooseNew ? await panel.connectRemembered(rememberedBrowserId, displayId.trim(), options) : await panel.connect(chooseNew ? '' : displayId.trim(), options)
        if (owner.current !== account || client.current !== panel || operation.current !== currentOperation) { panel.disconnect(); return }
        setDevice(connected); setDisplayId(connected.name); setRememberedBrowserId(connected.id)
        setDeviceHistory(rememberPanelDevice(pageOrigin, account, { transport: 'browser', name: connected.name, id: connected.id })); notice.current(`已连接 ${connected.name}`)
      }
    } catch (error) { if (owner.current === account && operation.current === currentOperation) setConnectionError(controller.signal.aborted ? '已取消查找设备' : bluetoothError(error)) }
    finally { if (progressTimer) window.clearInterval(progressTimer); if (connectionAbort.current === controller) connectionAbort.current = null; if (owner.current === account && operation.current === currentOperation) { setConnecting(false); setSearchProgress(null); busy.current = false; if (transport === 'native') void loadNative() } }
  }
  function disconnect() { operation.current++; connectionAbort.current?.abort(); transferAbort.current?.abort(); client.current?.disconnect(); setDevice(null); setNativeDevice(null); setNativeDevices([]); setChoosingDevice(true); setConnectionError('') }
  function chooseHistory(value: string) {
    const remembered = deviceHistory.find(entry => panelDeviceKey(entry) === value)
    if (!remembered) return
    disconnect(); setTransport(remembered.transport); setDisplayId(remembered.name)
    setRememberedBrowserId(remembered.transport === 'browser' ? remembered.id || '' : '')
    setNativeDevice(remembered.transport === 'native' ? { name: remembered.name, address: remembered.address! } : null)
  }
  async function cancelConnection() {
    connectionAbort.current?.abort()
    if (transport === 'native' && connecting) { try { await nativePanel('/cancel', {}) } catch (error) { setConnectionError(errorMessage(error)) } }
  }
  async function send() {
    const panel = client.current, preview = canvas.current, account = userId
    if (!account || !panel || !preview || busy.current || nativeHealth?.busy || loadError || renderError || loadedDate !== today() || transport === 'browser' && !panel.connected || transport === 'native' && (profileDirty || !nativeDevice || !nativeOriginMatches || !ownPairing)) return
    const sentDate = date, sentPage = previewPage, sentPages = pages.length, sentSignature = signature, currentOperation = ++operation.current
    busy.current = true; setSending(true); setWriteError(''); setProgress(null)
    const controller = new AbortController(); transferAbort.current = controller
    try {
      assertAccessOwner(token.get(), account)
      let name = ''
      if (transport === 'native') {
        const live = await nativePanel<NativeHealth>('/health')
        if (owner.current !== account || operation.current !== currentOperation) return
        if (live.sync_mode !== 'on_change' || !live.capabilities?.guarded_refresh) throw new NativePanelError('请先更新本机 Windows 助手代码并重新启动，以启用任务变化同步。', 'upgrade_required')
        const result = await nativePanel<{ ok: boolean; notification: string }>('/refresh', { page: sentPage, expected_pairing_id: ownPairing!.id, expected_device_name: nativeDevice!.name, expected_address: nativeDevice!.address })
        if (!result.ok || result.notification?.toLowerCase() !== 'ff00000000000000') throw new Error('未收到设备完成通知，请查看屏幕后重试')
        name = nativeDevice!.name
      } else { const image = drawBadge(preview, pages[sentPage], profile, weatherText, sentDate, sentPage, sentPages, avatar); const result = await panel.sendImage(image, { signal: controller.signal, onProgress: value => { if (owner.current === account && client.current === panel) setProgress(value) } }); name = result.deviceName }
      if (owner.current !== account || operation.current !== currentOperation || sentDate !== today()) return
      setWritten({ signature: sentSignature, device: name, page: sentPage + 1, pages: sentPages, time: new Date().toLocaleTimeString('zh-CN', { timeZone: zone, hour: '2-digit', minute: '2-digit' }) }); notice.current('墨水屏已确认更新')
    } catch (error) { if (owner.current === account && operation.current === currentOperation) setWriteError(controller.signal.aborted ? '发送已取消，请重新写入完整画面' : bluetoothError(error)) }
    finally { if (owner.current === account && operation.current === currentOperation) { setSending(false); busy.current = false; transferAbort.current = null; void loadNative() } }
  }
  async function copyStartCommand() { try { await navigator.clipboard.writeText(startCommand); setCommandCopied('已复制') } catch { setCommandCopied('请选择命令手动复制') } }

  const frozen = sending || connecting || profileSaving, helperBusy = Boolean(nativeHealth?.busy)
  const nativeReady = Boolean(nativeDevice && ownPairing && nativeHealth?.config.enabled && nativeHealth.config.device_name === nativeDevice.name && nativeHealth.config.address?.toUpperCase() === nativeDevice.address.toUpperCase())
  const selectedDeviceName = transport === 'native' ? nativeReady ? nativeDevice?.name : undefined : device?.name
  const canSend = Boolean(selectedDeviceName && !frozen && !helperBusy && (transport === 'browser' || !profileDirty && nativeOriginMatches && nativeHealth?.ble_supported) && !loading && !loadError && !renderError && (!profile.avatar_data_url || avatar) && loadedDate === date && date === today())
  const nativeLabel = nativeConnection === 'checking' ? '正在连接…' : nativeConnection === 'denied' ? '本地访问被拒绝' : nativeConnection === 'needs-permission' || nativeConnection === 'idle' && cloudPage ? '等待连接助手' : nativeConnection === 'origin-mismatch' ? '地址不匹配' : nativeConnection === 'insecure' ? '需要 HTTPS' : nativeHealth ? helperBusy ? '正在更新墨水屏' : nativeHealth.ble_supported ? '助手在线' : '蓝牙不可用' : '助手未连接'
  const completed = tasks.filter(task => task.source.status === 'satisfied').length
  const historySelection = transport === 'native' && nativeDevice ? panelDeviceKey({ transport, address: nativeDevice.address }) : transport === 'browser' && rememberedBrowserId ? panelDeviceKey({ transport, id: rememberedBrowserId }) : ''
  const syncFailure = nativeReady && nativeHealth?.last_status?.status === 'failed' ? nativeHealth.last_status.message : ''

  if (!userId) return null
  return <section className="page panel-page" hidden={!active} aria-label="工牌更新">
    <header className="page-head"><div className="title-block"><h1>工牌更新</h1><p className="meta">{date} · 今日任务</p></div><mdui-button variant="text" icon="refresh" loading={loading} disabled={loading || frozen} onClick={() => { void load(); void loadWeather() }}>刷新预览</mdui-button></header>
    <div className="panel-layout">
      <div className="panel-primary">
        <mdui-card variant="outlined" className="panel-section panel-preview-section" aria-labelledby="panel-preview-heading">
          <div className="panel-section-head"><h2 id="panel-preview-heading">屏幕预览</h2><span className="panel-subtle">800 × 480</span></div>
          <div className="panel-preview-stage"><div className="panel-screen"><canvas ref={canvas} width={DISPLAY_WIDTH} height={DISPLAY_HEIGHT} role="img" aria-label={`${date} 工牌及今日任务，预览第 ${previewPage + 1} 页，共 ${pages.length} 页`}/></div></div>
          <div className="panel-preview-footer"><span className="meta">{profileDirty ? '资料修改尚未保存' : `${tasks.length} 项任务 · 已完成 ${completed} 项`}</span>{pages.length > 1 && <div className="panel-pagination"><mdui-button-icon icon="chevron_left" aria-label="上一页预览" disabled={frozen || previewPage === 0} onClick={() => setPageIndex(previewPage - 1)}/><span>{previewPage + 1} / {pages.length}</span><mdui-button-icon icon="chevron_right" aria-label="下一页预览" disabled={frozen || previewPage + 1 >= pages.length} onClick={() => setPageIndex(previewPage + 1)}/></div>}</div>
          {renderError && <p className="panel-error" role="alert">{renderError}</p>}
        </mdui-card>
        <section className="panel-tasks-section" aria-labelledby="panel-tasks-heading">
          <div className="panel-section-head"><h2 id="panel-tasks-heading">今日任务</h2><span className="meta">{loadedAt ? `${loadedAt} 更新` : ''}</span></div>
          {loadError ? <p className="panel-error" role="alert">读取失败：{loadError}</p> : loading && !loadedDate ? <p className="panel-empty">正在读取…</p> : !tasks.length ? <p className="panel-empty">今天暂无任务</p> : <ol className="panel-task-list">{tasks.map((task, index) => <li key={task.key}><span className="panel-task-number">{String(index + 1).padStart(2, '0')}</span><div className="panel-task-text"><span>{task.title.trim() || task.source.title}</span>{task.detail && <small>{task.detail}</small>}</div><span className={`panel-task-status${task.source.status === 'satisfied' ? ' is-complete' : ''}`}>{taskState(task.source)}</span></li>)}</ol>}
        </section>
      </div>
      <aside className="panel-sidebar">
        <mdui-card variant="outlined" className="panel-section" aria-labelledby="panel-profile-heading">
          <div className="panel-section-head"><h2 id="panel-profile-heading">显示资料</h2></div>
          <div className="panel-profile-layout"><label className={`panel-avatar-upload${frozen ? ' is-disabled' : ''}`} title="选择头像或证件照">{profile.avatar_data_url ? <img src={profile.avatar_data_url} alt="工牌头像"/> : <mdui-icon name="person_outline"/>}<span>更换照片</span><input aria-label="上传头像或证件照" type="file" accept="image/png,image/jpeg,image/webp" disabled={frozen} onChange={event => { void uploadAvatar(event.target.files?.[0]); event.target.value = '' }}/></label><div className="panel-profile-fields"><mdui-text-field label="姓名" aria-label="工牌姓名" variant="outlined" value={profile.display_name} maxlength={40} disabled={frozen} onInput={event => setProfile(previous => ({ ...previous, display_name: fieldValue(event) }))}/><mdui-text-field label="顶栏标题" variant="outlined" value={profile.title} maxlength={40} disabled={frozen} onInput={event => setProfile(previous => ({ ...previous, title: fieldValue(event) }))}/></div></div>
          <div className="panel-weather-line"><div><span className="panel-subtle">武汉 · 东西湖</span><strong>{profile.weather_override.trim() ? `${profile.weather_override.trim()}（手动）` : weather?.local_date === date && weather.status !== 'unavailable' ? `${weather.summary}${weather.status === 'stale' || weatherError ? '（缓存）' : ''}` : '天气暂不可用'}</strong></div></div>
          {weatherError && <p className="panel-error" role="status">{weatherError}</p>}
          <details className="panel-details"><summary>更多资料设置</summary><div className="panel-details-content"><mdui-text-field label="修正天气（留空自动获取）" variant="outlined" value={profile.weather_override} maxlength={80} disabled={frozen} placeholder="例如 小雨 22°C" onInput={event => setProfile(previous => ({ ...previous, weather_override: fieldValue(event) }))}/>{profile.avatar_data_url && <mdui-button variant="text" disabled={frozen} onClick={() => setProfile(previous => ({ ...previous, avatar_data_url: '' }))}>移除照片</mdui-button>}</div></details>
          <div className="panel-form-actions"><mdui-button variant="filled" disabled={frozen || !profileDirty || !savedProfile} loading={profileSaving} onClick={() => void saveProfile()}>保存资料</mdui-button>{profileDirty && <mdui-button variant="text" disabled={frozen || !savedProfile} onClick={() => { if (savedProfile) setProfile({ ...savedProfile }); setProfileError('') }}>还原</mdui-button>}</div>
          {profileError && <p className="panel-error" role="alert">{profileError}</p>}
        </mdui-card>
        <mdui-card variant="outlined" className="panel-section panel-connection" aria-labelledby="panel-connection-heading">
          <div className="panel-section-head"><h2 id="panel-connection-heading">设备</h2><span className={`panel-connection-state${selectedDeviceName ? ' is-connected' : ''}`}><i/>{selectedDeviceName ? '已连接' : '未连接'}</span></div>
          {!!deviceHistory.length && <mdui-select label="已连接设备" variant="outlined" value={historySelection} disabled={frozen || helperBusy} onChange={event => chooseHistory(fieldValue(event))}>{deviceHistory.map(value => <mdui-menu-item key={panelDeviceKey(value)} value={panelDeviceKey(value)}>{value.name} · {value.transport === 'native' ? 'Windows' : '浏览器'}</mdui-menu-item>)}</mdui-select>}
          {selectedDeviceName ? <div className="panel-connected-device"><mdui-icon name="bluetooth_connected"/><div><strong>{selectedDeviceName}</strong><span>{transport === 'native' ? nativeDevice?.address : '浏览器蓝牙'}</span></div><mdui-button variant="text" disabled={frozen || helperBusy} onClick={disconnect}>更换</mdui-button></div> : <>
            <mdui-text-field label="蓝牙名称 / ID" variant="outlined" autocomplete="off" spellcheck={false} placeholder={transport === 'browser' ? '留空选择附近设备' : '输入墨水屏的蓝牙名称 / ID'} value={displayId} disabled={frozen} onInput={event => { setDisplayId(fieldValue(event).toUpperCase()); setRememberedBrowserId(''); setNativeDevice(null); setNativeDevices([]); setConnectionError('') }}/>
            {transport === 'native' && !!nativeDevices.length && <mdui-select label="附近设备" variant="outlined" value={nativeDevice?.address || ''} disabled={frozen} onChange={event => setNativeDevice(nativeDevices.find(value => value.address === fieldValue(event)) || null)}>{nativeDevices.map(value => <mdui-menu-item key={value.address} value={value.address}>{value.name} · {value.address}</mdui-menu-item>)}</mdui-select>}
            <mdui-button variant="filled" full-width icon="bluetooth" disabled={frozen || helperBusy || transport === 'native' && (!displayId.trim() || nativeConnection === 'insecure') || transport === 'browser' && !capability.supported} loading={connecting} onClick={() => void connect()}>{rememberedBrowserId && transport === 'browser' ? '重新连接设备' : '连接设备'}</mdui-button>
          </>}
          {connecting && <div className="panel-search-progress" role="status"><mdui-linear-progress/><div><span>{searchProgress ? `${searchProgress.phase === 'scanning' ? '正在查找设备' : '正在连接设备'} ${searchProgress.attempt} / ${searchProgress.maxAttempts}` : '正在准备连接…'}</span>{(transport === 'browser' || searchProgress?.phase === 'scanning') && <mdui-button variant="text" onClick={() => void cancelConnection()}>取消</mdui-button>}</div></div>}
          {transport === 'native' && !connecting && <p className="meta" role="status">{selectedDeviceName ? helperBusy ? '正在更新墨水屏…' : nativeHealth?.monitor?.pending ? '检测到变化，等待更新' : '任务变化后自动同步' : nativeLabel}</p>}
          {syncFailure && <p className="panel-error" role="status">上次同步失败：{syncFailure}</p>}
          {nativeError && transport === 'native' && <p className="panel-error" role="status">{nativeError}</p>}{connectionError && <p className="panel-error" role="alert">{connectionError}</p>}
          {selectedDeviceName && <mdui-button variant="outlined" full-width icon="sync" disabled={!canSend} loading={sending} onClick={() => void send()}>{sending ? progress ? PHASE_LABELS[progress.phase] : '等待设备刷新…' : '更新墨水屏'}</mdui-button>}
          {selectedDeviceName && transport === 'native' && profileDirty && <p className="meta" role="status">请先保存资料，后台将自动更新墨水屏。</p>}
          {sending && transport === 'browser' && <div className="panel-transfer" role="status"><mdui-linear-progress value={(progress?.percent || 0) / 100}/><mdui-button variant="text" onClick={() => transferAbort.current?.abort()}>取消发送</mdui-button></div>}
          {writeError && <p className="panel-error" role="alert">{writeError}</p>}{written && <p className="panel-written" role="status">{written.signature === signature ? `${written.time} 已确认更新${written.pages > 1 ? ` · 第 ${written.page} / ${written.pages} 页` : ''}` : '预览已变化，等待更新'}</p>}
          <details className="panel-details"><summary>连接帮助</summary><div className="panel-details-content"><mdui-select label="连接方式" variant="outlined" value={transport} disabled={frozen || helperBusy} onChange={event => { disconnect(); setRememberedBrowserId(''); setTransport(fieldValue(event) as 'native' | 'browser') }}><mdui-menu-item value="native">Windows 助手</mdui-menu-item><mdui-menu-item value="browser">浏览器蓝牙</mdui-menu-item></mdui-select>{transport === 'browser' ? <><p className="meta">{capability.supported ? '浏览器连接仅在当前页面有效。关闭页面后自动同步需使用 Windows 助手。' : capability.reason}</p><mdui-button variant="text" disabled={frozen || helperBusy || !capability.supported} onClick={() => { disconnect(); setRememberedBrowserId(''); void connect(true) }}>选择其他设备</mdui-button></> : <><p className="meta">电脑需开机且未休眠，墨水屏在附近。首次连接请允许本地网络访问。</p><code className="panel-native-command">{startCommand}</code><div className="panel-form-actions"><mdui-button variant="text" onClick={() => void copyStartCommand()}>复制启动命令</mdui-button>{commandCopied && <span className="meta" role="status">{commandCopied}</span>}<mdui-button variant="text" disabled={frozen || nativeConnection === 'insecure'} onClick={() => void loadNative(true)}>重新检测</mdui-button></div></>}</div></details>
        </mdui-card>
      </aside>
    </div>
  </section>
}
