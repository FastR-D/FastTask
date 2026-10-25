import assert from 'node:assert/strict';
import { registerHooks } from 'node:module';
import { test } from 'node:test';
import { inflateRawSync } from 'node:zlib';

// Node 25 strips TypeScript directly; resolve Vite-style extensionless imports.
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (context.parentURL?.includes('/src/panel/') && specifier.startsWith('./') && !/\.[a-z]+$/i.test(specifier)) {
      return nextResolve(`${specifier}.ts`, context);
    }
    return nextResolve(specifier, context);
  },
});
const protocol = await import('../src/panel/protocol.ts');
const { WolinkPanelClient, getBluetoothCapability } = await import('../src/panel/bluetooth.ts');
const { matchingPanelPairing } = await import('../src/panel/pairing.ts');

test('native token reuse requires the current account pairing, device, expiry and exact origin', () => {
  const origin = 'https://tasks.example.org';
  const now = Date.parse('2026-10-25T12:00:00+08:00');
  const config = { enabled: true, device_name: 'WL14B00B1F', address: '66:66:14:B0:0B:1F', page: 0, has_token: true, pairing_id: 'mine', fasttask_url: origin };
  const pairing = { id: 'mine', device_name: config.device_name, expires_at: '2027-01-01T00:00:00+08:00' };
  assert.equal(matchingPanelPairing(config, [pairing], origin, now), pairing);
  assert.equal(matchingPanelPairing(config, [], origin, now), undefined, 'a different signed-in account must not inherit the local token');
  assert.equal(matchingPanelPairing(config, [{ ...pairing, id: 'other' }], origin, now), undefined);
  for (const override of [{ has_token: false }, { pairing_id: '' }, { device_name: 'WL00000000' }, { fasttask_url: 'https://other.example.org' }, { fasttask_url: `${origin}/foreign` }]) {
    assert.equal(matchingPanelPairing({ ...config, ...override }, [pairing], origin, now), undefined);
  }
  for (const override of [{ revoked_at: '2026-10-25T11:00:00+08:00' }, { expires_at: '2026-10-25T12:00:00+08:00' }, { expires_at: 'bad-date' }]) {
    assert.equal(matchingPanelPairing(config, [{ ...pairing, ...override }], origin, now), undefined);
  }
});

test('AES CBC matches the unpadded Python reference vector, exactly 16 bytes', async () => {
  const challenge = Uint8Array.from(Buffer.from('D81DE1825F92088636CB739D4D930A3E', 'hex'));
  const response = await protocol.encryptChallenge(challenge);
  assert.equal(response.length, 16);
  assert.equal(protocol.bytesToHex(response), 'C1D14423974D4E304001A94CC3C40D80');
  await assert.rejects(protocol.encryptChallenge(new Uint8Array(15)), /16/);
});

test('800 × 480 pixels yield two MSB-first black/white/red planes totaling 96000 bytes', () => {
  const data = new Uint8ClampedArray(800 * 480 * 4).fill(255);
  const pixels = [[0, 0, 0], [255, 255, 255], [255, 0, 0], [0, 0, 0],
    [255, 255, 255], [255, 0, 0], [255, 255, 255], [0, 0, 0]];
  pixels.forEach((rgb, index) => data.set([...rgb, 255], index * 4));
  const raw = protocol.imageDataToRaw({ width: 800, height: 480, data });
  assert.equal(raw.length, 96000);
  assert.equal(raw[0], 0b01101110);
  assert.equal(raw[48000], 0b00100100);
  assert.ok(raw.subarray(1, 48000).every((byte) => byte === 255));
  assert.ok(raw.subarray(48001).every((byte) => byte === 0));
  assert.throws(() => protocol.imageDataToRaw({ width: 480, height: 800, data }), /800/);
});

test('transparent canvas pixels are composited on physical white', () => {
  const raw = protocol.imageDataToRaw({ width: 800, height: 480, data: new Uint8ClampedArray(800 * 480 * 4) });
  assert.ok(raw.subarray(0, 48000).every((byte) => byte === 255));
  assert.ok(raw.subarray(48000).every((byte) => byte === 0));
});

test('image stream contains 12 independently raw-DEFLATE-compressed blocks', async () => {
  const raw = Uint8Array.from({ length: 96000 }, (_, i) => i % 251);
  const stream = await protocol.buildImageStream(raw);
  assert.deepEqual(Array.from(stream.subarray(0, 4)), [0xa5, 0xa6, 0x0c, 0x02]);
  const view = new DataView(stream.buffer);
  let offset = 4;
  let rawOffset = 0;
  for (let id = 1; id <= 12; id++) {
    assert.equal(stream[offset], id);
    const length = view.getUint16(offset + 1, true);
    const unpacked = inflateRawSync(stream.subarray(offset + 3, offset + 3 + length));
    assert.deepEqual(Uint8Array.from(unpacked), raw.subarray(rawOffset, rawOffset + 8192));
    assert.equal(unpacked.length, id === 12 ? 5888 : 8192);
    offset += 3 + length;
    rawOffset += unpacked.length;
  }
  assert.equal(offset, stream.length);
  assert.equal(rawOffset, 96000);
  await assert.rejects(protocol.buildImageStream(new Uint8Array(48000)), /96000/);
});

test('packet wrapper uses 238-byte payload and little-endian offsets/end length', () => {
  const chunk = Uint8Array.from({ length: 238 }, (_, i) => i);
  const packet = protocol.buildDataPacket(238, chunk);
  assert.equal(packet.length, 244);
  assert.deepEqual(Array.from(packet.subarray(0, 6)), [0, 0xa5, 238, 0, 0, 0]);
  assert.deepEqual(packet.subarray(6), chunk);
  assert.deepEqual(Array.from(protocol.buildEndPacket(272)), [2, 0xa5, 16, 1, 0, 0]);
  assert.throws(() => protocol.buildDataPacket(-1, chunk));
  assert.throws(() => protocol.buildEndPacket(0x100000000));
});

class Characteristic extends EventTarget {
  properties = { write: true, notify: true, indicate: false };
  value;
  writes = [];
  notifications = false;
  onWrite;
  async readValue() {
    return new DataView(Uint8Array.from(Buffer.from('D81DE1825F92088636CB739D4D930A3E', 'hex')).buffer);
  }
  async writeValueWithResponse(buffer) {
    const bytes = new Uint8Array(buffer).slice();
    this.writes.push(bytes);
    await this.onWrite?.(bytes);
  }
  async startNotifications() { this.notifications = true; return this; }
  async stopNotifications() { this.notifications = false; return this; }
  notify(bytes) {
    this.value = new DataView(Uint8Array.from(bytes).buffer);
    this.dispatchEvent(new Event('characteristicvaluechanged'));
  }
}

function mockScreen(options = {}) {
  const auth = new Characteristic();
  const writer = new Characteristic();
  const done = new Characteristic();
  const device = new EventTarget();
  device.id = 'browser-opaque-id-not-mac';
  device.name = options.name ?? 'WL14B00B1F';
  device.gatt = {
    connected: false,
    async connect() { this.connected = true; return this; },
    disconnect() {
      if (!this.connected) return;
      this.connected = false;
      device.dispatchEvent(new Event('gattserverdisconnected'));
    },
    async getPrimaryService(uuid) {
      assert.equal(uuid, protocol.SERVICE_UUID);
      return {
        async getCharacteristic(characteristic) {
          const chars = { [protocol.AUTH_UUID]: auth, [protocol.IMAGE_WRITE_UUID]: writer, [protocol.DONE_NOTIFY_UUID]: done };
          assert.ok(chars[characteristic]);
          return chars[characteristic];
        },
      };
    },
  };
  writer.onWrite = async (bytes) => {
    assert.ok(done.notifications, 'subscribe to notifications before writing the image');
    if (bytes[0] === 2 && options.confirm !== false) {
      done.notify(options.notification ?? protocol.SUCCESS_NOTIFICATION);
    }
    await options.onWrite?.(bytes);
  };
  const requests = [];
  Object.defineProperty(globalThis, 'window', { configurable: true, value: { isSecureContext: true } });
  Object.defineProperty(globalThis, 'navigator', {
    configurable: true,
    value: { bluetooth: { requestDevice(request) { requests.push(request); return Promise.resolve(device); } } },
  });
  return { auth, writer, done, device, requests };
}

test('Web Bluetooth chooser is synchronous, suffix matches broadcast name, service is optional', async () => {
  const screen = mockScreen();
  assert.equal(getBluetoothCapability().supported, true);
  const client = new WolinkPanelClient();
  const connection = client.connect('14b00b1f');
  assert.equal(screen.requests.length, 1, 'chooser must run in the caller click stack');
  assert.deepEqual(screen.requests[0].filters, [{ name: 'WL14B00B1F' }]);
  assert.deepEqual(screen.requests[0].optionalServices, [protocol.SERVICE_UUID]);
  assert.deepEqual(await connection, { id: screen.device.id, name: 'WL14B00B1F' });
  assert.equal(client.connected, true);
  client.disconnect();
  assert.equal(client.connected, false);
});

test('broad chooser still rejects a device whose name mismatches the supplied ID', async () => {
  const screen = mockScreen({ name: 'WLWRONG' });
  const client = new WolinkPanelClient();
  await assert.rejects(client.connect('WL14B00B1F', { acceptAllDevices: true }), /不一致/);
  assert.equal(screen.requests[0].acceptAllDevices, true);
  assert.equal(client.connected, false);
});

test('only the exact success notification confirms an authenticated image transfer', async () => {
  const screen = mockScreen();
  const client = new WolinkPanelClient();
  await client.connect('WL14B00B1F');
  const phases = [];
  const result = await client.sendRawImage(new Uint8Array(96000), { onProgress: (p) => phases.push(p.phase) });
  assert.equal(result.notificationHex, 'FF00000000000000');
  assert.equal(protocol.bytesToHex(screen.auth.writes[0]), 'C1D14423974D4E304001A94CC3C40D80');
  assert.equal(phases.at(-1), 'done');
  assert.equal(result.packets, screen.writer.writes.length);
  const payloads = screen.writer.writes.slice(0, -1);
  let offset = 0;
  for (const packet of payloads) {
    assert.equal(packet[0], 0);
    assert.equal(new DataView(packet.buffer).getUint32(2, true), offset);
    assert.ok(packet.length <= 244);
    offset += packet.length - 6;
  }
  assert.equal(offset, result.streamBytes);
  assert.deepEqual(screen.writer.writes.at(-1), protocol.buildEndPacket(offset));
  assert.equal(screen.done.notifications, false);
  client.disconnect();
});

for (const [label, config] of [
  ['missing', { confirm: false }],
  ['truncated', { notification: [0xff, 0] }],
  ['failure', { notification: [0xff, 1, 0, 0, 0, 0, 0, 0] }],
]) {
  test(`${label} completion notification must fail, never report success`, async () => {
    mockScreen(config);
    const client = new WolinkPanelClient();
    await client.connect('WL14B00B1F');
    const phases = [];
    await assert.rejects(client.sendRawImage(new Uint8Array(96000), {
      confirmationTimeoutMs: 20, onProgress: (p) => phases.push(p.phase),
    }), /未收到墨水屏刷新完成确认/);
    assert.ok(!phases.includes('done'));
    assert.equal(client.connected, false);
  });
}

test('canceling an in-flight GATT write disconnects and prevents later writes', async () => {
  const cancellation = new AbortController();
  let started;
  const firstWrite = new Promise((resolve) => { started = resolve; });
  const screen = mockScreen({
    onWrite(bytes) { if (bytes[0] === 0) { started(); return new Promise(() => {}); } },
  });
  const client = new WolinkPanelClient();
  await client.connect();
  const sending = client.sendRawImage(new Uint8Array(96000), { signal: cancellation.signal });
  await firstWrite;
  cancellation.abort();
  await assert.rejects(sending, { name: 'AbortError' });
  assert.equal(screen.writer.writes.length, 1);
  assert.equal(client.connected, false);
  assert.equal(screen.device.gatt.connected, false);
});

test('physical disconnection aborts an in-flight transfer and notifies the UI', async () => {
  let disconnections = 0;
  const screen = mockScreen({ onWrite(bytes) { if (bytes[0] === 0) screen.device.gatt.disconnect(); } });
  const client = new WolinkPanelClient({ onDisconnected: () => disconnections++ });
  await client.connect();
  await assert.rejects(client.sendRawImage(new Uint8Array(96000)), { name: 'AbortError' });
  assert.equal(disconnections, 1);
  assert.equal(client.connected, false);
  assert.equal(screen.writer.writes.length, 1);
});

test('leaving while the Windows GATT connection is pending closes a late connection', async () => {
  const screen = mockScreen();
  let finishConnection;
  let connectionStarted;
  const started = new Promise((resolve) => { connectionStarted = resolve; });
  screen.device.gatt.connect = () => {
    connectionStarted();
    return new Promise((resolve) => { finishConnection = () => {
      screen.device.gatt.connected = true;
      resolve(screen.device.gatt);
    }; });
  };
  const client = new WolinkPanelClient();
  const connecting = client.connect();
  await started;
  client.disconnect();
  await assert.rejects(connecting, { name: 'AbortError' });
  finishConnection();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(screen.device.gatt.connected, false);
});
