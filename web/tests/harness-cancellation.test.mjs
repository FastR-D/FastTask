import assert from 'node:assert/strict';
import { test } from 'node:test';
import { requestRunCancellation } from '../src/harness/cancellation.ts';

test('sidecar and restored runs cancel on the server without a local instance', async () => {
  const calls = [];
  await requestRunCancellation('sidecar-restored', async id => calls.push(id), () => {});
  assert.deepEqual(calls, ['sidecar-restored']);
});

test('failed delivery aborts the local host and preserves the error for retry feedback', async () => {
  let aborted = false;
  const failure = new TypeError('Failed to fetch');
  await assert.rejects(requestRunCancellation('wasm-run', async () => { throw failure; }, () => { aborted = true; }), error => error === failure);
  assert.equal(aborted, true);
});

test('an unknown run does not cancel another host', async () => {
  await requestRunCancellation(undefined, async () => assert.fail('unexpected request'), () => assert.fail('unexpected local abort'));
});
