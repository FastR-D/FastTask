import assert from 'node:assert/strict'
import { test } from 'node:test'
import { clearComposerDrafts, moveFailedMessages, preserveFailedMessage, readFailedMessages, removeFailedMessage, writeComposerDraft, readComposerDraft } from '../src/agent/draft-storage.ts'

function store() {
  const entries = new Map()
  return { get length() { return entries.size }, key: index => [...entries.keys()][index] ?? null, getItem: key => entries.get(key) ?? null, setItem: (key, value) => entries.set(key, value), removeItem: key => entries.delete(key) }
}

test('failed sends remain separate from drafts, accounts and conversations, including repeated failures', () => {
  globalThis.sessionStorage = store(); clearComposerDrafts()
  writeComposerDraft('alice', 'one', '继续写的新草稿')
  preserveFailedMessage('alice', 'one', '未确认的原消息')
  preserveFailedMessage('alice', 'one', '另一条未确认消息')
  preserveFailedMessage('alice', 'one', '未确认的原消息')
  assert.equal(readComposerDraft('alice', 'one'), '继续写的新草稿')
  assert.deepEqual(readFailedMessages('alice', 'one'), ['未确认的原消息', '另一条未确认消息'])
  assert.deepEqual(readFailedMessages('bob', 'one'), [])
  assert.deepEqual(readFailedMessages('alice', 'two'), [])
  removeFailedMessage('alice', 'one', '未确认的原消息')
  assert.deepEqual(readFailedMessages('alice', 'one'), ['另一条未确认消息'])
  clearComposerDrafts()
})

test('first server thread assignment carries preserved failures with the conversation', () => {
  globalThis.sessionStorage = store(); clearComposerDrafts()
  preserveFailedMessage('alice', undefined, '新会话的未确认消息')
  moveFailedMessages('alice', 'server-thread')
  assert.deepEqual(readFailedMessages('alice', undefined), [])
  assert.deepEqual(readFailedMessages('alice', 'server-thread'), ['新会话的未确认消息'])
  clearComposerDrafts()
})

test('blocked browser storage does not lose a failed message and logout clears the in-memory copy', () => {
  globalThis.sessionStorage = { getItem() { throw new Error('blocked') }, setItem() { throw new Error('blocked') }, get length() { throw new Error('blocked') } }
  clearComposerDrafts()
  preserveFailedMessage('alice', 'one', '仅本次会话保留')
  assert.deepEqual(readFailedMessages('alice', 'one'), ['仅本次会话保留'])
  clearComposerDrafts()
  assert.deepEqual(readFailedMessages('alice', 'one'), [])
})
