import assert from 'node:assert/strict';
import { test } from 'node:test';
import { loadPanelDeviceHistory, rememberPanelDevice } from '../src/panel/history.ts';

function memoryStorage() {
  const values = new Map();
  return { values, getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
}
const origin = 'https://tasks.example.org';
const device = { transport: 'native', name: 'WL00112233', address: '66:66:00:11:22:33' };

test('device history is isolated by server origin and signed-in owner', () => {
  const storage = memoryStorage();
  rememberPanelDevice(origin, 'alice', device, storage);
  assert.equal(loadPanelDeviceHistory(origin, 'alice', storage)[0].name, device.name);
  assert.deepEqual(loadPanelDeviceHistory(origin, 'bob', storage), []);
  assert.deepEqual(loadPanelDeviceHistory('https://other.example.org', 'alice', storage), []);
  assert.deepEqual(loadPanelDeviceHistory(origin, '', storage), []);
  rememberPanelDevice(origin, '', device, storage);
  assert.equal(storage.values.size, 1);
});

test('history keeps ten recent devices, deduplicates and restores the last device first', () => {
  const storage = memoryStorage();
  for (let index = 0; index < 12; index++) {
    rememberPanelDevice(origin, 'alice', { transport: 'browser', name: `WLDEVICE${index}`, id: `opaque-${index}` }, storage, new Date(2026, 9, 25, 12, index));
  }
  const recent = rememberPanelDevice(origin, 'alice', { transport: 'browser', name: 'WLDEVICE3', id: 'opaque-3' }, storage, new Date(2026, 9, 25, 13));
  assert.equal(recent.length, 10);
  assert.equal(recent[0].id, 'opaque-3');
  assert.equal(recent.filter(value => value.id === 'opaque-3').length, 1);
  assert.equal(recent.some(value => value.id === 'opaque-0'), false);
  assert.deepEqual(loadPanelDeviceHistory(origin, 'alice', storage), recent);
});

test('history strips tokens, avatars and unknown fields, and drops malformed targets', () => {
  const storage = memoryStorage();
  rememberPanelDevice(origin, 'alice', { ...device, panel_token: 'secret', avatar_data_url: 'photo' }, storage);
  const key = [...storage.values.keys()][0];
  const saved = JSON.parse(storage.getItem(key));
  storage.setItem(key, JSON.stringify([...saved, { ...saved[0], name: '<script>', panel_token: 'secret' }, { ...saved[0], name: 'WLINVALID', address: 'http://wrong' }]));
  assert.equal(loadPanelDeviceHistory(origin, 'alice', storage).length, 1);
  assert.deepEqual(Object.keys(saved[0]).sort(), ['address', 'last_used_at', 'name', 'transport']);
  assert.equal(JSON.stringify(saved).includes('secret'), false);
  assert.equal(JSON.stringify(saved).includes('photo'), false);
});

test('corrupt or unavailable storage cannot block connecting a device in this session', () => {
  const unavailable = { getItem() { throw new Error('blocked'); }, setItem() { throw new Error('blocked'); } };
  assert.deepEqual(loadPanelDeviceHistory(origin, 'alice', unavailable), []);
  assert.equal(rememberPanelDevice(origin, 'alice', device, unavailable).length, 1);
  assert.deepEqual(loadPanelDeviceHistory(origin, 'alice', { getItem: () => 'not json', setItem() {} }), []);
});
