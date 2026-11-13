import assert from 'node:assert/strict'
import { test } from 'node:test'
import { createComponentHarness, nodes, text } from './helpers/component-harness.mjs'

async function setup() {
  let owner = 'alice', threadId = 'one', draft = '正在编写的新草稿'
  const entries = new Map([['fasttask.agent-draft.v1:alice:thread:one:failed-messages', '["上次未确认的文本"]']])
  const listeners = new Set(), composerListeners = new Set()
  const thread = {
    getState: () => ({ state: { threadId } }),
    subscribe: listener => { listeners.add(listener); return () => listeners.delete(listener) },
    composer: {
      getState: () => ({ text: draft }),
      setText: value => { draft = value; for (const listener of composerListeners) listener() },
      subscribe: listener => { composerListeners.add(listener); return () => composerListeners.delete(listener) },
    },
  }
  const h = await createComponentHarness(new URL('../src/agent/FailedMessageRecovery.tsx', import.meta.url), 'FailedMessageRecovery', {
    mocks: { './ComposerDraft': { useComposerDraftUserId: () => owner } },
    globals: { Event, window: new EventTarget(), sessionStorage: { getItem: key => entries.get(key) ?? null, setItem: (key, value) => entries.set(key, value), removeItem: key => entries.delete(key) } },
  })
  h.render({ thread })
  return { h, entries, thread, get draft() { return draft },
    button: () => nodes(h.output, node => node.type === 'mdui-button')[0],
    changeOwner(value) { owner = value; h.render({ thread }) },
    changeThread(value) { threadId = value; for (const listener of listeners) listener() },
  }
}

test('recovering failed text never overwrites current or just-typed input', async () => {
  const f = await setup()
  assert.match(text(f.h.output), /上次未确认的文本/)
  assert.equal(f.button().props.disabled, true)
  f.button().props.onClick()
  assert.equal(f.draft, '正在编写的新草稿')
  f.thread.composer.setText(''); await f.h.flush()
  const click = f.button().props.onClick
  f.thread.composer.setText('点击前刚输入的内容'); click()
  assert.equal(f.draft, '点击前刚输入的内容')
  f.thread.composer.setText(''); await f.h.flush(); f.button().props.onClick(); await f.h.flush()
  assert.equal(f.draft, '上次未确认的文本')
  assert.equal(f.entries.size, 0)
  assert.equal(f.h.output, null)
  f.h.unmount()
})

test('queued recovery clicks cannot write text into another account or conversation', async () => {
  const f = await setup(); f.thread.composer.setText(''); await f.h.flush()
  const oldClick = f.button().props.onClick
  f.changeThread('two'); oldClick(); await f.h.flush()
  assert.equal(f.draft, '')
  assert.equal(f.h.output, null)
  f.changeThread('one'); await f.h.flush()
  const oldOwnerClick = f.button().props.onClick
  f.changeOwner('bob'); oldOwnerClick(); await f.h.flush()
  assert.equal(f.draft, '')
  assert.equal(f.h.output, null)
  assert.equal(f.entries.size, 1)
  f.h.unmount()
})
