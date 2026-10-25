import assert from 'node:assert/strict';
import { test } from 'node:test';
import { isLoopbackPanelPage, localNetworkPermission, nativeConfigMatchesOrigin, nativePanel, nativeStartCommand } from '../src/panel/native.ts';

test('a cloud account cannot treat the old local deployment as its paired source', () => {
  const config = { fasttask_url: 'http://127.0.0.1:10000' };
  assert.equal(nativeConfigMatchesOrigin(config, 'https://tasks.example.com'), false);
  assert.equal(nativeConfigMatchesOrigin({ fasttask_url: 'https://tasks.example.com/' }, 'https://tasks.example.com'), true);
  for (const suffix of ['/other', '?source=old', '#old']) assert.equal(nativeConfigMatchesOrigin({ fasttask_url: `https://tasks.example.com${suffix}` }, 'https://tasks.example.com'), false);
  assert.equal(nativeConfigMatchesOrigin({ fasttask_url: 'https://old@tasks.example.com' }, 'https://tasks.example.com'), false);
  assert.equal(nativeConfigMatchesOrigin({}, 'https://tasks.example.com'), false);
  assert.equal(isLoopbackPanelPage('https://tasks.example.com'), false);
  assert.equal(isLoopbackPanelPage('https://127.0.0.1.attacker.test'), false);
  assert.equal(isLoopbackPanelPage('http://127.0.0.1:10000'), true);
  assert.match(nativeStartCommand('https://tasks.example.com'), /-Origin 'https:\/\/tasks\.example\.com'/);
});

test('helper fetches always target this computer and never attach web credentials or follow redirects', async t => {
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, options) => { requests.push({ url, options }); return Response.json({ ok: true }); });
  await nativePanel('/config', { fasttask_url: 'https://tasks.example.com', panel_token: 'new-cloud-read-only-token' }, 'PUT');
  assert.equal(requests.length, 1);
  assert.equal(requests[0].url, 'http://127.0.0.1:19743/config');
  assert.equal(requests[0].options.targetAddressSpace, 'loopback');
  assert.equal(requests[0].options.credentials, 'omit');
  assert.equal(requests[0].options.redirect, 'error');
  assert.equal(JSON.parse(requests[0].options.body).fasttask_url, 'https://tasks.example.com');
});

test('network errors do not falsely identify CORS failures as an offline helper', async t => {
  t.mock.method(globalThis, 'fetch', async () => { throw new TypeError('Failed to fetch'); });
  await assert.rejects(nativePanel('/health'), error => error.code === 'unreachable' && /启动来源不匹配/.test(error.message) && /本地网络/.test(error.message));
});

test('unknown JSON or structured error payloads never become raw UI output', async t => {
  const fetch = t.mock.method(globalThis, 'fetch', async () => Response.json(null));
  await assert.rejects(nativePanel('/health'), error => error.code === 'invalid_response');
  fetch.mock.mockImplementation(async () => Response.json({ ok: false, error: { secret: 'never-display' }, message: { internal: 'never-display' } }, { status: 500 }));
  await assert.rejects(nativePanel('/health'), error => !error.message.includes('never-display') && /500/.test(error.message));
});

test('network permission queries prefer loopback and fall back without requesting permission', async t => {
  const calls = [];
  const before = Object.getOwnPropertyDescriptor(navigator, 'permissions');
  Object.defineProperty(navigator, 'permissions', { configurable: true, value: { query: async ({ name }) => { calls.push(name); if (name === 'loopback-network') throw new TypeError('unsupported'); return { state: 'denied' }; } } });
  t.after(() => { if (before) Object.defineProperty(navigator, 'permissions', before); else delete navigator.permissions; });
  assert.equal(await localNetworkPermission(), 'denied');
  assert.deepEqual(calls, ['loopback-network', 'local-network-access']);
});
