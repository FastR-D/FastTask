import assert from 'node:assert/strict';
import { test } from 'node:test';
import { formatPanelLastSuccess, loadBrowserPanelSuccess, nativePanelLastSuccess, rememberBrowserPanelSuccess } from '../src/panel/last-success.ts';

const origin = 'https://tasks.example.org';
const acknowledgedAt = '2026-11-12T08:23:45.123Z';
const device = { name: 'WL00112233', address: '66:66:00:11:22:AA' };
const pairing = { id: 'pairing-alice', device_name: device.name, expires_at: '2099-01-01T00:00:00Z' };
const health = { config: { has_token: true, pairing_id: pairing.id, device_name: device.name, address: device.address }, monitor: { last_synced_at: acknowledgedAt, last_checked_at: '2026-11-12T09:00:00Z' } };
function memoryStorage() {
  const values = new Map();
  return { values, getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
}

test('native success requires a verified current pairing and the exact selected device', () => {
  assert.equal(nativePanelLastSuccess(health, device, pairing), acknowledgedAt);
  assert.equal(nativePanelLastSuccess(health, { ...device, address: device.address.toLowerCase() }, pairing), acknowledgedAt);
  assert.equal(nativePanelLastSuccess(health, device, undefined), '');
  assert.equal(nativePanelLastSuccess(health, null, pairing), '');
  assert.equal(nativePanelLastSuccess(health, { ...device, address: '66:66:00:11:22:BB' }, pairing), '');
  assert.equal(nativePanelLastSuccess(health, { ...device, name: 'WLOTHER' }, pairing), '');
  assert.equal(nativePanelLastSuccess(health, device, { ...pairing, id: 'pairing-bob' }), '');
  assert.equal(nativePanelLastSuccess({ ...health, config: { ...health.config, has_token: false } }, device, pairing), '');
});

test('native polls, queued updates, and failed sends never become a new success', () => {
  assert.equal(nativePanelLastSuccess({ ...health, monitor: { last_checked_at: acknowledgedAt, pending: true }, last_status: { status: 'success', finished_at: acknowledgedAt } }, device, pairing), '');
  assert.equal(nativePanelLastSuccess({ ...health, last_status: { status: 'failed', finished_at: '2026-11-12T10:00:00Z' } }, device, pairing), acknowledgedAt);
});

test('browser acknowledgement survives reload and stays isolated by origin, account, and device', () => {
  const storage = memoryStorage();
  assert.equal(rememberBrowserPanelSuccess(origin, 'alice', 'device-1', acknowledgedAt, storage), acknowledgedAt);
  assert.equal(loadBrowserPanelSuccess(origin, 'alice', 'device-1', storage), acknowledgedAt);
  assert.equal(loadBrowserPanelSuccess(origin, 'bob', 'device-1', storage), '');
  assert.equal(loadBrowserPanelSuccess(origin, 'alice', 'device-2', storage), '');
  assert.equal(loadBrowserPanelSuccess('https://other.example.org', 'alice', 'device-1', storage), '');
  assert.equal(loadBrowserPanelSuccess(origin, '', 'device-1', storage), '');
  assert.equal(storage.values.size, 1);
  assert.equal([...storage.values.values()][0], acknowledgedAt);
});

test('invalid success records never overwrite a previously confirmed update', () => {
  const storage = memoryStorage();
  rememberBrowserPanelSuccess(origin, 'alice', 'device-1', acknowledgedAt, storage);
  for (const invalid of ['', 'not a date', '2026-11-12', '2026-11-12T08:23:45', '2026-99-99T08:23:45Z']) {
    assert.equal(rememberBrowserPanelSuccess(origin, 'alice', 'device-1', invalid, storage), '');
    assert.equal(loadBrowserPanelSuccess(origin, 'alice', 'device-1', storage), acknowledgedAt);
  }
  storage.setItem([...storage.values.keys()][0], '{bad json}');
  assert.equal(loadBrowserPanelSuccess(origin, 'alice', 'device-1', storage), '');
});

test('blocked storage still returns this session success without blocking the device', () => {
  const storage = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); } };
  assert.equal(loadBrowserPanelSuccess(origin, 'alice', 'device-1', storage), '');
  assert.equal(rememberBrowserPanelSuccess(origin, 'alice', 'device-1', acknowledgedAt, storage), acknowledgedAt);
});

test('success times include the local date and follow the user timezone across midnight', () => {
  const timestamp = '2026-11-12T16:23:45+00:00';
  assert.equal(formatPanelLastSuccess(timestamp, 'Asia/Shanghai'), '2026-11-13 00:23:45');
  assert.equal(formatPanelLastSuccess(timestamp, 'America/New_York'), '2026-11-12 11:23:45');
  assert.equal(formatPanelLastSuccess(timestamp, 'UTC'), '2026-11-12 16:23:45');
  assert.equal(formatPanelLastSuccess(timestamp, ''), '2026-11-13 00:23:45');
  assert.equal(formatPanelLastSuccess('invalid', 'UTC'), '');
});
