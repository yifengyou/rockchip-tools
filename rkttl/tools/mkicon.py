#!/usr/bin/env python3
"""生成 rkttl 应用图标 v2（超采样抗锯齿，无第三方依赖）。

设计：深灰蓝渐变圆角底 + 终端绿 ">_" 提示符（经典 terminal 图形）。
4 倍超采样后盒式下采样，得到平滑的抗锯齿边缘。
输出多尺寸 ICO（16/24/32/48/64/128/256）。
"""

import struct


# ---- 基础几何（浮点坐标，作用于超采样画布） ----

def in_rounded_rect(px, py, x0, y0, x1, y1, r):
    """点是否在圆角矩形内。"""
    if px < x0 or px > x1 or py < y0 or py > y1:
        return False
    # 四角圆角判定
    for cx, cy in ((x0 + r, y0 + r), (x1 - r, y0 + r), (x0 + r, y1 - r), (x1 - r, y1 - r)):
        if (px < x0 + r or px > x1 - r) and (py < y0 + r or py > y1 - r):
            # 仅在角区需要圆判定
            if (px - x0 < r and py - y0 < r):
                return (px - (x0 + r)) ** 2 + (py - (y0 + r)) ** 2 <= r * r
            if (x1 - px < r and py - y0 < r):
                return (px - (x1 - r)) ** 2 + (py - (y0 + r)) ** 2 <= r * r
            if (px - x0 < r and y1 - py < r):
                return (px - (x0 + r)) ** 2 + (py - (y1 - r)) ** 2 <= r * r
            if (x1 - px < r and y1 - py < r):
                return (px - (x1 - r)) ** 2 + (py - (y1 - r)) ** 2 <= r * r
    return True


def dist_to_seg(px, py, x1, y1, x2, y2):
    """点到线段距离。"""
    dx, dy = x2 - x1, y2 - y1
    L2 = dx * dx + dy * dy
    if L2 == 0:
        return ((px - x1) ** 2 + (py - y1) ** 2) ** 0.5
    t = max(0.0, min(1.0, ((px - x1) * dx + (py - y1) * dy) / L2))
    qx, qy = x1 + t * dx, y1 + t * dy
    return ((px - qx) ** 2 + (py - qy) ** 2) ** 0.5


def render(size, ss=4):
    """渲染 size×size 图标（BGRA 像素，预乘前的直接色值）。"""
    W = size * ss  # 超采样画布
    # 逻辑坐标（按 size 归一化，乘 ss）
    def S(v):
        return v / 256.0 * W

    # 调色
    grad_top = (0x37, 0x46, 0x58)    # 顶部 #374658
    grad_bot = (0x1F, 0x29, 0x35)    # 底部 #1F2935
    edge = (0x59, 0x6E, 0x87, 0x55)  # 半透明描边
    green = (0x9E, 0xC9, 0x7F)       # 终端绿 #9EC97F
    green_dim = (0x7A, 0xA8, 0x5E)   # 略暗（下划线）

    px = [[None] * W for _ in range(W)]
    r = S(52)
    half = 1.5  # 描边半宽（超采样像素）≈ 0.4 逻辑像素
    for y in range(W):
        for x in range(W):
            # 背景圆角矩形（含渐变）
            inside = in_rounded_rect(x + 0.5, y + 0.5, S(14), S(14), S(242), S(242), r)
            in_edge = in_rounded_rect(x + 0.5, y + 0.5, S(14) - half, S(14) - half,
                                      S(242) + half, S(242) + half, r + half)
            if not in_edge:
                px[y][x] = (0, 0, 0, 0)
                continue
            t = (y + 0.5 - S(14)) / (S(242) - S(14))
            cr = int(grad_top[0] + (grad_bot[0] - grad_top[0]) * t)
            cg = int(grad_top[1] + (grad_bot[1] - grad_top[1]) * t)
            cb = int(grad_top[2] + (grad_bot[2] - grad_top[2]) * t)
            if not inside:
                # 边缘带（描边）：与背景色混合
                a = edge[3] / 255.0
                px[y][x] = (int(cr * (1 - a) + edge[0] * a),
                            int(cg * (1 - a) + edge[1] * a),
                            int(cb * (1 - a) + edge[2] * a), 255)
                continue
            px[y][x] = (cr, cg, cb, 255)

    # ">" 提示符：两段粗线（chevron），带圆头
    w = S(26) / 2  # 线宽一半
    pts = [(S(86), S(76)), (S(140), S(128)), (S(86), S(180))]
    for y in range(W):
        for x in range(W):
            if px[y][x][3] == 0:
                continue
            d1 = dist_to_seg(x + 0.5, y + 0.5, pts[0][0], pts[0][1], pts[1][0], pts[1][1])
            d2 = dist_to_seg(x + 0.5, y + 0.5, pts[1][0], pts[1][1], pts[2][0], pts[2][1])
            if d1 <= w or d2 <= w:
                px[y][x] = (*green, 255)

    # "_" 光标横条（圆角）
    ux0, uy0, ux1, uy1 = S(158), S(160), S(212), S(184)
    ur = S(8)
    for y in range(W):
        for x in range(W):
            if px[y][x][3] == 0:
                continue
            if in_rounded_rect(x + 0.5, y + 0.5, ux0, uy0, ux1, uy1, ur):
                px[y][x] = (*green_dim, 255)

    # 盒式下采样
    out = []
    for y in range(size):
        row = []
        for x in range(size):
            r_ = g_ = b_ = a_ = 0
            for sy in range(ss):
                for sx in range(ss):
                    c = px[y * ss + sy][x * ss + sx]
                    r_ += c[0] * c[3]
                    g_ += c[1] * c[3]
                    b_ += c[2] * c[3]
                    a_ += c[3]
            n = ss * ss
            if a_ == 0:
                row.append((0, 0, 0, 0))
            else:
                row.append((r_ // a_, g_ // a_, b_ // a_, min(255, a_ // n + (a_ % n and 1 or 0))))
        out.append(row)
    return out


def encode_bgra(pxs):
    h = len(pxs)
    w = len(pxs[0])
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
        data = encode_bgra(render(s))
        bih = struct.pack("<IiiHHIIiiII", 40, s, 2 * s, 1, 32, 0, len(data), 0, 0, 0, 0)
        images.append((s, bih + data))
    header = struct.pack("<HHH", 0, 1, len(images))
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
    make_ico([16, 24, 32, 48, 64, 128, 256], "rkttl.ico")
