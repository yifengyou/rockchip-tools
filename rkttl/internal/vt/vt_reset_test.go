// 用真实 xterm/busybox reset 命令的完整 terminfo 序列复现测试。
package vt

import (
	"strings"
	"testing"
)

// realResetSeq 是 xterm 类终端上 `reset`（tput init）的典型完整输出：
// RIS + 软复位 + 私有模式 + 字符集 + 清屏 + 滚区/属性重置 + 清制表位。
const realResetSeq = "\x1bc" + // rs1: RIS
	"\x1b[!p" + // DECSTR 软复位（'!' 前缀，非常规解析路径）
	"\x1b[?3;4l" + // 私有模式：80列、平滑滚动
	"\x1b[4l" + // IRM 关闭
	"\x1b>" + // keypad 应用模式关
	"\x1b(B\x1b)0" + // G0/G1 字符集声明
	"\x0f" + // SI
	"\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1006l" + // 鼠标追踪全关
	"\x1b[?25h" + // 光标显示
	"\x1b[39;49m\x1b[0m" + // 颜色复位
	"\r\r\n" + // 回车换行
	"\x1b[2J\x1b[H" + // 清屏 + 光标回原点
	"\x1b[?7h" + // DECAWM 自动折行开
	"\x1b[?1;4;6l" + // 光标键/插入/起源模式复位
	"\x1b[r" + // 滚动区域复位（全屏）
	"\x1b[m" + // SGR 复位
	"\x1b[3g" + // 清除制表位
	"root@armbian:~# "

func TestRealResetSequence(t *testing.T) {
	s := NewScreen(80, 24)
	s.Feed("root@armbian:~# reset\r\n")
	s.Feed(realResetSeq)
	if !s.ConsumeClearRequest() {
		t.Fatal("reset 序列应产生清屏请求")
	}
	dump := s.Dump()
	if !strings.Contains(dump, "root@armbian:~#") {
		t.Fatalf("reset 后应显示新提示符: %q", dump)
	}
	if strings.Contains(dump, "reset") && strings.Count(dump, "reset") > 1 {
		t.Fatalf("reset 旧内容应被清空: %q", dump)
	}
	// 后续正常交互
	s.Feed("ls\r\nfile1\r\nroot@armbian:~# ")
	if !strings.Contains(s.Dump(), "file1") {
		t.Fatalf("reset 后正常交互失败: %q", s.Dump())
	}
	// 不应误入全屏
	if s.FullScreenActive() {
		t.Fatal("reset 不应触发全屏判定")
	}
	// 分块不变性（含后续交互的完整序列对比）
	full := NewScreen(80, 24)
	full.Feed("root@armbian:~# reset\r\n")
	full.Feed(realResetSeq)
	full.Feed("ls\r\nfile1\r\nroot@armbian:~# ")
	chunked := NewScreen(80, 24)
	all := "root@armbian:~# reset\r\n" + realResetSeq + "ls\r\nfile1\r\nroot@armbian:~# "
	rs := []rune(all)
	for i := 0; i < len(rs); i += 3 {
		e := i + 3
		if e > len(rs) {
			e = len(rs)
		}
		chunked.Feed(string(rs[i:e]))
	}
	if chunked.Dump() != full.Dump() {
		t.Fatalf("分块不一致:\n%q\n%q", full.Dump(), chunked.Dump())
	}
}

// 并发安全：一个 goroutine 持续 Feed，另一个持续 Dump/状态查询
// （GUI 的 flushPending 与 UI 闭包真实场景）。-race 下必须无告警。
func TestConcurrentFeedAndDump(t *testing.T) {
	s := NewScreen(80, 24)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			s.Feed("line " + itoa(i%97) + "\r\n")
			s.Feed(realResetSeq) // 大改 lines，扩大竞态窗口
		}
	}()
	for i := 0; i < 2000; i++ {
		_ = s.Dump()
		_ = s.FullScreenActive()
		_ = s.ConsumeClearRequest()
	}
	<-done
}
