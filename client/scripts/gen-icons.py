#!/usr/bin/env python3
"""Generate Hearth PWA icons (192/512 + apple-touch 180). Pure stdlib PNG writer."""
import os
import struct
import zlib

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "public", "icons")


def chunk(tag, data):
    c = struct.pack(">I", len(data)) + tag + data
    return c + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)


def write_png(path, w, h, px):
    raw = b""
    for y in range(h):
        row = b"".join(bytes(px(x, y)) for x in range(w))
        raw += b"\x00" + row
    png = b"\x89PNG\r\n\x1a\n"
    png += chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0))
    png += chunk(b"IDAT", zlib.compress(raw, 9))
    png += chunk(b"IEND", b"")
    with open(path, "wb") as f:
        f.write(png)


def make_icon(size):
    def px(x, y):
        t = y / (size - 1)
        r = int(13 + 20 * (1 - t))
        g = int(10 + 14 * (1 - t))
        b = int(18 + 42 * (1 - t))
        cx, cy = size * 0.5, size * 0.55
        d = ((x - cx) ** 2 + (y - cy) ** 2) ** 0.5
        rad = size * 0.34
        if d < rad:
            k = 1 - d / rad
            r, g, b = 245, 158 + int(50 * k), 11 + int(40 * k)
            if k > 0.55:
                r, g, b = 255, 214 + int(30 * k), 102
        for (sx, sy, sr) in ((0.25, 0.22, 0.02), (0.78, 0.30, 0.015), (0.68, 0.80, 0.018)):
            if ((x - size * sx) ** 2 + (y - size * sy) ** 2) ** 0.5 < size * sr:
                r = g = b = 255
        return (r, g, b, 255)

    return px


os.makedirs(OUT, exist_ok=True)
for size in (192, 512):
    write_png(os.path.join(OUT, f"icon-{size}.png"), size, size, make_icon(size))
write_png(os.path.join(OUT, "apple-touch-icon.png"), 180, 180, make_icon(180))
print("icons written to", OUT)
