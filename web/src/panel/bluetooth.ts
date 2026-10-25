import type {
  PanelBluetooth, PanelBluetoothCharacteristic, PanelBluetoothDevice,
} from './bluetooth-types';
import {
  AUTH_UUID, DONE_NOTIFY_UUID, IMAGE_WRITE_UUID, SERVICE_UUID, WRITE_PAYLOAD_SIZE,
  buildDataPacket, buildEndPacket, buildImageStream, bytesToHex,
  encryptChallenge, imageDataToRaw, isSuccessNotification,
} from './protocol';

export interface BluetoothCapability { supported: boolean; reason: string }
export interface PanelDevice { id: string; name: string }
export interface TransferProgress {
  phase: 'preparing' | 'authenticating' | 'sending' | 'waiting' | 'done';
  sentPackets: number;
  totalPackets: number;
  percent: number;
}
export interface TransferOptions {
  signal?: AbortSignal;
  onProgress?: (progress: TransferProgress) => void;
  confirmationTimeoutMs?: number;
}
export interface TransferResult {
  deviceName: string;
  streamBytes: number;
  packets: number;
  notificationHex: string;
}

function bluetooth(): PanelBluetooth | undefined {
  return (navigator as Navigator & { bluetooth?: PanelBluetooth }).bluetooth;
}

export function getBluetoothCapability(): BluetoothCapability {
  if (!window.isSecureContext) {
    return { supported: false, reason: '蓝牙需要安全页面，请使用本机 http://localhost、http://127.0.0.1 或 HTTPS。' };
  }
  if (!bluetooth()) {
    return { supported: false, reason: '当前浏览器没有 Web Bluetooth，请在 Windows 的 Chrome 或 Edge 中打开此页面。' };
  }
  if (!crypto.subtle) return { supported: false, reason: '浏览器无法执行墨水屏认证，请更新 Chrome 或 Edge。' };
  try {
    new CompressionStream('deflate-raw');
  } catch {
    return { supported: false, reason: '浏览器不支持墨水屏所需的原始 DEFLATE 压缩，请更新 Chrome 或 Edge。' };
  }
  return { supported: true, reason: '' };
}

function copyValue(value: DataView): Uint8Array {
  return new Uint8Array(value.buffer, value.byteOffset, value.byteLength).slice();
}

function abortError(): DOMException { return new DOMException('已取消墨水屏传输。', 'AbortError'); }

/** Race GATT operations against cancellation and a bounded transport timeout. */
function bounded<T>(promise: Promise<T>, signal: AbortSignal, timeoutMs = 20_000, message = '墨水屏蓝牙操作超时，请重新连接。'): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    let settled = false;
    const finish = (action: () => void) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal.removeEventListener('abort', onAbort);
      action();
    };
    const onAbort = () => finish(() => reject(signal.reason ?? abortError()));
    const timer = setTimeout(() => finish(() => reject(new Error(message))), timeoutMs);
    signal.addEventListener('abort', onAbort, { once: true });
    promise.then((value) => finish(() => resolve(value)), (error: unknown) => finish(() => reject(error)));
    if (signal.aborted) onAbort();
  });
}

export class WolinkPanelClient {
  private device: PanelBluetoothDevice | null = null;
  private auth: PanelBluetoothCharacteristic | null = null;
  private writer: PanelBluetoothCharacteristic | null = null;
  private done: PanelBluetoothCharacteristic | null = null;
  private connecting = false;
  private generation = 0;
  private transfer: AbortController | null = null;
  private connectionAbort: AbortController | null = null;
  private readonly onDisconnected?: () => void;

  constructor(options: { onDisconnected?: () => void } = {}) {
    this.onDisconnected = options.onDisconnected;
  }

  get connected(): boolean { return Boolean(this.device?.gatt?.connected && this.auth && this.writer && this.done); }
  get deviceName(): string | null { return this.device?.name ?? null; }
  /** Browser opaque identifier, not a Bluetooth MAC address or display label. */
  get deviceId(): string | null { return this.device?.id ?? null; }

  private readonly handleDisconnected = () => {
    this.clearConnection();
    this.onDisconnected?.();
  };

  private clearConnection(): void {
    this.generation++;
    this.connectionAbort?.abort(abortError());
    this.transfer?.abort(abortError());
    this.device?.removeEventListener('gattserverdisconnected', this.handleDisconnected);
    this.device = null;
    this.auth = null;
    this.writer = null;
    this.done = null;
  }

  /** Call directly from a click handler, before any await, to preserve activation. */
  async connect(displayId = '', options: { acceptAllDevices?: boolean } = {}): Promise<PanelDevice> {
    if (this.connecting) throw new Error('蓝牙设备选择或连接正在进行。');
    if (this.transfer) throw new Error('请等待当前传输结束后再连接墨水屏。');
    if (this.connected) throw new Error('请先断开当前墨水屏，再连接其他设备。');
    const capability = getBluetoothCapability();
    if (!capability.supported) throw new Error(capability.reason);
    const requestedName = displayId.trim().toUpperCase();
    if (requestedName && !/^(?:WL)?[A-Z0-9_-]{1,32}$/.test(requestedName)) {
      throw new Error('请输入墨水屏以 WL 开头的蓝牙名称，或对应的 ID 后缀。');
    }
    const fullName = requestedName.startsWith('WL') ? requestedName : `WL${requestedName}`;
    // Invoke the browser chooser synchronously: no network, permission check or
    // availability await may precede requestDevice inside this click chain.
    const selected = bluetooth()!.requestDevice(options.acceptAllDevices
      ? { acceptAllDevices: true, optionalServices: [SERVICE_UUID] }
      : {
        filters: requestedName
          ? [{ name: fullName }]
          : [{ namePrefix: 'WL' }, { services: [SERVICE_UUID] }],
        optionalServices: [SERVICE_UUID],
      });
    this.connecting = true;
    const generation = ++this.generation;
    const controller = new AbortController();
    this.connectionAbort = controller;
    let device: PanelBluetoothDevice | null = null;
    try {
      device = await selected;
      controller.signal.throwIfAborted();
      if (generation !== this.generation) throw abortError();
      if (!device.gatt) throw new Error('所选设备未提供可连接的蓝牙 GATT 服务。');
      // With a supplied ID, even the broad chooser must not bind another screen.
      if (requestedName && device.name?.toUpperCase() !== fullName) {
        throw new Error(`所选设备与输入的 ${fullName} 不一致，请核对墨水屏广播 ID。`);
      }
      this.device = device;
      device.addEventListener('gattserverdisconnected', this.handleDisconnected);
      const connectingServer = device.gatt.connect();
      // Windows may finish an in-flight connection after cancellation/timeout.
      // Close that late connection rather than leaving an unowned GATT session.
      void connectingServer.then((server) => {
        if (controller.signal.aborted || generation !== this.generation) server.disconnect();
      }, () => {});
      const server = await bounded(connectingServer, controller.signal, 30_000);
      const service = await bounded(server.getPrimaryService(SERVICE_UUID), controller.signal);
      const auth = await bounded(service.getCharacteristic(AUTH_UUID), controller.signal);
      const writer = await bounded(service.getCharacteristic(IMAGE_WRITE_UUID), controller.signal);
      const done = await bounded(service.getCharacteristic(DONE_NOTIFY_UUID), controller.signal);
      if (!auth.properties.write || !writer.properties.write || (!done.properties.notify && !done.properties.indicate)) {
        throw new Error('所选设备不支持 WOLINK 墨水屏所需的写入与完成通知。');
      }
      if (generation !== this.generation) throw abortError();
      this.auth = auth;
      this.writer = writer;
      this.done = done;
      return { id: device.id, name: device.name || 'WOLINK 墨水屏' };
    } catch (error) {
      if (device?.gatt?.connected) device.gatt.disconnect();
      if (generation === this.generation) this.clearConnection();
      throw error;
    } finally {
      this.connecting = false;
      if (this.connectionAbort === controller) this.connectionAbort = null;
    }
  }

  disconnect(): void {
    const device = this.device;
    const wasConnected = Boolean(device?.gatt?.connected);
    this.clearConnection();
    device?.gatt?.disconnect();
    if (wasConnected) this.onDisconnected?.();
  }

  sendImage(image: ImageData, options: TransferOptions = {}): Promise<TransferResult> {
    return this.sendRawImage(imageDataToRaw(image), options);
  }

  async sendRawImage(raw: Uint8Array, options: TransferOptions = {}): Promise<TransferResult> {
    if (!this.connected || !this.auth || !this.writer || !this.done) throw new Error('请先连接墨水屏。');
    if (this.transfer) throw new Error('已有任务正在传输，请等待完成或取消。');
    const timeout = options.confirmationTimeoutMs ?? 60_000;
    if (!Number.isFinite(timeout) || timeout <= 0) throw new Error('完成通知等待时间必须大于 0。');
    const controller = new AbortController();
    this.transfer = controller;
    const signal = controller.signal;
    const externalAbort = () => {
      controller.abort(abortError());
      // Stop queued GATT writes and abandon this connection, so a canceled
      // transfer cannot finish later and be mistaken for the next refresh.
      this.disconnect();
    };
    options.signal?.addEventListener('abort', externalAbort, { once: true });
    if (options.signal?.aborted) externalAbort();
    const auth = this.auth;
    const writer = this.writer;
    const done = this.done;
    const deviceName = this.deviceName || 'WOLINK 墨水屏';
    let notified = false;
    let allowDone = false;
    let lastNotification = '';
    let resolveDone!: (bytes: Uint8Array) => void;
    const confirmation = new Promise<Uint8Array>((resolve) => { resolveDone = resolve; });
    const handleDone = (event: Event) => {
      const value = (event.target as PanelBluetoothCharacteristic).value;
      if (!value || !allowDone) return;
      const bytes = copyValue(value);
      lastNotification = bytesToHex(bytes);
      if (isSuccessNotification(bytes)) resolveDone(bytes);
    };
    let totalPackets = 0;
    let sentPackets = 0;
    const progress = (phase: TransferProgress['phase']) => options.onProgress?.({
      phase, sentPackets, totalPackets,
      percent: totalPackets ? Math.round(sentPackets / totalPackets * 100) : 0,
    });
    try {
      signal.throwIfAborted();
      progress('preparing');
      const stream = await bounded(buildImageStream(raw, signal), signal);
      totalPackets = Math.ceil(stream.length / WRITE_PAYLOAD_SIZE) + 1;
      progress('authenticating');
      const challenge = copyValue(await bounded(auth!.readValue(), signal));
      const response = await bounded(encryptChallenge(challenge), signal);
      await bounded(auth!.writeValueWithResponse(response.slice().buffer), signal);
      done!.addEventListener('characteristicvaluechanged', handleDone);
      await bounded(done!.startNotifications(), signal);
      notified = true;
      progress('sending');
      for (let offset = 0; offset < stream.length; offset += WRITE_PAYLOAD_SIZE) {
        signal.throwIfAborted();
        const packet = buildDataPacket(offset, stream.subarray(offset, offset + WRITE_PAYLOAD_SIZE));
        await bounded(writer!.writeValueWithResponse(packet.buffer as ArrayBuffer), signal);
        sentPackets++;
        progress('sending');
      }
      signal.throwIfAborted();
      // A fast screen may notify before the final write promise resolves.
      allowDone = true;
      await bounded(writer!.writeValueWithResponse(buildEndPacket(stream.length).buffer as ArrayBuffer), signal);
      sentPackets++;
      progress('waiting');
      const notification = await bounded(confirmation, signal, timeout,
        `任务数据已写入，但未收到墨水屏刷新完成确认${lastNotification ? `（通知 ${lastNotification}）` : ''}。请检查屏幕后重新连接。`);
      signal.throwIfAborted();
      progress('done');
      return { deviceName, streamBytes: stream.length, packets: totalPackets, notificationHex: bytesToHex(notification) };
    } catch (error) {
      this.disconnect();
      throw error;
    } finally {
      options.signal?.removeEventListener('abort', externalAbort);
      done?.removeEventListener('characteristicvaluechanged', handleDone);
      if (notified && this.connected && done) {
        try { await bounded(done.stopNotifications(), signal, 5_000); } catch { this.disconnect(); }
      }
      if (this.transfer === controller) this.transfer = null;
    }
  }
}
