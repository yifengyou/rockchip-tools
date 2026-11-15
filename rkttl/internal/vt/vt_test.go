package vt

import (
	"strings"
	"testing"
)

func lines(s string) []string { return strings.Split(s, "\n") }

// 基本流式行为：\r\n 换行、\r 覆盖重写当前行（进度条）。
func TestBasicStreamAndCarriageReturn(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("boot 10%\rboot 50%\rboot 99%\r\nOK\n")
	got := lines(s.Dump())
	if got[0] != "boot 99%" {
		t.Fatalf("覆盖重写失败: %q", got[0])
	}
	if got[1] != "OK" || len(got) != 2 {
		t.Fatalf("换行错误: %q", got)
	}
}

// busybox top 风格：每帧 CUP(1,1)+清屏重绘 —— 最终帧应完整可见；
// 持续多帧才判定全屏（单帧不触发）。
func TestBusyboxTopFrames(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("\x1b[H\x1b[J  1 [I] root 20% cpu\r\nMEM 100M/1G\r\n")
	s.Feed("\x1b[H\x1b[J  2 [I] root 35% cpu\r\nMEM 200M/1G\r\n")
	got := lines(s.Dump())
	if got[0] != "  2 [I] root 35% cpu" || got[1] != "MEM 200M/1G" {
		t.Fatalf("帧覆盖错误: %q", got[:2])
	}
	if !s.FullScreenActive() {
		t.Fatal("持续帧重绘应触发全屏判定（2 帧 × 2 序列 = 4 次）")
	}
}

// 普通 shell 场景：bash 行编辑/单次 clear 不会误触发全屏判定。
func TestNoFalsePositiveForShell(t *testing.T) {
	s := NewScreen(80, 24)
	// 单次 clear（\x1b[H\x1b[2J）+ readline 行内重绘（\r \x1b[K CUF）
	s.Feed("root@x:~# \x1b[H\x1b[2Jroot@x:~# ls\r\nfile1\r\nroot@x:~# \r\x1b[K\x1b[10C")
	if s.FullScreenActive() {
		t.Fatal("单次 clear 不应触发全屏判定")
	}
	// bash 的 bracketed paste 等私有模式也不触发
	s.Feed("\x1b[?2004h")
	if s.FullScreenActive() {
		t.Fatal("bracketed paste 不应触发全屏判定")
	}
}

// 全屏判定衰减：序列停止后退出（GUI 恢复流式）。
func TestFullScreenDecay(t *testing.T) {
	s := NewScreen(80, 24)
	// 快速喂 4 帧触发
	for i := 0; i < 4; i++ {
		s.Feed("\x1b[H\x1b[2Jtop\r\n")
	}
	if !s.FullScreenActive() {
		t.Fatal("持续帧应为全屏")
	}
	// 模拟 2.6 秒后（时间窗口滑出）
	s.marks = nil
	if s.FullScreenActive() {
		t.Fatal("衰减后应退出全屏判定")
	}
}

// htop 风格：alt screen + 光标定位 + 滚动区域 + 局部 EL 更新。
func TestHtopAltScreen(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("shell> htop\r\n")
	s.Feed("\x1b[?1049h\x1b[H\x1b[2J") // 进入备用屏，清屏
	s.Feed("  1  [|||   20%]\r\n  2  [|     5%]\r\n  3  [||    10%]\r\n")
	s.Feed("\x1b[2;1H\x1b[K  2  [|||||  35%]") // 定位第2行，清行后重写

	dump := s.Dump()
	got := lines(dump)
	if len(got) != 24 { // 备用屏固定 24 行
		t.Fatalf("备用屏应为 24 行, got %d", len(got))
	}
	if got[0] != "  1  [|||   20%]" {
		t.Fatalf("行1: %q", got[0])
	}
	if got[1] != "  2  [|||||  35%]" {
		t.Fatalf("CUP+EL 重写失败: %q", got[1])
	}
	if got[2] != "  3  [||    10%]" {
		t.Fatalf("行3: %q", got[2])
	}
	if !s.AltActive() {
		t.Fatal("应处于备用屏")
	}

	// 退出 alt screen：恢复主屏（htop 退出后的 shell 提示符）
	s.Feed("\x1b[?1049l")
	if s.AltActive() {
		t.Fatal("应退出备用屏")
	}
	dump = s.Dump()
	if !strings.Contains(dump, "shell> htop") {
		t.Fatalf("主屏内容应恢复: %q", dump)
	}
}

// htop 主循环里的滚区滚动（状态栏固定底部）。
func TestScrollRegion(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("\x1b[?1049h\x1b[1;23r") // 备用屏，滚区 1..23（底部状态栏固定）
	s.Feed("\x1b[Hline1\r\nline2\r\n")
	s.Feed("\x1b[22;1H") // 光标到滚区底
	for i := 3; i <= 26; i++ {
		s.Feed("line" + itoa(i) + "\r\n") // 出底 → 滚区内上滚
	}
	s.Feed("\x1b[24;1HSTATUS-BAR")
	got := lines(s.Dump())
	if got[23] != "STATUS-BAR" {
		t.Fatalf("滚区外底部行应保持: %q", got[23])
	}
	if !strings.Contains(got[21], "line26") {
		t.Fatalf("滚区末行应为最新: %q", got[21])
	}
	if got[0] != "line5" {
		t.Fatalf("滚区顶部应为 line5（旧行滚走）: %q", got[0])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 任意分块 Feed 与整块 Feed 结果一致（跨块序列缓冲）。
func TestChunkInvariance(t *testing.T) {
	mk := func() string {
		s := NewScreen(80, 24)
		s.Feed("=> ls -l\r\n")
		s.Feed("\x1b[?1049h\x1b[H\x1b[2J")
		s.Feed("PID  CPU MEM\r\n")
		s.Feed("123  20%  1%\r\n456")
		s.Feed("  3%  2%\r\n\x1b[3;1H\x1b[K789  9%  1%")
		s.Feed("\x1b[?1049l")
		s.Feed("=> \x1b[32mOK\x1b[0m done\r\n")
		return s.Dump()
	}
	want := mk()
	for chunk := 1; chunk <= 9; chunk++ {
		s := NewScreen(80, 24)
		full := "=> ls -l\r\n\x1b[?1049h\x1b[H\x1b[2JPID  CPU MEM\r\n123  20%  1%\r\n456  3%  2%\r\n\x1b[3;1H\x1b[K789  9%  1%\x1b[?1049l=> \x1b[32mOK\x1b[0m done\r\n"
		rs := []rune(full)
		for i := 0; i < len(rs); i += chunk {
			end := i + chunk
			if end > len(rs) {
				end = len(rs)
			}
			s.Feed(string(rs[i:end]))
		}
		if s.Dump() != want {
			t.Fatalf("分块(%d)与整块不一致:\n整块: %q\n分块: %q", chunk, want, s.Dump())
		}
	}
}

// 孤立 ESC 在块尾：缓冲到下一块（不产生垃圾字符）。
func TestTrailingEscBuffered(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("line\x1b")
	s.Feed("[2Krest") // \x1b[2K 清整行（光标不动，仍在 col4）
	got := lines(s.Dump())
	// 真终端语义：EL2 不移动光标，rest 从原光标位置写起
	if got[0] != "    rest" {
		t.Fatalf("跨块 CSI 处理错误: %q", got[0])
	}
}

// 主屏历史保留：CUP 定位不影响 scrollback。
func TestPrimaryHistory(t *testing.T) {
	s := NewScreen(80, 24)
	for i := 1; i <= 30; i++ {
		s.Feed("row" + itoa(i) + "\r\n")
	}
	s.Feed("\x1b[1;1Hoverwrite") // 定位到视口首行覆盖
	dump := s.Dump()
	got := lines(dump)
	if len(got) < 20 || len(got) > 24 { // 可视区（尾部空行已裁剪）
		t.Fatalf("视口行数异常: %d", len(got))
	}
	if !strings.HasPrefix(got[0], "overwrite") {
		t.Fatalf("视口首行: %q", got[0])
	}
	if !strings.Contains(strings.Join(got, "\n"), "row30") {
		t.Fatalf("视口应含 row30: %q", got)
	}
	// 历史行仍在（视口之上），通过全量 Dump 不可见但未丢失：
	if len(s.lines) < 30 {
		t.Fatalf("历史行丢失: %d", len(s.lines))
	}
}

// 光标移动 CUU/CUD/CUF/CUB/CHA。
func TestCursorMoves(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("\x1b[?1049h")
	s.Feed("\x1b[10;5H") // (行10,列5)
	s.Feed("AB")
	s.Feed("\x1b[1D")                // 左移1
	s.Feed("X")                      // 覆盖 B
	s.Feed("\x1b[10;1H\x1b[C\x1b[C") // 行首右移2
	s.Feed("Y")                      // 覆盖 A？(列3)
	got := lines(s.Dump())
	// (10,5) 写 AB → "    AB"；1D → col5；X 覆盖 B 位置 → "    AX"；
	// 10;1 后右移 2 → col2；Y 覆盖空格 → "  Y AX"
	if got[9] != "  Y AX" {
		t.Fatalf("光标移动结果: %q", got[9])
	}
}

// 保存/恢复光标（DECSC/DECRC 与 CSI s/u）。
func TestSaveRestoreCursor(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("\x1b[?1049h")
	s.Feed("\x1b[5;3H")
	s.Feed("\x1b7") // 保存
	s.Feed("\x1b[10;1Hmoved\r\n")
	s.Feed("\x1b8") // 恢复
	s.Feed("Z")     // 应写回 (5,3)
	got := lines(s.Dump())
	if !strings.HasPrefix(got[4], "  Z") {
		t.Fatalf("恢复光标写入: %q", got[4])
	}
}

// 折行：超过列宽自动换行。
func TestWrap(t *testing.T) {
	s := NewScreen(80, 24)
	s.Cols = 10 // 缩窄列宽以触发折行（NewScreen 有 ≥20 列的最小保护）
	s.Feed("0123456789ABC")
	got := lines(s.Dump())
	if got[0] != "0123456789" || got[1] != "ABC" {
		t.Fatalf("折行: %q", got[:2])
	}
}

// \b 只移动光标不删除（真终端语义）。
func TestBackspaceMove(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("abc\b\bZ")
	got := lines(s.Dump())
	if got[0] != "aZc" {
		t.Fatalf("退格覆盖: %q", got[0])
	}
}

// TAB 推进到 8 的倍数列。
func TestTab(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("a\tb")
	got := lines(s.Dump())
	if got[0] != "a       b" {
		t.Fatalf("TAB: %q", got[0])
	}
}

// 随机模糊：任意字节序列不崩溃、不 panic、分块不变。
func TestFuzzNoPanicAndChunkInvariant(t *testing.T) {
	rng := uint32(42)
	next := func() uint32 { rng = rng*1103515245 + 12345; return rng & 0x7fffffff }
	gen := func() string {
		var b []rune
		for i := 0; i < 400; i++ {
			switch next() % 10 {
			case 0:
				b = append(b, 0x1b)
			case 1:
				b = append(b, rune('0'+next()%10))
			case 2:
				b = append(b, []rune(";[HJKmABCDrsu?hl<>")...)
			case 3:
				b = append(b, '\n')
			case 4:
				b = append(b, '\r')
			case 5:
				b = append(b, '\b')
			case 6:
				b = append(b, '\t')
			case 7:
				b = append(b, ' ')
			case 8:
				b = append(b, rune(0x4e00+next()%100)) // 中文
			default:
				b = append(b, rune(0x20+next()%0x5f))
			}
		}
		return string(b)
	}
	for round := 0; round < 100; round++ {
		input := gen()
		full := NewScreen(80, 24)
		full.Feed(input)
		chunked := NewScreen(80, 24)
		for i := 0; i < len(input); i += 7 {
			e := i + 7
			if e > len(input) {
				e = len(input)
			}
			chunked.Feed(input[i:e])
		}
		if full.Dump() != chunked.Dump() {
			t.Fatalf("round %d 分块不一致", round)
		}
		_ = full.FullScreenActive()
	}
}
