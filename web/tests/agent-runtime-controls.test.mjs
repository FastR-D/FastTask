import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createComponentHarness } from './helpers/component-harness.mjs'

async function setup({ runId = 'restored-sidecar', mode = 'sidecar', cancellationError } = {}) {
  let user = 'account-a', text = '', transport
  const listeners = new Set(), notices = [], cancelled = [], driven = [], saved = new Map()
  const initialState = { messages: [], isRunning: Boolean(runId), fasttask: { threadId: 'thread-a', ...(runId ? { runId, harnessMode: mode } : {}) } }
  const state = { state: initialState.fasttask, isRunning: Boolean(runId), messages: [] }
  const runtime = { thread: { getState: () => state, subscribe: listener => { listeners.add(listener); return () => listeners.delete(listener) }, composer: { getState: () => ({ text }), setText: value => { text = value } } } }
  const access = `header.${btoa(JSON.stringify({ sub: 'account-a' }))}.signature`
  const mocks = {
      '@assistant-ui/react': { useAssistantTransportRuntime: options => { transport = options; return runtime }, fromThreadMessageLike: value => value },
      'assistant-stream': {}, 'assistant-stream/utils': {},
      '../api': { ensureFreshAccessToken: async () => access, offlineSessionAvailable: () => false, readOfflineUser: () => ({ id: user }) },
      '../harness': { selectMode: async () => ({ mode }), activeRunId: () => 'unrelated-local-run', cancelActiveRun: async id => { cancelled.push(id); if (cancellationError) throw cancellationError }, driveRun: async value => { driven.push(value) } },
      './ComposerDraft': { useComposerDraftUserId: () => user },
      './modePreference': { readModePreference: () => 'auto', forcedModeOf: () => null },
      './threads': { threadState: async () => initialState },
  }
  // Bundling the wrapper rewrites relative externals against the fixture path.
  for (const [id, value] of Object.entries(mocks)) {
    if (id.startsWith('../')) mocks[`../../src/${id.slice(3)}`] = value
    else if (id.startsWith('./')) mocks[`../../src/agent/${id.slice(2)}`] = value
  }
  const harness = await createComponentHarness(new URL('./fixtures/agent-runtime-probe.ts', import.meta.url), 'AgentRuntimeProbe', {
    mocks,
    globals: { atob, btoa, Event, window: new EventTarget(), sessionStorage: { getItem: key => saved.get(key) ?? null, setItem: (key, value) => saved.set(key, value), removeItem: key => saved.delete(key) } },
  })
  harness.render({ initialState, onNotice: value => notices.push(value), options: { getUserId: () => user } })
  return { harness, state, notices, cancelled, driven, saved,
    get transport() { return transport }, get text() { return text },
    setDraft(value) { text = value }, setOwner(value) { user = value },
    emit() { for (const listener of listeners) listener() },
    async prepare(message = '本会话的问题') { await transport.headers(); return transport.prepareSendCommandsRequest({ state: initialState, commands: [{ type: 'add-message', message: { parts: [{ type: 'text', text: message }] } }] }) },
  }
}

test('the stop control cancels the restored server run instead of an unrelated local host', async () => {
  const f = await setup(); f.transport.onCancel({}); await f.harness.flush()
  assert.deepEqual(f.cancelled, ['restored-sidecar'])
  assert.equal(f.notices.length, 0)
  f.harness.unmount()
})

test('an unknown run or changed account never falls back to cancelling another conversation', async () => {
  const missing = await setup({ runId: '' }); missing.transport.onCancel({}); await missing.harness.flush()
  assert.deepEqual(missing.cancelled, [])
  assert.match(missing.notices.join(), /无法确认/)
  missing.harness.unmount()
  const changed = await setup(); changed.setOwner('account-b'); changed.transport.onCancel({}); await changed.harness.flush()
  assert.deepEqual(changed.cancelled, [])
  changed.harness.unmount()
})

test('failed cancellation is visible while transport errors never issue an implicit stop', async () => {
  const f = await setup({ cancellationError: new TypeError('Failed to fetch') })
  f.transport.onCancel({ error: new Error('stream failed') }); await f.harness.flush()
  assert.deepEqual(f.cancelled, [])
  f.transport.onCancel({}); await f.harness.flush()
  assert.deepEqual(f.cancelled, ['restored-sidecar'])
  assert.match(f.notices.join(), /停止请求未送达/)
  f.harness.unmount()
})

test('a WASM run is driven from converted runtime state when its response header is missing', async () => {
  const f = await setup({ mode: 'wasm', runId: 'wasm-run' }); await f.prepare(); f.emit(); f.emit(); await f.harness.flush()
  assert.deepEqual(f.driven.map(value => [value.runId, value.text]), [['wasm-run', '本会话的问题']])
  f.harness.unmount()
})

test('a failed send preserves the next draft and retains the failed text under its exact account and thread', async () => {
  const f = await setup(); await f.prepare('之前没有确认的问题'); f.setDraft('继续写的新问题')
  const commands = [{ type: 'add-message', message: { parts: [{ type: 'text', text: '之前没有确认的问题' }] } }]
  await f.transport.onError(new Error('network down'), { commands })
  // assistant-ui's run manager always calls onFinish in its finally block.
  f.transport.onFinish()
  assert.equal(f.text, '继续写的新问题')
  assert.deepEqual([...f.saved.entries()], [['fasttask.agent-draft.v1:account-a:thread:thread-a:failed-messages', '["之前没有确认的问题"]']])
  f.setOwner('account-b')
  await f.transport.onError(new Error('late error'), { commands: [{ type: 'add-message', message: { parts: [{ type: 'text', text: '不能写入新账号' }] } }] })
  assert.equal(f.text, '继续写的新问题')
  assert.equal(f.saved.size, 1)
  f.harness.unmount()
})

test('only a completed retry clears its failed message, while a cancelled retry retains it', async () => {
  const f = await setup(), message = '等待重试的问题'
  const commands = [{ type: 'add-message', message: { parts: [{ type: 'text', text: message }] } }]
  await f.prepare(message); f.setDraft('保留新草稿')
  await f.transport.onError(new Error('network down'), { commands }); f.transport.onFinish()
  assert.equal(f.saved.size, 1)
  await f.prepare(message); f.transport.onCancel({}); f.transport.onFinish(); await f.harness.flush()
  assert.equal(f.saved.size, 1)
  assert.equal(f.text, '保留新草稿')
  await f.prepare(message); f.transport.onFinish()
  assert.equal(f.saved.size, 0)
  assert.equal(f.text, '保留新草稿')
  f.harness.unmount()
})

test('a failed send restores an empty composer but an unmounted conversation cannot restore late text', async () => {
  const f = await setup(), commands = [{ type: 'add-message', message: { parts: [{ type: 'text', text: '可重新发送的问题' }] } }]
  await f.transport.onError(new Error('offline'), { commands })
  assert.equal(f.text, '可重新发送的问题')
  f.harness.unmount(); f.setDraft('另一段新草稿')
  await f.transport.onError(new Error('late failure'), { commands })
  assert.equal(f.text, '另一段新草稿')
})
