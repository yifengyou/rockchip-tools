#!/usr/bin/env python3
"""生成 rkttl 应用图标 v3（超采样抗锯齿，无第三方依赖）。

设计：深色石板底 + 终端绿信号波纹（串口数据流的现代演绎）。
灵感：示波器波形 + 终端提示符 ">_" 的融合，暗示"实时数据流"的
专业串口终端工具。比 v2 的静态 chevron 更有动感和识别度。

4 倍超采样后盒式下采样，得到平滑的抗锯齿边缘。
输出多尺寸 ICO（16/24/32/48/64/128/256）。
"""

import struct


def in_rounded_rect(px, py, x0, y0, x1, y1, r):
    """点是否在圆角矩形内。"""
    if px < x0 or px > x1 or py < y0 or py > y1:
        return False
    for cx, cy in ((x0 + r, y0 + r), (x1 - r, y0 + r), (x0 + r, y1 - r), (x1 - r, y1 - r)):
        if (px < x0 + r or px > x1 - r) and (py < y0 + r or py > y1 - r):
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
    """渲染 size×size 图标（BGRA 像素）。"""
    W = size * ss
    def S(v):
        return v / 256.0 * W

    # 调色板（现代深色 + 终端绿）
    grad_top = (0x2D, 0x3A, 0x4D)    # 顶部 #2D3A4D 深蓝灰
    grad_bot = (0x1A, 0x22, 0x2E)    # 底部 #1A222E 近黑
    edge = (0x5A, 0x7A, 0xA8, 0x66)  # 半透明描边（带蓝）
    green = (0x6E, 0xD0, 0x9A)       # 终端绿 #6ED09A（亮）
    green_dim = (0x4A, 0xA0, 0x70)   # 暗绿 #4AA070

    px = [[None] * W for _ in range(W)]
    r = S(56)
    half = 1.5
    for y in range(W):
        for x in range(W):
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
                a = edge[3] / 255.0
                px[y][x] = (int(cr * (1 - a) + edge[0] * a),
                            int(cg * (1 - a) + edge[1] * a),
                            int(cb * (1 - a) + edge[2] * a), 255)
                continue
            px[y][x] = (cr, cg, cb, 255)

    # 信号波纹：三条水平起伏的线（示意串口数据流）
    # 三条线不同相位，形成"实时数据"的动感
    waves = [
        # (y_offset, amplitude, phase, color, thickness)
        (S(88),  S(28), 0.0, green,     S(14)),
        (S(128), S(24), 0.7, green,     S(12)),
        (S(168), S(20), 1.4, green_dim, S(10)),
    ]
    x_start, x_end = S(46), S(210)
    for y_off, amp, phase, color, thick in waves:
        half_t = thick / 2
        # 采样波形路径为线段
        steps = 32
        pts = []
        for i in range(steps + 1):
            t = i / steps
            x = x_start + (x_end - x_start) * t
            # 平滑的方波近似（方波 = 串口数字信号的特征）
            import math
            s = math.sin(t * 4 * math.pi + phase * math.pi)
            # 把 sin 锐化为方波感（tanh 增强）
            s_sq = math.tanh(s * 2.5)
            y = y_off + s_sq * amp
            pts.append((x, y))
        # 渲染：点到路径距离
        for y in range(W):
            for x in range(W):
                if px[y][x][3] == 0:
                    continue
                d_min = 1e9
                for i in range(steps):
                    d = dist_to_seg(x + 0.5, y + 0.5,
                                    pts[i][0], pts[i][1],
                                    pts[i + 1][0], pts[i + 1][1])
                    if d < d_min:
                        d_min = d
                if d_min <= half_t:
                    # 抗锯齿：按距离衰减亮度
                    alpha = max(0.0, 1.0 - (d_min / half_t) * 0.3)
                    cr, cg, cb = px[y][x][0], px[y][x][1], px[y][x][2]
                    px[y][x] = (
                        int(color[0] * alpha + cr * (1 - alpha)),
                        int(color[1] * alpha + cg * (1 - alpha)),
                        int(color[2] * alpha + cb * (1 - alpha)),
                        255,
                    )

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
