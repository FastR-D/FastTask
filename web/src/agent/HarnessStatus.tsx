import { useEffect, useState } from 'react'
import { fieldValue } from '../mdui-react'
import { resetModeProbe, subscribeHarnessStatus, warmUpHarness, type HarnessStatus } from '../harness'
import { readModePreference, writeModePreference, type ModePreference } from './modePreference'

/** The browser's connectivity flag is advisory; local services can remain reachable. */
export function useOnline(): boolean {
  const [online, setOnline] = useState<boolean>(() => (typeof navigator === 'undefined' ? true : navigator.onLine !== false))
  useEffect(() => {
    const update = () => setOnline(navigator.onLine !== false)
    window.addEventListener('online', update)
    window.addEventListener('offline', update)
    update()
    return () => {
      window.removeEventListener('online', update)
      window.removeEventListener('offline', update)
    }
  }, [])
  return online
}

// The harness status line (doc/harness.md §3.2).
//
// Degradation must be visible: a user on a browser without JSPI is running through the sidecar (or not
// at all), and saying nothing would leave them guessing why the agent behaves differently. The same line
// carries the one-time notice that a thread's history was compressed because its checkpoint could not be
// read (§6.3), and any error the host hit.

const MODE_LABEL: Record<string, string> = {
  wasm: '本机运行',
  sidecar: '服务端运行',
  unavailable: '不可用',
  idle: '待命',
}

const REASON_LABEL: Record<string, string> = {
  LIBFX_JSPI_UNAVAILABLE: '当前浏览器不支持 WASM 线程，已改用服务端宿主',
  LIBFX_WASM_LOAD_FAILED: '本机宿主加载失败，已改用服务端宿主',
  FORCED: '按设置强制指定',
  LIBFX_JSPI_AVAILABLE: '',
}

export function HarnessStatusLine() {
  const [status, setStatus] = useState<HarnessStatus | null>(null)
  const online = useOnline()

  useEffect(() => {
    // The probe compiles 2 MB of wasm, so it belongs here — when the chat view mounts — and not on the
    // send path of the first message (§3.2).
    void warmUpHarness().catch(() => undefined)
    return subscribeHarnessStatus(setStatus)
  }, [])

  const networkHint = !online
    ? <span className="agent-harness-reason">浏览器报告网络不可用；可继续写草稿，发送时将尝试连接 FastTask。</span>
    : null
  if (!status || status.mode === 'idle') {
    if (online) return null
    return (
      <div className="agent-harness" data-testid="harness-status" data-mode="idle">
        {networkHint}
      </div>
    )
  }
  const degraded = status.mode !== 'wasm'
  const reason = status.reason ? REASON_LABEL[status.reason] ?? status.reason : ''
  return (
    <div className={`agent-harness${degraded ? ' degraded' : ''}`} data-testid="harness-status" data-mode={status.mode}>
      <span className="agent-harness-mode">{MODE_LABEL[status.mode] ?? status.mode}</span>
      {degraded && reason && <span className="agent-harness-reason">{reason}</span>}
      {status.detail && <span className="agent-harness-detail">{status.detail}</span>}
      {status.contextDegraded && (
        <span className="agent-harness-degraded">本会话已从保留的历史记录恢复</span>
      )}
      {status.error && <span className="agent-harness-error">{status.error}</span>}
      {networkHint}
    </div>
  )
}

// The host switch (§3.2's "管理员或用户在设置中强制指定", §15's decision to expose it to ordinary users).
//
// It sits on the same line as the status because the two answer one question: where is the agent running,
// and do I want it somewhere else. Choosing a host clears the session's probe answer, so the switch takes
// effect on the next run instead of after a reload, and re-warms the probe when it is set back to auto.
export function HarnessModeControl() {
  const [preference, setPreference] = useState<ModePreference>(() => readModePreference())

  function choose(value: ModePreference) {
    writeModePreference(value)
    setPreference(value)
    resetModeProbe()
    void warmUpHarness().catch(() => undefined)
  }

  return (
    <mdui-select
      className="agent-harness-choice"
      variant="outlined"
      label="运行位置"
      value={preference}
      onChange={event => choose(fieldValue(event) as ModePreference)}
    >
      <mdui-menu-item value="auto">自动（探测本机能力）</mdui-menu-item>
      <mdui-menu-item value="wasm">本机运行</mdui-menu-item>
      <mdui-menu-item value="sidecar">服务端运行</mdui-menu-item>
    </mdui-select>
  )
}
