import math
import struct
import zlib
from typing import Iterable, List, Sequence

from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

from .models import DEFAULT_AUTH_KEY, DEFAULT_CONFIG, DisplayConfig

IMAGE_STREAM_HEADER = b"\xA5\xA6\x0C\x02"
SUCCESS_NOTIFICATION = b"\xFF\x00\x00\x00\x00\x00\x00\x00"


def encrypt_challenge(challenge: bytes, key: bytes = DEFAULT_AUTH_KEY) -> bytes:
    """Encrypt the 16-byte authentication challenge with AES-128-CBC and a zero IV."""
    if len(challenge) != 16:
        raise ValueError("authentication challenge must be exactly 16 bytes")
    if len(key) != 16:
        raise ValueError("authentication key must be exactly 16 bytes")

    cipher = Cipher(algorithms.AES(key), modes.CBC(b"\x00" * 16))
    encryptor = cipher.encryptor()
    return encryptor.update(challenge) + encryptor.finalize()


def raw_deflate(data: bytes, level: int = 9) -> bytes:
    compressor = zlib.compressobj(level=level, wbits=-15)
    return compressor.compress(data) + compressor.flush()


def split_blocks(raw: bytes, config: DisplayConfig = DEFAULT_CONFIG) -> List[bytes]:
    if len(raw) != config.raw_bytes:
        raise ValueError(f"raw image must be {config.raw_bytes} bytes")
    return [
        raw[offset : offset + config.block_size]
        for offset in range(0, len(raw), config.block_size)
    ]


def compress_blocks(raw: bytes, config: DisplayConfig = DEFAULT_CONFIG) -> List[bytes]:
    return [raw_deflate(block) for block in split_blocks(raw, config)]


def build_image_stream(raw: bytes, config: DisplayConfig = DEFAULT_CONFIG) -> bytes:
    blocks = compress_blocks(raw, config)
    expected_blocks = math.ceil(config.raw_bytes / config.block_size)
    if len(blocks) != expected_blocks:
        raise ValueError(f"expected {expected_blocks} blocks, got {len(blocks)}")

    out = bytearray(IMAGE_STREAM_HEADER)
    for block_id, compressed in enumerate(blocks, start=1):
        if len(compressed) > 0xFFFF:
            raise ValueError(f"compressed block {block_id} is too large")
        out.append(block_id)
        out.extend(struct.pack("<H", len(compressed)))
        out.extend(compressed)
    return bytes(out)


def build_data_packet(offset: int, chunk: bytes) -> bytes:
    if offset < 0:
        raise ValueError("offset must be non-negative")
    return b"\x00\xA5" + struct.pack("<I", offset) + chunk


def build_end_packet(stream_length: int) -> bytes:
    if stream_length < 0:
        raise ValueError("stream length must be non-negative")
    return b"\x02\xA5" + struct.pack("<I", stream_length)


def iter_data_packets(
    image_stream: bytes,
    payload_size: int = DEFAULT_CONFIG.write_payload_size,
) -> Iterable[bytes]:
    if payload_size <= 0:
        raise ValueError("payload size must be positive")
    for offset in range(0, len(image_stream), payload_size):
        yield build_data_packet(offset, image_stream[offset : offset + payload_size])


def build_write_packets(
    image_stream: bytes,
    payload_size: int = DEFAULT_CONFIG.write_payload_size,
) -> List[bytes]:
    packets = list(iter_data_packets(image_stream, payload_size))
    packets.append(build_end_packet(len(image_stream)))
    return packets


def parse_block_entries(image_stream: bytes) -> List[tuple[int, bytes]]:
    """Return block id and compressed payload tuples from an image stream.

    This helper is mainly useful in tests and diagnostics.
    """
    if not image_stream.startswith(IMAGE_STREAM_HEADER):
        raise ValueError("image stream has an invalid header")

    entries = []
    offset = len(IMAGE_STREAM_HEADER)
    while offset < len(image_stream):
        block_id = image_stream[offset]
        compressed_len = struct.unpack_from("<H", image_stream, offset + 1)[0]
        start = offset + 3
        end = start + compressed_len
        if end > len(image_stream):
            raise ValueError("image stream has a truncated compressed block")
        entries.append((block_id, image_stream[start:end]))
        offset = end
    return entries


def join_planes(plane1: Sequence[int] | bytes, plane2: Sequence[int] | bytes) -> bytes:
    p1 = bytes(plane1)
    p2 = bytes(plane2)
    if len(p1) != len(p2):
        raise ValueError("plane lengths must match")
    return p1 + p2
