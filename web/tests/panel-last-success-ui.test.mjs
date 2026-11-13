import assert from 'node:assert/strict';
import { test } from 'node:test';
import { rememberPanelDevice } from '../src/panel/history.ts';
import { loadBrowserPanelSuccess, rememberBrowserPanelSuccess } from '../src/panel/last-success.ts';
import { createComponentHarness, deferred, eventTarget, nodes, text } from './helpers/component-harness.mjs';

const origin = 'http://127.0.0.1:10000';
const nativeDevice = { name: 'WL00112233', address: '66:66:00:11:22:33' };
const browserDevice = { name: nativeDevice.name, id: 'opaque-browser-device' };
const profile = { display_name: '测试用户', seat: '', title: '今日任务', avatar_data_url: '', weather_override: '', task_overrides: {} };
const pairing = { id: 'pair-alice', device_name: nativeDevice.name, expires_at: '2099-01-01T00:00:00Z' };
const health = { ok: true, version: 'test', ble_supported: true, sync_mode: 'on_change', capabilities: { guarded_refresh: true }, config: { enabled: true, has_token: true, fasttask_url: origin, pairing_id: pairing.id, device_name: nativeDevice.name, address: nativeDevice.address, page: 0 }, monitor: { last_synced_at: '2026-11-11T16:23:45Z', interval_seconds: 30, pending: false } };
function memoryStorage() {
  const values = new Map();
  return { values, getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
}

async function fixture(options = {}) {
  const storage = options.storage ?? memoryStorage(), transport = options.transport ?? 'browser';
  rememberPanelDevice(origin, 'alice', { transport, ...(transport === 'native' ? nativeDevice : browserDevice) }, storage);
  let currentHealth = structuredClone(options.health ?? health), sendImage = options.sendImage ?? (() => Promise.resolve({ deviceName: browserDevice.name }));
  let now = Date.parse('2026-11-12T08:00:00Z');
  class ClockDate extends Date { constructor(...args) { super(...(args.length ? args : [now])); } static now() { return now; } }
  class BluetoothClient {
    connected = false;
    disconnect() { this.connected = false; }
    async connect() { this.connected = true; return browserDevice; }
    async connectRemembered() { return this.connect(); }
    sendImage() { return sendImage(); }
  }
  const window = eventTarget({ location: { origin, protocol: 'http:' }, isSecureContext: true, setInterval: () => 1, clearInterval() {} });
  const document = eventTarget({ visibilityState: 'visible' });
  const harness = await createComponentHarness(new URL('../src/FastPanel.tsx', import.meta.url), 'FastPanel', {
    mocks: {
      './api': { ApiError: class extends Error {}, idem: () => 'request-id', token: { get: () => 'access-token' }, request: async (path, init, owner) => {
        if (path === '/panel/profile') return { data: profile, etag: '"1"' };
        if (path === '/daily-plans/current') return { data: { plan: { local_date: '2026-11-12' }, today_items: [] } };
        if (path === '/panel/pairings') return { data: { items: owner === 'alice' ? options.pairings ?? [pairing] : [] } };
        throw new Error(`Unexpected request: ${path}`);
      } },
      './session-owner': { assertAccessOwner() {} },
      './panel/bluetooth': { getBluetoothCapability: () => ({ supported: true }), WolinkPanelClient: BluetoothClient },
      './panel/badge': { EMPTY_PROFILE: profile, paginateBadge: () => [[]], drawBadge: () => ({}), prepareAvatar: async () => '' },
      './panel/native': { isLoopbackPanelPage: () => true, localNetworkPermission: async () => 'granted', nativeConfigMatchesOrigin: (config, expected) => config.fasttask_url === expected, NativePanelError: class extends Error {}, nativeStartCommand: () => 'start helper', nativePanel: async path => {
        if (path === '/health') return currentHealth;
        throw new Error(`Unexpected native request: ${path}`);
      } },
    },
    globals: { Date: ClockDate, DOMException, Error, window, document, localStorage: storage, navigator: {}, fetch: async () => ({ ok: true, json: async () => ({ local_date: '2026-11-12', status: 'fresh', summary: '晴' }) }) },
    commit(tree) { for (const node of nodes(tree, node => node.type === 'canvas')) node.props.ref.current = {}; },
  });
  const props = { active: true, userId: 'alice', timezone: 'Asia/Shanghai', refreshVersion: 0, onNotice() {} };
  harness.render(props); await harness.flush();
  return { harness, props, storage, window,
    success: () => text(nodes(harness.output, node => node.props.className === 'panel-last-success')[0]),
    click: label => { const button = nodes(harness.output, node => node.type === 'mdui-button' && text(node) === label)[0]; assert.ok(button, `Missing button: ${label}`); assert.equal(Boolean(button.props.disabled), false, `Disabled button: ${label}`); button.props.onClick(); },
    setSend: fn => { sendImage = fn; }, setNow: timestamp => { now = Date.parse(timestamp); }, setHealth: value => { currentHealth = structuredClone(value); },
  };
}

test('browser success is saved only after acknowledgement, survives reload, and a failure preserves it', async t => {
  const pending = deferred(), f = await fixture({ sendImage: () => pending.promise });
  t.after(() => f.harness.unmount());
  assert.equal(f.success(), '上次成功更新暂无成功更新记录');
  f.click('重新连接设备'); await f.harness.flush();
  f.click('更新墨水屏'); await f.harness.flush();
  assert.equal(loadBrowserPanelSuccess(origin, 'alice', browserDevice.id, f.storage), '');
  f.setNow('2026-11-12T08:02:03Z'); pending.resolve({ deviceName: browserDevice.name }); await f.harness.flush();
  assert.equal(f.success(), '上次成功更新2026-11-12 16:02:03');
  assert.equal(loadBrowserPanelSuccess(origin, 'alice', browserDevice.id, f.storage), '2026-11-12T08:02:03.000Z');
  f.setSend(async () => { throw new Error('蓝牙连接中断'); }); f.setNow('2026-11-12T08:05:00Z');
  f.click('更新墨水屏'); await f.harness.flush();
  assert.equal(f.success(), '上次成功更新2026-11-12 16:02:03');
  assert.match(text(f.harness.output), /蓝牙连接中断/);
  const reloaded = await fixture({ storage: f.storage }); t.after(() => reloaded.harness.unmount());
  assert.equal(reloaded.success(), '上次成功更新2026-11-12 16:02:03');
  assert.match(text(reloaded.harness.output), /重新连接设备/);
});

test('a late browser acknowledgement after account change cannot save a success for either account', async t => {
  const pending = deferred(), f = await fixture({ sendImage: () => pending.promise });
  t.after(() => f.harness.unmount());
  f.click('重新连接设备'); await f.harness.flush(); f.click('更新墨水屏'); await f.harness.flush();
  f.harness.render({ ...f.props, userId: 'bob' }); await f.harness.flush();
  pending.resolve({ deviceName: browserDevice.name }); await f.harness.flush();
  assert.equal(loadBrowserPanelSuccess(origin, 'alice', browserDevice.id, f.storage), '');
  assert.equal(loadBrowserPanelSuccess(origin, 'bob', browserDevice.id, f.storage), '');
  assert.equal(f.success(), '上次成功更新暂无成功更新记录');
});

test('native timestamps use the verified account pairing and cannot fall back to browser history', async t => {
  const storage = memoryStorage();
  rememberBrowserPanelSuccess(origin, 'alice', browserDevice.id, '2026-11-12T07:00:00Z', storage);
  const f = await fixture({ transport: 'native', storage }); t.after(() => f.harness.unmount());
  assert.equal(f.success(), '上次成功更新2026-11-12 00:23:45');
  f.setHealth({ ...health, monitor: { ...health.monitor, last_checked_at: '2026-11-12T08:10:00Z' }, last_status: { status: 'failed', message: '设备未回应' } });
  f.window.dispatch('focus'); await f.harness.flush();
  assert.equal(f.success(), '上次成功更新2026-11-12 00:23:45');
  f.harness.render({ ...f.props, userId: 'bob' }); await f.harness.flush();
  assert.equal(f.success(), '上次成功更新暂无成功更新记录');
  const otherOrigin = await fixture({ transport: 'native', health: { ...health, config: { ...health.config, fasttask_url: 'https://other.example.org' } } });
  t.after(() => otherOrigin.harness.unmount());
  assert.equal(otherOrigin.success(), '上次成功更新暂无成功更新记录');
});

test('a connected device without a success record remains connected and ready to update', async t => {
  const f = await fixture({ transport: 'native', health: { ...health, monitor: { interval_seconds: 30, pending: false } } });
  t.after(() => f.harness.unmount());
  assert.equal(f.success(), '上次成功更新暂无成功更新记录');
  const card = nodes(f.harness.output, node => node.props.className === 'panel-section panel-connection')[0];
  assert.match(text(card), /已连接/);
  const button = nodes(card, node => node.type === 'mdui-button' && text(node) === '更新墨水屏')[0];
  assert.equal(Boolean(button.props.disabled), false);
});
