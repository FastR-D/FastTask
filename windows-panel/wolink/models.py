from dataclasses import dataclass


SERVICE_UUID = "30323032-4C53-4545-4C42-4B4E494C4F57"
IMAGE_WRITE_UUID = "31323032-4C53-4545-4C42-4B4E494C4F57"
DEVICE_INFO_UUID = "32323032-4C53-4545-4C42-4B4E494C4F57"
AUTH_UUID = "33323032-4C53-4545-4C42-4B4E494C4F57"
DONE_NOTIFY_UUID = "34323032-4C53-4545-4C42-4B4E494C4F57"
BATTERY_UUID = "35323032-4C53-4545-4C42-4B4E494C4F57"

DEFAULT_AUTH_KEY = bytes.fromhex("9B609F28BC49E25729BD7B8DF22B4420")


@dataclass(frozen=True)
class DisplayConfig:
    width: int = 800
    height: int = 480
    block_size: int = 8192
    write_payload_size: int = 238

    @property
    def plane_bytes(self) -> int:
        pixels = self.width * self.height
        if pixels % 8:
            raise ValueError("width * height must be divisible by 8")
        return pixels // 8

    @property
    def raw_bytes(self) -> int:
        return self.plane_bytes * 2


DEFAULT_CONFIG = DisplayConfig()
