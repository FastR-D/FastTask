from pathlib import Path
from typing import Iterable, Literal, Tuple

from PIL import Image

from .models import DEFAULT_CONFIG, DisplayConfig
from .protocol import join_planes

PaletteColor = Literal["black", "white", "red"]

PALETTE_RGB = {
    "black": (0, 0, 0),
    "white": (255, 255, 255),
    "red": (255, 0, 0),
}

COLOR_BITS = {
    "black": (0, 0),
    "white": (1, 0),
    "red": (1, 1),
}


def _resize_cover(img: Image.Image, width: int, height: int) -> Image.Image:
    source = img.convert("RGB")
    target_ratio = width / height
    source_ratio = source.width / source.height

    if source_ratio > target_ratio:
        new_height = height
        new_width = round(height * source_ratio)
    else:
        new_width = width
        new_height = round(width / source_ratio)

    resized = source.resize((new_width, new_height), Image.Resampling.LANCZOS)
    left = (new_width - width) // 2
    top = (new_height - height) // 2
    return resized.crop((left, top, left + width, top + height))


def fit_image(
    img: Image.Image,
    width: int = DEFAULT_CONFIG.width,
    height: int = DEFAULT_CONFIG.height,
    mode: Literal["cover", "stretch"] = "cover",
) -> Image.Image:
    if mode == "cover":
        return _resize_cover(img, width, height)
    if mode == "stretch":
        return img.convert("RGB").resize((width, height), Image.Resampling.LANCZOS)
    raise ValueError("fit mode must be 'cover' or 'stretch'")


def nearest_palette_color(rgb: Tuple[int, int, int]) -> PaletteColor:
    r, g, b = rgb
    distances = {
        name: (r - pr) ** 2 + (g - pg) ** 2 + (b - pb) ** 2
        for name, (pr, pg, pb) in PALETTE_RGB.items()
    }
    return min(distances, key=distances.get)  # type: ignore[return-value]


def quantize_to_palette(img: Image.Image, dither: bool = False) -> Image.Image:
    source = img.convert("RGB")
    if dither:
        palette = Image.new("P", (1, 1))
        palette_values = [channel for color in PALETTE_RGB.values() for channel in color]
        palette.putpalette(palette_values + [0] * (768 - len(palette_values)))
        return source.quantize(
            palette=palette,
            dither=Image.Dither.FLOYDSTEINBERG,
        ).convert("RGB")

    pixels = [PALETTE_RGB[nearest_palette_color(pixel)] for pixel in source.getdata()]
    out = Image.new("RGB", source.size)
    out.putdata(pixels)
    return out


def _pack_bits(bits: Iterable[int]) -> bytes:
    out = bytearray()
    value = 0
    count = 0
    for bit in bits:
        value = (value << 1) | (1 if bit else 0)
        count += 1
        if count == 8:
            out.append(value)
            value = 0
            count = 0
    if count:
        out.append(value << (8 - count))
    return bytes(out)


def image_to_planes(img: Image.Image) -> tuple[bytes, bytes]:
    source = img.convert("RGB")
    plane1_bits = []
    plane2_bits = []
    for pixel in source.getdata():
        color = nearest_palette_color(pixel)
        p1, p2 = COLOR_BITS[color]
        plane1_bits.append(p1)
        plane2_bits.append(p2)
    return _pack_bits(plane1_bits), _pack_bits(plane2_bits)


def image_to_raw(
    img: Image.Image,
    config: DisplayConfig = DEFAULT_CONFIG,
    fit: Literal["cover", "stretch"] = "cover",
) -> bytes:
    prepared = quantize_to_palette(fit_image(img, config.width, config.height, fit))
    plane1, plane2 = image_to_planes(prepared)
    if len(plane1) != config.plane_bytes or len(plane2) != config.plane_bytes:
        raise ValueError("encoded image plane size does not match display config")
    return join_planes(plane1, plane2)


def load_image_raw(
    path: str | Path,
    config: DisplayConfig = DEFAULT_CONFIG,
    fit: Literal["cover", "stretch"] = "cover",
) -> bytes:
    with Image.open(path) as img:
        return image_to_raw(img, config=config, fit=fit)
