#!/usr/bin/env python3
"""Generate bridge PWA icons: the Golden Gate, in international orange.

Pure stdlib (zlib + struct), no PIL — rerun after tweaking.
"""
import struct
import sys
import zlib
from pathlib import Path

SKY_TOP = (13, 17, 23)       # #0d1117 — matches the PWA background
SKY_BOT = (30, 40, 66)       # a touch of dusk toward the water
ORANGE = (224, 78, 45)       # international orange, warmed to pop on dark
ORANGE_DK = (176, 54, 30)    # shaded side of towers/cables
CABLE = (232, 104, 66)


def png(width, height, pixels):
    def chunk(tag, data):
        raw = tag + data
        return struct.pack(">I", len(data)) + raw + struct.pack(">I", zlib.crc32(raw))

    raw = b"".join(b"\x00" + bytes(c for px in row for c in px) for row in pixels)
    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 9))
            + chunk(b"IEND", b""))


def draw(size):
    s = size / 100.0
    px = [[SKY_TOP] * size for _ in range(size)]

    def blend(a, b, t):
        return tuple(int(a[i] + (b[i] - a[i]) * t) for i in range(3))

    # dusk gradient sky
    for y in range(size):
        col = blend(SKY_TOP, SKY_BOT, y / size)
        for x in range(size):
            px[y][x] = col

    def rect(x0, y0, x1, y1, color):
        for y in range(max(0, int(y0)), min(size, int(y1))):
            for x in range(max(0, int(x0)), min(size, int(x1))):
                px[y][x] = color

    def dot(cx, cy, r, color):
        for y in range(max(0, int(cy - r)), min(size, int(cy + r + 1))):
            for x in range(max(0, int(cx - r)), min(size, int(cx + r + 1))):
                if (x - cx) ** 2 + (y - cy) ** 2 <= r * r:
                    px[y][x] = color

    def cable(pts, thick, color):
        # pts are in 0-100 design space; scale into pixels here
        p = [(x * s, y * s) for (x, y) in pts]
        r = max(thick * s / 2, 0.6)
        for i in range(len(p) - 1):
            x0, y0 = p[i]
            x1, y1 = p[i + 1]
            steps = int(max(abs(x1 - x0), abs(y1 - y0))) + 1
            for k in range(steps + 1):
                t = k / steps
                dot(x0 + (x1 - x0) * t, y0 + (y1 - y0) * t, r, color)

    # roadway deck
    deck = 74
    rect(0, deck * s, size, (deck + 3) * s, ORANGE_DK)

    # two towers with the Golden Gate's stacked cross-braces
    for tx in (30, 70):
        rect((tx - 3) * s, 20 * s, (tx + 3) * s, (deck + 3) * s, ORANGE)   # legs block
        rect((tx - 3) * s, 20 * s, (tx - 1) * s, (deck + 3) * s, ORANGE)   # left leg
        rect((tx + 1) * s, 20 * s, (tx + 3) * s, (deck + 3) * s, ORANGE)   # right leg
        # hollow the gap between legs, then add horizontal cross-braces
        rect((tx - 1) * s, 24 * s, (tx + 1) * s, deck * s, blend(SKY_TOP, SKY_BOT, deck / size))
        for by in (26, 38, 52, 66):
            rect((tx - 3) * s, by * s, (tx + 3) * s, (by + 2) * s, ORANGE)
        rect((tx - 4) * s, 20 * s, (tx + 4) * s, 23 * s, ORANGE)           # top cap

    # main suspension cables: catenary sweep, tower-top → mid-span dip → tower-top
    cable([(0, 40), (14, 48), (30, 22)], 2.4, CABLE)
    cable([(30, 22), (50, 52), (70, 22)], 2.4, CABLE)
    cable([(70, 22), (86, 48), (100, 40)], 2.4, CABLE)

    # vertical suspenders dropping to the deck
    for hx in range(8, 100, 8):
        # follow the cable height roughly for a believable drape
        if hx < 30:
            cy = 48 - (48 - 22) * (hx / 30) if hx > 14 else 40 + (48 - 40) * (hx / 14)
        elif hx < 50:
            cy = 22 + (52 - 22) * ((hx - 30) / 20)
        elif hx < 70:
            cy = 52 - (52 - 22) * ((hx - 50) / 20)
        else:
            cy = 22 + (48 - 22) * ((hx - 70) / 16) if hx < 86 else 48 + (40 - 48) * ((hx - 86) / 14)
        cable([(hx, cy), (hx, deck)], 0.9, CABLE)

    return px


def main():
    out = Path(__file__).resolve().parent.parent / "pwa" / "icons"
    out.mkdir(parents=True, exist_ok=True)
    for size in (180, 192, 512):
        (out / f"icon-{size}.png").write_bytes(png(size, size, draw(size)))
        print(f"wrote icon-{size}.png")


if __name__ == "__main__":
    sys.exit(main())
