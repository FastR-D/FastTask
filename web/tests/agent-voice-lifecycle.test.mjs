import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createComponentHarness, deferred } from './helpers/component-harness.mjs'

async function setup({ permission, failAt, jobResult } = {}) {
  let owner = 'account-a', text = '', permissionCalls = 0, stopped = 0
  const requests = [], notices = [], writes = [], recorders = []
  const stream = { getTracks: () => [{ stop: () => { stopped++ } }] }
  class Recorder {
    static isTypeSupported() { return true }
    state = 'inactive'
    constructor() { if (failAt === 'constructor') throw new Error('recorder unavailable'); recorders.push(this) }
    start() { if (failAt === 'start') throw new Error('start failed'); this.state = 'recording' }
    stop() {
      this.state = 'inactive'
      this.ondataavailable?.({ data: new Blob(['audio']) })
      this.onstop?.()
    }
  }
  const harness = await createComponentHarness(new URL('../src/agent/VoiceButton.tsx', import.meta.url), 'VoiceButton', {
    mocks: {
      '@assistant-ui/react': { unstable_useComposerInput: () => ({ value: text, setText: value => { text = value; writes.push(value) } }) },
      './ComposerDraft': { useComposerDraftUserId: () => owner },
      '../api': { idem: () => 'test-key', request: async (path, options, account) => {
        requests.push({ path, options, account })
        return { data: path === '/voice-transcription-jobs' ? { id: 'voice-job' } : { status: 'succeeded', output_json: JSON.stringify({ transcript: '转写内容' }) } }
      } },
      '../jobs': { waitJob: (_id, options) => jobResult ? jobResult.promise : options.readJob('voice-job') },
    },
    globals: { Blob, navigator: { mediaDevices: { getUserMedia: () => { permissionCalls++; return permission ? permission.promise : Promise.resolve(stream) } } }, MediaRecorder: Recorder },
  })
  const props = { onNotice: message => notices.push(message) }
  harness.render(props)
  return { harness, stream, requests, notices, writes, recorders,
    click: () => harness.output.props.onClick(),
    setOwner(value) { owner = value; harness.render(props) },
    setDraft(value) { text = value; harness.render(props) },
    get stopped() { return stopped }, get permissionCalls() { return permissionCalls },
  }
}

test('switching conversation or account releases a recording without uploading it', async () => {
  for (const mode of ['unmount', 'account']) {
    const f = await setup(); await f.click(); await f.harness.flush()
    assert.equal(f.recorders[0].state, 'recording')
    if (mode === 'unmount') f.harness.unmount(); else f.setOwner('account-b')
    await f.harness.flush()
    assert.equal(f.recorders[0].state, 'inactive')
    assert.equal(f.stopped, 1)
    assert.equal(f.recorders[0].onstop, null)
    assert.equal(f.requests.length, 0)
    if (mode !== 'unmount') f.harness.unmount()
  }
})

test('a late microphone permission result is released after unmount and cannot create a recorder', async () => {
  const permission = deferred(), f = await setup({ permission })
  const first = f.click(); await f.harness.flush(); await f.click()
  assert.equal(f.permissionCalls, 1)
  f.harness.unmount(); permission.resolve(f.stream); await first; await f.harness.flush()
  assert.equal(f.stopped, 1)
  assert.equal(f.recorders.length, 0)
  assert.equal(f.requests.length, 0)
})

test('recorder construction, start and capture errors release tracks without uploading', async () => {
  for (const failAt of ['constructor', 'start', 'capture']) {
    const f = await setup({ failAt }); await f.click(); await f.harness.flush()
    if (failAt === 'capture') { f.recorders[0].onerror(); await f.harness.flush() }
    assert.equal(f.stopped, 1)
    assert.equal(f.requests.length, 0)
    assert.equal(f.harness.output.props.disabled, false)
    assert.ok(f.notices.length)
    f.harness.unmount()
  }
})

test('only an explicit stop uploads once and polling stays bound to the original account', async () => {
  const f = await setup(); await f.click(); await f.harness.flush()
  await f.click(); await f.harness.flush()
  assert.deepEqual(f.requests.map(value => [value.path, value.account]), [['/voice-transcription-jobs', 'account-a'], ['/agent-jobs/voice-job', 'account-a']])
  assert.equal(f.stopped, 1)
  assert.deepEqual(f.writes, ['转写内容'])
  assert.equal(f.harness.output.props.disabled, false)
  f.harness.unmount()
})

test('late transcription after a conversation switch cannot replace a draft', async () => {
  const jobResult = deferred(), f = await setup({ jobResult })
  await f.click(); await f.harness.flush(); await f.click(); await f.harness.flush()
  const upload = f.requests[0]
  f.harness.unmount()
  assert.equal(upload.options.signal.aborted, true)
  jobResult.resolve({ status: 'succeeded', output_json: JSON.stringify({ transcript: '旧会话的转写' }) })
  await f.harness.flush()
  assert.equal(f.writes.length, 0)
})

test('typing while transcription is pending is preserved when text is inserted', async () => {
  const jobResult = deferred(), f = await setup({ jobResult })
  await f.click(); await f.harness.flush(); await f.click(); await f.harness.flush()
  f.setDraft('刚输入的新问题')
  jobResult.resolve({ status: 'succeeded', output_json: JSON.stringify({ transcript: '录音转写' }) })
  await f.harness.flush()
  assert.deepEqual(f.writes, ['刚输入的新问题\n录音转写'])
  f.harness.unmount()
})
