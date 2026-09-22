import { useMemo } from 'react'
import { useAssistantTransportRuntime, type AssistantRuntime } from '@assistant-ui/react'
import { ensureFreshAccessToken } from '../api'
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

export function prepareAgentCommand(body: { state?: unknown; [key: string]: unknown }) {
  const state = body.state as ServerAgentState | undefined
  return { ...body, threadId: state?.fasttask?.threadId ?? null }
}

// useAgentRuntime wires the assistant-transport runtime. ALL transport wiring is
// converged in this single file (ADR-0002 §4: "前端把运行时接线收敛在单个文件内").
export function useAgentRuntime(goalId: string | null, onNotice: (message: string) => void): AssistantRuntime {
  // goalId seeds the optimistic active-goal hint; the server remains the source of
  // truth and pushes fasttask.activeGoalId back over the same stream (§2.7).
  const initialState = useMemo<ServerAgentState>(
    () => ({
      messages: [],
      isRunning: false,
      fasttask: goalId ? { activeGoalId: goalId, pendingProposals: [] } : { pendingProposals: [] },
    }),
    [goalId],
  )
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
