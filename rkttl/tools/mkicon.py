#!/usr/bin/env python3
"""生成 rkttl 图标（16/32/48/256 多尺寸 ICO，无第三方依赖）。

设计：深色圆角底板 + 双向箭头（TTL 收/发），下方一根"电路引脚"。
ICO 内嵌 BITMAPINFOHEADER 的未压缩 32bpp BGRA 位图与 AND 掩码。
"""

import struct


def px(x, y, w, h, color):
    """在 (x,y) 处画 1px。"""
    return [(x, y, color)]


def rect(x0, y0, w, h, color):
    return [(x, y, color) for y in range(y0, y0 + h) for x in range(x0, x0 + w)]


def draw(size):
    """返回 (size, size) 的 BGRA 像素二维列表。"""
    bg = (28, 32, 36, 255)        # 深灰蓝 BGRA
    fg = (109, 176, 77, 255)      # 终端绿（RX）
    fg2 = (240, 210, 140, 255)    # 琥珀（TX）
    pxs = [[bg for _ in range(size)] for _ in range(size)]

    def fill(x0, y0, w, h, c):
        for y in range(y0, y0 + h):
            for x in range(x0, x0 + w):
                if 0 <= x < size and 0 <= y < size:
                    pxs[y][x] = c

    u = max(1, size // 16)        # 基本单位

    # 圆角底板
    r = 2 * u
    for y in range(size):
        for x in range(size):
            cx = min(x, size - 1 - x)
            cy = min(y, size - 1 - y)
            if cx < r and cy < r and (r - cx) ** 2 + (r - cy) ** 2 > r * r:
                pxs[y][x] = (0, 0, 0, 0)

    def arrow(y_center, color, direction):
        shaft_h = max(1, u)
        x0, x1 = 3 * u, 11 * u          # 箭身范围
        sy = y_center - shaft_h // 2
        fill(x0, sy, x1 - x0, shaft_h, color)
        L = 3 * u                        # 箭头长度
        H = 3 * u                        # 箭头底部高度
        for i in range(L):
            hh = max(shaft_h, H - (i * H) // L)
            yy = y_center - hh // 2
            if direction == "right":
                fill(x1 + i, yy, 1, hh, color)
            else:
                fill(x0 - 1 - i, yy, 1, hh, color)

    arrow(6 * u, fg, "right")            # RX →
    arrow(10 * u, fg2, "left")           # ← TX
    fill(3 * u, 13 * u, 10 * u, max(1, u), fg2)  # 底部引脚条
    return pxs


def encode_bgra(pxs):
    h = len(pxs)
    w = len(pxs[0])
    # 自下而上、行 4 字节对齐
    rows = []
    for y in range(h - 1, -1, -1):
        row = bytearray()
        for c in pxs[y]:
            b, g, r_, a = c
            row += bytes((b, g, r_, a))
        rows.append(bytes(row))
    and_mask = bytearray()
    mask_row_bytes = ((w + 31) // 32) * 4
    for y in range(h - 1, -1, -1):
        m = bytearray(mask_row_bytes)
        for x in range(w):
            if pxs[y][x][3] == 0:
                m[x // 8] |= 0x80 >> (x % 8)
        and_mask += m
    return b"".join(rows) + bytes(and_mask)


def make_ico(sizes, out):
    images = []
    for s in sizes:
        pxs = draw(s)
        data = encode_bgra(pxs)
        bih = struct.pack(
            "<IiiHHIIiiII",
            40, s, 2 * s, 1, 32, 0, len(data), 0, 0, 0, 0,
        )
        images.append((s, bih + data))

    header = struct.pack("<HHH", 0, 1, len(images))  # ICONDIR
    offset = 6 + 16 * len(images)
    entries = b""
    blob = b""
    for s, data in images:
        w = 0 if s >= 256 else s
        entries += struct.pack("<BBBBHHII", w, w, 0, 0, 1, 32, len(data), offset)
        blob += data
        offset += len(data)
    with open(out, "wb") as f:
        f.write(header + entries + blob)
    print(f"wrote {out}: sizes={sizes}")


if __name__ == "__main__":
    make_ico([16, 24, 32, 48, 64], "rkttl.ico")
