import { unstable_useComposerInput } from '@assistant-ui/react'
import { useEffect, useRef, useState } from 'react'
import { idem, request } from '../api'
import { waitJob } from '../jobs'
import type { Job } from '../types'
import { negotiateRecordingMime } from './audio'
import { useComposerDraftUserId } from './ComposerDraft'

type Capture = {
  ownerId: string
  controller: AbortController
  stream?: MediaStream
  media?: MediaRecorder
  chunks: Blob[]
  submitted: boolean
}

function stopTracks(capture: Capture) {
  capture.stream?.getTracks().forEach(track => track.stop())
  capture.stream = undefined
}

// Detach first: stop() also fires onstop, which must never upload on unmount.
function disposeCapture(capture: Capture) {
  capture.controller.abort()
  const media = capture.media
  if (media) {
    media.ondataavailable = null; media.onstop = null; media.onerror = null
    if (media.state !== 'inactive') { try { media.stop() } catch { /* Release the tracks even if the recorder failed. */ } }
  }
  stopTracks(capture)
  capture.chunks = []
}

// Record → explicitly stop → upload → confirm the transcription in the composer.
// A capture belongs to this mounted conversation and its authenticated account.
export function VoiceButton({ onNotice }: { onNotice: (message: string) => void }) {
  const { value, setText } = unstable_useComposerInput()
  const userId = useComposerDraftUserId()
  const owner = useRef(userId), draft = useRef(value)
  owner.current = userId; draft.current = value
  const [recording, setRecording] = useState(false)
  const [busy, setBusy] = useState(false)
  const captureRef = useRef<Capture | null>(null)
  const mounted = useRef(false)
  useEffect(() => {
    mounted.current = true
    setRecording(false); setBusy(false)
    return () => {
      mounted.current = false
      const capture = captureRef.current
      captureRef.current = null
      if (capture) disposeCapture(capture)
    }
  }, [userId])

  function current(capture: Capture) {
    return mounted.current && captureRef.current === capture && owner.current === capture.ownerId && !capture.controller.signal.aborted
  }

  function fail(capture: Capture, message: string) {
    const visible = current(capture)
    disposeCapture(capture)
    if (captureRef.current === capture) captureRef.current = null
    if (visible) { setRecording(false); setBusy(false); onNotice(message) }
  }

  async function transcribe(capture: Capture, mime: string, extension: string) {
    stopTracks(capture)
    if (!current(capture) || !capture.submitted) return
    setRecording(false); setBusy(true)
    const blob = new Blob(capture.chunks, { type: mime })
    capture.chunks = []
    if (!blob.size) { fail(capture, '没有录到声音，请重试'); return }
    const form = new FormData()
    form.append('audio', blob, `voice.${extension}`)
    try {
      const { data } = await request<Job>('/voice-transcription-jobs', {
        method: 'POST', headers: { 'Idempotency-Key': idem() }, body: form, signal: capture.controller.signal,
      }, capture.ownerId)
      if (!current(capture)) return
      const job = await waitJob(data.id, { readJob: async id => {
        capture.controller.signal.throwIfAborted()
        return (await request<Job>(`/agent-jobs/${id}`, { signal: capture.controller.signal }, capture.ownerId)).data
      } })
      if (!current(capture)) return
      if (job.status !== 'succeeded') throw new Error(job.error_message || '转写作业失败')
      if (job.output_json) {
        const output = JSON.parse(job.output_json) as { transcript?: string }
        if (typeof output.transcript === 'string' && output.transcript) {
          // Typing remains available while transcription is pending.
          setText([draft.current?.trimEnd(), output.transcript].filter(Boolean).join('\n'))
        }
      }
      onNotice('转写完成，请确认后发送')
    } catch (error) {
      if (current(capture)) onNotice(error instanceof Error ? error.message : '转写失败')
    } finally {
      const visible = current(capture)
      disposeCapture(capture)
      if (captureRef.current === capture) captureRef.current = null
      if (visible) { setRecording(false); setBusy(false) }
    }
  }

  async function toggle() {
    const previous = captureRef.current
    if (previous) {
      if (!current(previous) || !previous.media || previous.submitted || previous.media.state !== 'recording') return
      previous.submitted = true
      setRecording(false); setBusy(true)
      try { previous.media.stop() } catch { fail(previous, '录音未能完成，请重新录制') }
      return
    }
    if (!mounted.current || !userId) return
    if (typeof MediaRecorder === 'undefined' || !navigator.mediaDevices?.getUserMedia) { onNotice('当前浏览器不支持录音'); return }
    const format = negotiateRecordingMime(mime => MediaRecorder.isTypeSupported(mime))
    if (!format) { onNotice('浏览器不支持可用的录音格式'); return }
    const capture: Capture = { ownerId: userId, controller: new AbortController(), chunks: [], submitted: false }
    captureRef.current = capture
    setBusy(true)
    try {
      capture.stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      // getUserMedia cannot be aborted; late permission results still need disposal.
      if (!current(capture)) { disposeCapture(capture); return }
      const media = new MediaRecorder(capture.stream, { mimeType: format.mime })
      capture.media = media
      media.ondataavailable = event => { if (current(capture) && event.data.size) capture.chunks.push(event.data) }
      media.onerror = () => fail(capture, '录音中断，请检查麦克风后重新录制')
      media.onstop = () => {
        if (!capture.submitted) { fail(capture, '录音已中断，未上传音频，请重新录制'); return }
        void transcribe(capture, format.mime, format.ext)
      }
      media.start()
      if (!current(capture)) { disposeCapture(capture); return }
      setRecording(true); setBusy(false)
    } catch { fail(capture, '浏览器无法访问麦克风，请检查权限') }
  }

  return <mdui-button-icon
    className={recording ? 'agent-mic recording' : 'agent-mic'}
    icon={busy ? 'hourglass_top' : recording ? 'stop' : 'mic'}
    disabled={busy}
    aria-label={recording ? '停止录音' : '语音输入'}
    onClick={toggle}
  />
}
