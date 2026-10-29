import { useAui, useAssistantTransportRuntime, useAssistantTransportState, type AssistantRuntime, type AttachmentAdapter } from '@assistant-ui/react'
import { AssistantMessageAccumulator, AssistantTransportDecoder, unstable_createInitialMessage } from 'assistant-stream'
import { asAsyncIterableStream, type ReadonlyJSONValue } from 'assistant-stream/utils'
import { useEffect, useRef } from 'react'
import { ensureFreshAccessToken, offlineSessionAvailable, readOfflineUser, refreshAccessTokenAfterUnauthorized } from '../api'
import { activeRunId, cancelActiveRun, driveRun, selectMode } from '../harness'
import { forcedModeOf, readModePreference } from './modePreference'
import { convertState } from './converter'
import type { ApprovalDecision, ServerAgentState } from './state'
import { threadState } from './threads'
import { useComposerDraftUserId } from './ComposerDraft'
import { writeComposerDraft } from './draft-storage'
import { assertAccessOwner, SESSION_OWNER_CHANGED } from '../session-owner'

export function agentErrorText(error: unknown): string {
  const message = error instanceof Error ? error.message : typeof error === 'string' ? error : '请稍后重试'
  if (message.includes('HARNESS_UNAVAILABLE')) return '当前浏览器无法直接运行研究助手，备用服务也未连接。请更新浏览器或联系管理员启用备用服务。'
  if (/\bRUN_ACTIVE\b/.test(message) || message.includes('an agent run is already active')) return '当前账号还有对话正在生成，请回到正在生成的对话完成或停止后重试。'
  if (message.includes('thread not found')) return '这段对话已不可用，请重新选择会话。'
  if (message.includes('run is not awaiting approval')) return '这次对话已停止等待确认。请重新载入对话核对提案状态，再继续研究。'
  if (message.includes('approval receipt already resolved')) return '这个提案已经处理过。请查看最新结果后继续研究。'
  if (message.includes('the task tree changed before this approval')) return '任务已发生变化，这个提案已失效。请让助手根据最新任务重新提出建议。'
  if (message.includes('the proposal basis changed before this approval')) return '相关任务或计划已发生变化，这个提案已失效。请让助手读取最新状态后重新提出建议。'
  if (message.includes('authentication required')) return '登录已过期，请重新登录后继续。'
  if (message.includes('internal server error') || message.includes('model configuration unavailable')) return '对话服务暂时不可用，请稍后重试。'
  if (/failed to fetch|fetch failed|networkerror/i.test(message)) return '无法连接对话服务，请检查网络后重试。'
  return message
}

function assertOwner(access: string | null, ownerId?: string, getUserId?: () => string | undefined): asserts access is string {
  if (getUserId && !ownerId) throw new Error('账号信息正在载入，草稿已保留，请稍后发送。')
  if (getUserId && getUserId() !== ownerId) throw new Error(SESSION_OWNER_CHANGED)
  if (!access) {
    if (offlineSessionAvailable()) {
      if (ownerId && readOfflineUser()?.id !== ownerId) throw new Error(SESSION_OWNER_CHANGED)
      throw new Error('需要联网，草稿已保留，请联网后重试。')
    }
    throw new Error('authentication required')
  }
  if (ownerId) assertAccessOwner(access, ownerId)
}

async function authHeaders(ownerId?: string, getUserId?: () => string | undefined): Promise<Record<string, string>> {
  const access = await ensureFreshAccessToken()
  assertOwner(access, ownerId, getUserId)
  return { Authorization: `Bearer ${access}` }
}

type ProposalReceiptOptions = {
  threadId?: string
  state?: ServerAgentState
  onState?: (state: ServerAgentState) => void
  signal?: AbortSignal
  ownerId?: string
  getUserId?: () => string | undefined
}

// Decisions always use a separate receipt: even a stopped stream must not put
// an old approval in the same SDK batch as the user's next message. A normal
// worker can return its continuation here; decode its server state just as the
// main transport does. Harness and interrupted-run receipts may only return DONE.
export async function sendProposalDecision(
  _streaming: boolean, toolCallId: string, toolName: string, result: ApprovalDecision,
  _addResult: (result: ApprovalDecision) => void,
  options: ProposalReceiptOptions = {},
): Promise<void> {
  const headers:Record<string,string>={ 'Content-Type': 'application/json', ...await authHeaders(options.ownerId, options.getUserId) }
  const restoredRunId=options.state?.fasttask?.runId
  const detachHarness=Boolean(restoredRunId&&activeRunId()!==restoredRunId)
  const controller=new AbortController()
  const timeout=setTimeout(()=>controller.abort(),20_000)
  let response:Response
  const body=JSON.stringify({ ...(options.threadId?{threadId:options.threadId}:{}), commands: [{ type: 'add-tool-result', toolCallId, toolName, ...(detachHarness?{detachHarness:true}:{}), result }] })
  const signal=options.signal?AbortSignal.any([controller.signal,options.signal]):controller.signal
  try {
    response = await fetch('/api/v1/agent/commands', {
      method: 'POST', headers,
      body, signal,
    })
    if(response.status===401 && options.ownerId && !signal.aborted) {
      const access=await refreshAccessTokenAfterUnauthorized(headers.Authorization.slice('Bearer '.length))
      assertOwner(access,options.ownerId,options.getUserId)
      if(signal.aborted)throw signal.reason
      // Authentication rejected the first receipt before any business action.
      // Replay this exact receipt once, never a message or a changed decision.
      response=await fetch('/api/v1/agent/commands',{method:'POST',headers:{...headers,Authorization:`Bearer ${access}`},body,signal})
    }
  } finally { clearTimeout(timeout) }
  if (!response.ok) {
    let detail = '确认未能送达，请稍后重试。'
    try { const problem = await response.json(); detail = problem.detail || detail } catch { /* keep the useful fallback */ }
    throw new Error(agentErrorText(detail))
  }
  if(!response.body)throw new Error('确认响应不完整，请重新载入对话核对结果。')
  const initial=options.state??{messages:[],isRunning:false,fasttask:{}}
  let streamError:string|undefined
  const stream=response.body.pipeThrough(new AssistantTransportDecoder({strict:true})).pipeThrough(new AssistantMessageAccumulator({
    initialMessage:unstable_createInitialMessage({unstable_state:initial as unknown as ReadonlyJSONValue}),
    strict:true,onError:error=>{streamError=error},
  }))
  for await(const chunk of asAsyncIterableStream(stream)) {
    const next=chunk.metadata.unstable_state
    if(next&&next!==initial)options.onState?.(next as unknown as ServerAgentState)
  }
  if(streamError)throw new Error(agentErrorText(streamError))
}

/** Synchronize the exact restored thread without remounting or clearing its composer. */
export function useProposalDecision() {
  const aui=useAui()
  const userId=useComposerDraftUserId()
  const currentOwner=useRef(userId)
  currentOwner.current=userId
  const owner=useRef(userId)
  owner.current??=userId
  const state=useAssistantTransportState<ServerAgentState>(value=>value as unknown as ServerAgentState)
  const stateRef=useRef(state)
  stateRef.current=state
  const requests=useRef(new Set<AbortController>())
  useEffect(()=>()=>{for(const controller of requests.current)controller.abort()},[])
  return async(toolCallId:string,toolName:string,result:ApprovalDecision)=>{
    const initial=stateRef.current,threadId=initial.fasttask?.threadId
    if(!threadId)throw new Error('无法确认提案所属的对话，请重新载入后重试。')
    const controller=new AbortController()
    requests.current.add(controller)
    const importState=(next:ServerAgentState)=>{
      if(controller.signal.aborted)return
      if(next.fasttask?.threadId!==threadId)throw new Error('提案响应的对话不一致，请重新载入后核对。')
      stateRef.current=next
      aui.thread.importExternalState(next)
    }
    let deliveryError:unknown
    try {
      try { await sendProposalDecision(false,toolCallId,toolName,result,()=>{}, {threadId,state:initial,onState:importState,signal:controller.signal,ownerId:owner.current,getUserId:()=>currentOwner.current}) }
      catch(error) { deliveryError=error }
      if(!controller.signal.aborted) {
        try {
          const basis=stateRef.current
          const latest=await threadState(threadId)
          // The user or another server stream may advance this thread while
          // the GET is in flight. That newer transcript must not be rolled back.
          if(stateRef.current===basis)importState(latest)
        }
        catch(error) { if(!deliveryError)deliveryError=new Error(`确认请求已送达，但最新对话未能载入。请重新载入对话核对结果：${agentErrorText(error)}`) }
      }
      if(deliveryError)throw deliveryError
    } finally { requests.current.delete(controller) }
  }
}

// lastUserText is the message the runtime is sending. The harness host needs the text to prompt with
// (§5.1), and assistant-ui — not this module — owns the request, so the text is captured on the way out
// and read on the way back.
let lastUserText = ''
// lastRequestedHarnessMode records who was asked to drive the current command. The response header is
// the fast path for learning the run id, but a transport/proxy can leave it unreadable; in that case the
// streamed fasttask.runId is the authoritative fallback.
let lastRequestedHarnessMode: 'wasm' | 'sidecar' | '' = ''

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
  lastUserText = text
  // A user who picked a host in settings beats the probe (§3.2); 'auto' means probe.
  const selection = await selectMode(forcedModeOf(readModePreference()))
  const mode = selection.mode === 'unavailable' ? undefined : selection.mode
  lastRequestedHarnessMode = mode ?? ''
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

export type AgentRuntimeOptions = {
  /** Stable reader of the real account prop; it never remounts the SDK runtime. */
  getUserId?: () => string | undefined
  /** The image attachment adapter (doc/chat-features.md §4.2). Omitted in tests that do not exercise it. */
  attachments?: AttachmentAdapter
  /** Called when a run's stream ends, so the conversation list can re-read titles and recency (§2). */
  onThreadChanged?: () => void
  /** Only server-confirmed applied results refresh business views, once per tool call. */
  onBusinessChanged?: () => void
}

// useAgentRuntime wires the assistant-transport runtime. ALL transport wiring is
// converged in this single file (ADR-0002 §4: "前端把运行时接线收敛在单个文件内").
export function useAgentRuntime(
  initialState: ServerAgentState,
  onNotice: (message: string) => void,
  options: AgentRuntimeOptions = {},
): AssistantRuntime {
  const owner=useRef(options.getUserId?.())
  const sentAuth=useRef<{ownerId?:string;access:string}|null>(null)
  const drivenRuns=useRef(new Set<string>())
  const noticeRef=useRef(onNotice)
  noticeRef.current=onNotice
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
    headers: async()=>{
      owner.current??=options.getUserId?.()
      const headers=await authHeaders(owner.current,options.getUserId)
      sentAuth.current={ownerId:owner.current,access:headers.Authorization.slice('Bearer '.length)}
      return headers
    },
    // assistant-ui's in-memory thread list assigns a __LOCALID_... remoteId
    // before the first command. Only the ID returned by FastTask in state is a
    // server thread; sending the temporary ID makes the first message a 404.
    prepareSendCommandsRequest: async body=>{
      const prepared=await prepareAgentCommand(body)
      const access=await ensureFreshAccessToken()
      assertOwner(access,owner.current,options.getUserId)
      // SDK captured its headers before the awaited host probe. A peer may have
      // rotated them in the meantime; stop before sending known-stale headers.
      if(sentAuth.current?.access!==access)throw new Error('登录状态已更新，草稿已保留，请再次发送。')
      return prepared
    },
    // Attachments are in the transport options, and the adapter uploads to our own storage so a message
    // carries a reference instead of bytes (doc/chat-features.md §4.1, §4.2).
    ...(options.attachments ? { adapters: { attachments: options.attachments } } : {}),
    onFinish: () => options.onThreadChanged?.(),
    // A run the server handed to a browser host has to be driven from here: the Worker will not touch
    // it, and nothing else does (doc/harness.md §1.2). The stream keeps rendering from server state
    // while the host works, so the UI needs no change (§13 phase D).
    onResponse: response => {
      if (options.getUserId && options.getUserId() !== owner.current) return
      const runId = harnessRunOf(response)
      if (!runId || drivenRuns.current.has(runId)) return
      drivenRuns.current.add(runId)
      void driveRun({ ownerId: owner.current, getOwner: options.getUserId, runId, text: lastUserText, forcedMode: forcedModeOf(readModePreference()) }).catch(error => {
        if (!options.getUserId || options.getUserId() === owner.current) noticeRef.current(`对话驱动失败：${agentErrorText(error)}`)
      })
    },
    // assistant-ui also invokes onCancel({error}) after a transport failure.
    // A dropped stream or reload must preserve the pending proposal; only an
    // explicit stop (without an error) requests cancellation on the server.
    onCancel: ({ error }) => {
      if (error) return
      const state = runtime.thread.getState().state as ServerAgentState | undefined
      void cancelActiveRun(state?.fasttask?.runId).catch(error => {
        noticeRef.current(`停止请求未送达：${agentErrorText(error)}`)
      })
    },
    onError: async (error, { commands }) => {
      const unsent = commands.find(command => command.type === 'add-message')
      if (unsent?.type === 'add-message') {
        const text = unsent.message.parts
          .flatMap(part => part.type === 'text' ? [part.text] : [])
          .join('')
        if (text) {
          if(options.getUserId && owner.current && options.getUserId()!==owner.current) {
            const current=runtime.thread.getState().state
            const threadId=current&&typeof current==='object'&&'threadId' in current&&typeof current.threadId==='string'?current.threadId:initialState.fasttask?.threadId
            writeComposerDraft(owner.current,threadId,text)
          }
          else runtime.thread.composer.setText(text)
        }
      }
      if(/\b(?:Status 401:|status 401:)/.test(error.message) && sentAuth.current?.ownerId) {
        try {
          const access=await refreshAccessTokenAfterUnauthorized(sentAuth.current.access)
          assertOwner(access,sentAuth.current.ownerId,options.getUserId)
          onNotice(unsent?'登录已恢复，草稿已保留，请再次发送。':'登录已恢复，请重新载入对话继续。')
        } catch(recoveryError) { onNotice(`对话发送失败：${agentErrorText(recoveryError)}`) }
        return
      }
      onNotice(`对话发送失败：${agentErrorText(error)}`)
    },
  })
  const businessRef = useRef(options.onBusinessChanged)
  businessRef.current = options.onBusinessChanged
  // If X-Harness-Run is not readable by the browser, the stream still carries the
  // same run id in fasttask.runId. Drive only runs the server actually accepted as
  // WASM; a wasm request may have fallen back to the sidecar.
  useEffect(() => {
    const maybeDrive = () => {
      if (options.getUserId && options.getUserId() !== owner.current) return
      if (lastRequestedHarnessMode !== 'wasm') return
      const state = runtime.thread.getState().state as ServerAgentState | undefined
      if (state?.fasttask?.harnessMode !== 'wasm') return
      const runId = state?.fasttask?.runId
      if (!state?.isRunning || !runId || drivenRuns.current.has(runId) || activeRunId() === runId) return
      drivenRuns.current.add(runId)
      void driveRun({ ownerId: owner.current, getOwner: options.getUserId, runId, text: lastUserText, forcedMode: forcedModeOf(readModePreference()) }).catch(error => {
        if (!options.getUserId || options.getUserId() === owner.current) noticeRef.current(`对话驱动失败：${agentErrorText(error)}`)
      })
    }
    maybeDrive()
    return runtime.thread.subscribe(maybeDrive)
  }, [runtime])
  const seen = useRef(new Set<string>())
  useEffect(() => {
    const inspect = () => {
      let changed = false
      for (const message of runtime.thread.getState().messages) {
        for (const part of message.content) {
          if (part.type !== 'tool-call' || part.isError || !part.result || typeof part.result !== 'object') continue
          if ((part.result as {applied?: unknown}).applied !== true || seen.current.has(part.toolCallId)) continue
          seen.current.add(part.toolCallId)
          changed = true
        }
      }
      if (changed) businessRef.current?.()
    }
    // Seed known results on restore without calling an old approval a new edit.
    for (const message of initialState.messages) for (const part of message.parts) {
      if (part.type === 'tool-call' && part.result && typeof part.result === 'object' && (part.result as {applied?: unknown}).applied === true) seen.current.add(part.toolCallId)
    }
    return runtime.thread.subscribe(inspect)
  }, [runtime])
  return runtime
}
