import { unstable_useComposerInput } from '@assistant-ui/react'
import { useRef, useState } from 'react'
import { idem, request } from '../api'
import type { Job } from '../types'
import { negotiateRecordingMime } from './audio'

// VoiceButton implements the existing voice interaction (frontend.md §4.2):
// record → upload → poll the transcription job → fill the composer for the user to
// confirm and send. It does NOT auto-run, and does NOT use assistant-ui's
// DictationAdapter (that targets in-browser live dictation; FastTask transcription
// is a server job). The container is negotiated via MediaRecorder.isTypeSupported
// and the real MIME + matching extension are sent to the backend (pwa.md §2 #3).
export function VoiceButton({ onNotice }: { onNotice: (message: string) => void }) {
  const { setText } = unstable_useComposerInput()
  const [recording, setRecording] = useState(false)
  const [busy, setBusy] = useState(false)
  const recorder = useRef<MediaRecorder | null>(null)
  const chunks = useRef<Blob[]>([])

  async function toggle() {
    if (recording) {
      recorder.current?.stop()
      return
    }
    if (typeof MediaRecorder === 'undefined' || !navigator.mediaDevices?.getUserMedia) {
      onNotice('当前浏览器不支持录音')
      return
    }
    const format = negotiateRecordingMime(mime => MediaRecorder.isTypeSupported(mime))
    if (!format) {
      onNotice('浏览器不支持可用的录音格式')
      return
    }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true })
      const media = new MediaRecorder(stream, { mimeType: format.mime })
      chunks.current = []
      media.ondataavailable = event => {
        if (event.data.size > 0) chunks.current.push(event.data)
      }
      media.onstop = async () => {
        stream.getTracks().forEach(track => track.stop())
        setRecording(false)
        const blob = new Blob(chunks.current, { type: format.mime })
        if (blob.size === 0) {
          onNotice('没有录到声音，请重试')
          return
        }
        const form = new FormData()
        // Real MIME + matching extension, never a hardcoded voice.webm.
        form.append('audio', blob, `voice.${format.ext}`)
        setBusy(true)
        try {
          const { data } = await request<Job>('/voice-transcription-jobs', {
            method: 'POST',
            headers: { 'Idempotency-Key': idem() },
            body: form,
          })
          const job = await waitJob(data.id)
          if (job.status !== 'succeeded') throw new Error(job.error_message || '转写作业失败')
          if (job.output_json) {
            const output = JSON.parse(job.output_json) as { transcript?: string }
            if (output.transcript) setText(output.transcript)
          }
          onNotice('转写完成，请确认后发送')
        } catch (error) {
          onNotice(error instanceof Error ? error.message : '转写失败')
        } finally {
          setBusy(false)
        }
      }
      media.start()
      recorder.current = media
      setRecording(true)
    } catch {
      onNotice('浏览器无法访问麦克风，请检查权限')
    }
  }

  return (
    <mdui-button-icon
      className={recording ? 'agent-mic recording' : 'agent-mic'}
      icon={busy ? 'hourglass_top' : recording ? 'stop' : 'mic'}
      disabled={busy}
      aria-label={recording ? '停止录音' : '语音输入'}
      onClick={toggle}
    />
  )
}

// waitJob polls a job until it reaches a terminal state (mirrors App.tsx's helper).
async function waitJob(id: string): Promise<Job> {
  for (let i = 0; i < 40; i++) {
    await new Promise(resolve => setTimeout(resolve, 250))
    const { data } = await request<Job>(`/agent-jobs/${id}`)
    if (['succeeded', 'failed', 'cancelled'].includes(data.status)) return data
  }
  throw new Error('转写超时，请稍后重试')
}
