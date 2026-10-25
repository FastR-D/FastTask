/** WOLINK ESL-75MR wire format, ported from myled/wolink_esl. */
export const DISPLAY_WIDTH = 800;
export const DISPLAY_HEIGHT = 480;
export const PLANE_BYTES = (DISPLAY_WIDTH * DISPLAY_HEIGHT) / 8;
export const RAW_IMAGE_BYTES = PLANE_BYTES * 2;
export const BLOCK_SIZE = 8192;
export const WRITE_PAYLOAD_SIZE = 238;

export const SERVICE_UUID = '30323032-4c53-4545-4c42-4b4e494c4f57';
export const IMAGE_WRITE_UUID = '31323032-4c53-4545-4c42-4b4e494c4f57';
export const DEVICE_INFO_UUID = '32323032-4c53-4545-4c42-4b4e494c4f57';
export const AUTH_UUID = '33323032-4c53-4545-4c42-4b4e494c4f57';
export const DONE_NOTIFY_UUID = '34323032-4c53-4545-4c42-4b4e494c4f57';
export const BATTERY_UUID = '35323032-4c53-4545-4c42-4b4e494c4f57';

const AUTH_KEY = new Uint8Array([
  0x9b, 0x60, 0x9f, 0x28, 0xbc, 0x49, 0xe2, 0x57,
  0x29, 0xbd, 0x7b, 0x8d, 0xf2, 0x2b, 0x44, 0x20,
]);
const IMAGE_HEADER = new Uint8Array([0xa5, 0xa6, 0x0c, 0x02]);
export const SUCCESS_NOTIFICATION = new Uint8Array([0xff, 0, 0, 0, 0, 0, 0, 0]);

/** AES-128-CBC, zero IV, exactly one block without padding on the wire. */
export async function encryptChallenge(challenge: Uint8Array): Promise<Uint8Array> {
  if (challenge.length !== 16) throw new Error('墨水屏认证挑战长度错误，应为 16 字节。');
  const key = await crypto.subtle.importKey('raw', AUTH_KEY, 'AES-CBC', false, ['encrypt']);
  const encrypted = await crypto.subtle.encrypt(
    { name: 'AES-CBC', iv: new Uint8Array(16) }, key, challenge.slice().buffer,
  );
  // WebCrypto appends a PKCS#7 block. CBC's first ciphertext block is identical
  // to the unpadded reference implementation; never send the padding block.
  return new Uint8Array(encrypted).slice(0, 16);
}

/** Independent raw DEFLATE stream (no zlib header/checksum) per block. */
export async function rawDeflate(block: Uint8Array): Promise<Uint8Array> {
  const stream = new Blob([block.slice().buffer]).stream()
    .pipeThrough(new CompressionStream('deflate-raw'));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

export async function buildImageStream(raw: Uint8Array, signal?: AbortSignal): Promise<Uint8Array> {
  if (raw.length !== RAW_IMAGE_BYTES) throw new Error(`墨水屏图像必须为 ${RAW_IMAGE_BYTES} 字节。`);
  const blocks: Uint8Array[] = [];
  let length = IMAGE_HEADER.length;
  for (let offset = 0; offset < raw.length; offset += BLOCK_SIZE) {
    signal?.throwIfAborted();
    const compressed = await rawDeflate(raw.subarray(offset, offset + BLOCK_SIZE));
    if (compressed.length > 0xffff) throw new Error('墨水屏压缩数据块超出协议长度。');
    blocks.push(compressed);
    length += 3 + compressed.length;
  }
  signal?.throwIfAborted();
  const output = new Uint8Array(length);
  const view = new DataView(output.buffer);
  output.set(IMAGE_HEADER);
  let offset = IMAGE_HEADER.length;
  blocks.forEach((block, index) => {
    output[offset] = index + 1;
    view.setUint16(offset + 1, block.length, true);
    output.set(block, offset + 3);
    offset += 3 + block.length;
  });
  return output;
}

export function buildDataPacket(offset: number, chunk: Uint8Array): Uint8Array {
  if (!Number.isSafeInteger(offset) || offset < 0 || offset > 0xffffffff) {
    throw new Error('墨水屏数据偏移无效。');
  }
  const packet = new Uint8Array(6 + chunk.length);
  packet.set([0x00, 0xa5]);
  new DataView(packet.buffer).setUint32(2, offset, true);
  packet.set(chunk, 6);
  return packet;
}

export function buildEndPacket(streamLength: number): Uint8Array {
  const packet = buildDataPacket(streamLength, new Uint8Array());
  packet[0] = 0x02;
  return packet;
}

/** Row-major, MSB-first; black=(0,0), white=(1,0), red=(1,1). */
export function imageDataToRaw(image: Pick<ImageData, 'width' | 'height' | 'data'>): Uint8Array {
  if (image.width !== DISPLAY_WIDTH || image.height !== DISPLAY_HEIGHT) {
    throw new Error(`墨水屏画布尺寸必须为 ${DISPLAY_WIDTH} × ${DISPLAY_HEIGHT}。`);
  }
  if (image.data.length !== DISPLAY_WIDTH * DISPLAY_HEIGHT * 4) {
    throw new Error('墨水屏画布像素数据长度无效。');
  }
  const raw = new Uint8Array(RAW_IMAGE_BYTES);
  for (let pixel = 0; pixel < DISPLAY_WIDTH * DISPLAY_HEIGHT; pixel++) {
    const source = pixel * 4;
    // Canvas transparency is composited over the physical white background.
    const alpha = image.data[source + 3] / 255;
    const r = Math.round(image.data[source] * alpha + 255 * (1 - alpha));
    const g = Math.round(image.data[source + 1] * alpha + 255 * (1 - alpha));
    const b = Math.round(image.data[source + 2] * alpha + 255 * (1 - alpha));
    const black = r * r + g * g + b * b;
    const white = (r - 255) ** 2 + (g - 255) ** 2 + (b - 255) ** 2;
    const red = (r - 255) ** 2 + g * g + b * b;
    // Strict comparisons preserve Python palette insertion order on a tie.
    const color = white < black ? (red < white ? 'red' : 'white') : (red < black ? 'red' : 'black');
    const byte = pixel >> 3;
    const mask = 0x80 >> (pixel & 7);
    if (color !== 'black') raw[byte] |= mask;
    if (color === 'red') raw[PLANE_BYTES + byte] |= mask;
  }
  return raw;
}

export function isSuccessNotification(bytes: Uint8Array): boolean {
  return bytes.length === SUCCESS_NOTIFICATION.length
    && bytes.every((value, index) => value === SUCCESS_NOTIFICATION[index]);
}

export function bytesToHex(bytes: Uint8Array): string {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('').toUpperCase();
}
