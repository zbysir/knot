#!/usr/bin/env python3
"""Draw the app icon: two interlocked rings on the panel's dark background.

Kept as a script rather than a checked-in binary so the icon can be adjusted
without a design tool, and so nobody has to trust an opaque .icns blob.
"""
import math, os, struct, subprocess, sys, tempfile, zlib

SS = 3  # supersampling factor; 3x is enough for a 1024px master

BG_TOP, BG_BOT = (0x1b, 0x1f, 0x29), (0x0d, 0x0f, 0x14)
RING_A = (0x5b, 0x9c, 0xf8)
RING_B = (0x3f, 0xb9, 0x50)


def write_png(path, w, h, rgba):
    raw = b"".join(b"\x00" + rgba[y * w * 4:(y + 1) * w * 4] for y in range(h))

    def chunk(tag, data):
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))

    hdr = struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0)
    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", hdr)
                + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


def squircle(x, y, cx, cy, half, r):
    """Signed-ish coverage test for a rounded square centred at (cx, cy)."""
    dx, dy = abs(x - cx) - (half - r), abs(y - cy) - (half - r)
    if dx <= 0 and dy <= 0:
        return True
    dx, dy = max(dx, 0.0), max(dy, 0.0)
    return dx * dx + dy * dy <= r * r


def ring(x, y, cx, cy, radius, thick):
    d = math.hypot(x - cx, y - cy)
    return abs(d - radius) <= thick / 2


def draw(size):
    w = h = size
    buf = bytearray(w * h * 4)
    c = size / 2.0
    half = size * 0.41          # rounded square half-width, leaves the grid margin
    corner = size * 0.225
    radius = size * 0.155
    thick = size * 0.055
    off = size * 0.125          # how far the two rings sit from centre

    for py in range(h):
        for px in range(w):
            acc = [0.0, 0.0, 0.0, 0.0]
            for sy in range(SS):
                for sx in range(SS):
                    x = px + (sx + 0.5) / SS
                    y = py + (sy + 0.5) / SS
                    if not squircle(x, y, c, c, half, corner):
                        continue
                    t = (y - (c - half)) / (2 * half)
                    bg = tuple(BG_TOP[i] + (BG_BOT[i] - BG_TOP[i]) * min(max(t, 0), 1)
                               for i in range(3))
                    col = bg
                    in_a = ring(x, y, c - off, c, radius, thick)
                    in_b = ring(x, y, c + off, c, radius, thick)
                    if in_b:
                        col = RING_B
                    if in_a:
                        # The left ring passes over the right one on the top of
                        # the overlap and under it on the bottom, which is what
                        # reads as "interlocked" rather than "two circles".
                        col = RING_A if (not in_b or y < c) else col
                    acc[0] += col[0]; acc[1] += col[1]; acc[2] += col[2]; acc[3] += 255
            n = SS * SS
            i = (py * w + px) * 4
            if acc[3] == 0:
                continue
            cov = acc[3] / (255.0 * n)
            buf[i] = int(acc[0] / (n * cov))
            buf[i + 1] = int(acc[1] / (n * cov))
            buf[i + 2] = int(acc[2] / (n * cov))
            buf[i + 3] = int(acc[3] / n)
    return bytes(buf)


def main():
    out = sys.argv[1] if len(sys.argv) > 1 else "knot.icns"
    with tempfile.TemporaryDirectory() as tmp:
        iconset = os.path.join(tmp, "knot.iconset")
        os.makedirs(iconset)
        master = 1024
        png = os.path.join(tmp, "master.png")
        write_png(png, master, master, draw(master))
        for size in (16, 32, 128, 256, 512):
            for scale, suffix in ((1, ""), (2, "@2x")):
                px = size * scale
                dst = os.path.join(iconset, f"icon_{size}x{size}{suffix}.png")
                subprocess.run(["sips", "-z", str(px), str(px), png, "--out", dst],
                               check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["iconutil", "-c", "icns", iconset, "-o", out], check=True)
    print(out)


if __name__ == "__main__":
    main()
