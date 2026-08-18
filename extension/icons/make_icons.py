"""Generate the extension icons with the stdlib only (no Pillow needed).

Draws a rounded blue tile with a white download arrow, supersampled 4x for
smooth edges, and writes 16/48/128 px PNGs next to this script.
"""
import os
import struct
import zlib

BG = (47, 109, 246)      # --accent
FG = (255, 255, 255)
SS = 4                   # supersampling factor


def coverage(size):
    """Return a size x size float mask: 1.0 inside the glyph, 0.0 outside."""
    n = size * SS
    radius = n * 0.22
    mask = [[0.0] * size for _ in range(size)]

    # Arrow geometry in supersampled space.
    cx = n / 2.0
    stem_w = n * 0.13
    stem_top = n * 0.22
    stem_bot = n * 0.52
    head_top = n * 0.44
    head_bot = n * 0.72
    head_half = n * 0.26
    bar_top = n * 0.78
    bar_bot = n * 0.88
    bar_half = n * 0.28

    for y in range(n):
        for x in range(n):
            # Rounded-rect clip so the tile corners are not square.
            dx = max(radius - x, x - (n - radius), 0)
            dy = max(radius - y, y - (n - radius), 0)
            if dx * dx + dy * dy > radius * radius:
                continue

            inside = False
            if stem_top <= y <= stem_bot and abs(x - cx) <= stem_w:
                inside = True
            elif head_top <= y <= head_bot:
                # Triangle narrowing to a point at head_bot.
                t = (head_bot - y) / (head_bot - head_top)
                if abs(x - cx) <= head_half * t:
                    inside = True
            elif bar_top <= y <= bar_bot and abs(x - cx) <= bar_half:
                inside = True

            if inside:
                mask[y // SS][x // SS] += 1.0

    inv = 1.0 / (SS * SS)
    return [[v * inv for v in row] for row in mask]


def tile_alpha(size):
    """Alpha mask for the rounded background tile itself."""
    n = size * SS
    radius = n * 0.22
    alpha = [[0.0] * size for _ in range(size)]
    for y in range(n):
        for x in range(n):
            dx = max(radius - x, x - (n - radius), 0)
            dy = max(radius - y, y - (n - radius), 0)
            if dx * dx + dy * dy <= radius * radius:
                alpha[y // SS][x // SS] += 1.0
    inv = 1.0 / (SS * SS)
    return [[v * inv for v in row] for row in alpha]


def write_png(path, size):
    glyph = coverage(size)
    tile = tile_alpha(size)

    raw = bytearray()
    for y in range(size):
        raw.append(0)  # filter type: none
        for x in range(size):
            g = glyph[y][x]
            a = tile[y][x]
            r = round(BG[0] * (1 - g) + FG[0] * g)
            gg = round(BG[1] * (1 - g) + FG[1] * g)
            b = round(BG[2] * (1 - g) + FG[2] * g)
            raw += bytes((r, gg, b, round(a * 255)))

    def chunk(tag, data):
        out = struct.pack(">I", len(data)) + tag + data
        return out + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

    png = b"\x89PNG\r\n\x1a\n"
    png += chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
    png += chunk(b"IDAT", zlib.compress(bytes(raw), 9))
    png += chunk(b"IEND", b"")

    with open(path, "wb") as f:
        f.write(png)
    return len(png)


if __name__ == "__main__":
    here = os.path.dirname(os.path.abspath(__file__))
    for s in (16, 48, 128):
        p = os.path.join(here, "%d.png" % s)
        print("%s (%d bytes)" % (p, write_png(p, s)))
