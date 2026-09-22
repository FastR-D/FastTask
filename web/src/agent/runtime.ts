import { useAssistantTransportRuntime, type AssistantRuntime } from '@assistant-ui/react'
import { ensureFreshAccessToken } from '../api'
import { cancelActiveRun, driveRun, selectMode } from '../harness'
import { convertState } from './converter'
import type { ServerAgentState } from './state'

// authHeaders is the only hook that runs before assistant-ui issues a request, so
// it is the only place the access token can be refreshed for the three agent
// endpoints (frontend.md §5). They bypass api.ts's reactive 401 retry, so the
// token must be proactively valid here. It shares api.ts's single-flight refresh,
// avoiding concurrent refresh-token rotation races.
async function authHeaders(): Promise<Record<string, string>> {
  const access = await ensureFreshAccessToken()
  return access ? { Authorization: `Bearer ${access}` } : {}
}

// lastUserText is the message the runtime is sending. The harness host needs the text to prompt with
// (§5.1), and assistant-ui — not this module — owns the request, so the text is captured on the way out
// and read on the way back.
let lastUserText = ''

/** extractUserText concatenates the text parts of the add-message commands in a request body. */
export function extractUserText(commands: unknown): string {
  if (!Array.isArray(commands)) return ''
  const parts: string[] = []
  for (const command of commands) {
    if (!command || typeof command !== 'object') continue
    const entry = command as { type?: string; message?: { parts?: Array<{ type?: string; text?: string }> } }
    if (entry.type !== 'add-message') continue
    for (const part of entry.message?.parts ?? []) {
      if (part?.type === 'text' && typeof part.text === 'string') parts.push(part.text)
    }
  }
  return parts.join('').trim()
}

/**
 * prepareAgentCommand adds the two fields the server needs beyond assistant-ui's own body: the thread
 * identity, and the harness mode that says who drives the run (doc/harness.md §1.2).
 *
 * The mode comes from the session probe rather than from a guess, and the probe is awaited here — it is
 * cached after the first call and warmed when the chat mounts (§3.2), so this does not put a 2 MB wasm
 * compile on the send path. An unavailable host sends no mode at all, which leaves the run to the
 * server instead of creating one nobody can drive.
 */
export async function prepareAgentCommand(body: { state?: unknown; commands?: unknown; [key: string]: unknown }) {
  const state = body.state as ServerAgentState | undefined
  const text = extractUserText(body.commands)
  if (text) lastUserText = text
  const selection = await selectMode(null)
  const mode = selection.mode === 'unavailable' ? undefined : selection.mode
  return {
    ...body,
    threadId: state?.fasttask?.threadId ?? null,
    ...(mode ? { harness_mode: mode } : {}),
  }
}

/** harnessRunOf reads the run a browser host has to drive. assistant-ui owns the fetch, so the header is
 *  the only place the server can name it (§1.2). */
export function harnessRunOf(response: Response): string | null {
  return response.headers.get('X-Harness-Run')
}

// useAgentRuntime wires the assistant-transport runtime. ALL transport wiring is
// converged in this single file (ADR-0002 §4: "前端把运行时接线收敛在单个文件内").
export function useAgentRuntime(initialState: ServerAgentState, onNotice: (message: string) => void): AssistantRuntime {
  const runtime = useAssistantTransportRuntime<ServerAgentState>({
    // MUST be explicit: the default protocol is "data-stream", and omitting this
    // silently speaks the wrong protocol — the stream looks fine but messages never
    // render (agent-impl.md §2.1, frontend.md §4).
    protocol: 'assistant-transport',
    api: '/api/v1/agent/commands',
    resumeApi: '/api/v1/agent/resume',
    resumeStateApi: '/api/v1/agent/resume-state',
    initialState,
    converter: convertState,
    headers: authHeaders,
    // assistant-ui's in-memory thread list assigns a __LOCALID_... remoteId
    // before the first command. Only the ID returned by FastTask in state is a
    // server thread; sending the temporary ID makes the first message a 404.
    prepareSendCommandsRequest: prepareAgentCommand,
    // A run the server handed to a browser host has to be driven from here: the Worker will not touch
    // it, and nothing else does (doc/harness.md §1.2). The stream keeps rendering from server state
    // while the host works, so the UI needs no change (§13 phase D).
    onResponse: response => {
      const runId = harnessRunOf(response)
      if (!runId) return
      void driveRun({ runId, text: lastUserText }).catch(error => {
        onNotice(`对话驱动失败：${error instanceof Error ? error.message : '请稍后重试'}`)
      })
    },
    // The stop button cancels through the server first: cancelling is the user's power, and a host may
    // not cancel its own run (§5.2, §10.2).
    onCancel: () => {
      void cancelActiveRun().catch(() => undefined)
    },
    onError: (error, { commands }) => {
      const unsent = commands.find(command => command.type === 'add-message')
      if (unsent?.type === 'add-message') {
        const text = unsent.message.parts
          .flatMap(part => part.type === 'text' ? [part.text] : [])
          .join('')
        if (text) runtime.thread.composer.setText(text)
      }
      onNotice(`对话发送失败：${error.message}`)
    },
  })
  return runtime
}
