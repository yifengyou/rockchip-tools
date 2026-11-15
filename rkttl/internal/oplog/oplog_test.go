package oplog

import (
	"strings"
	"testing"
	"time"
)

func TestLogBasic(t *testing.T) {
	l := New(100)
	if l.Count() != 0 || l.Text() != "" {
		t.Fatal("初始应为空")
	}
	l.Log("串口已打开: %s @ %d", "COM3", 1500000)
	l.Log("远端(%s) 导出会话日志（%d 字节）", "192.168.1.20:53124", 1024)

	txt := l.Text()
	lines := strings.Split(txt, "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 条，得到 %d", len(lines))
	}
	// 每条带 HH:MM:SS 时间戳前缀
	for _, ln := range lines {
		if len(ln) < 9 || ln[2] != ':' || ln[5] != ':' {
			t.Fatalf("缺少时间戳前缀: %q", ln)
		}
	}
	if !strings.Contains(lines[0], "串口已打开: COM3 @ 1500000") {
		t.Fatalf("内容: %q", lines[0])
	}
	if !strings.Contains(lines[1], "192.168.1.20:53124") {
		t.Fatalf("内容: %q", lines[1])
	}
}

func TestLogCapacity(t *testing.T) {
	l := New(50) // 容量下限 50
	for i := 0; i < 120; i++ {
		l.Log("entry %03d", i)
	}
	if got := l.Count(); got != 50 {
		t.Fatalf("应保留 50 条，得到 %d", got)
	}
	// 保留的是最新的
	if !strings.Contains(l.Text(), "entry 119") || strings.Contains(l.Text(), "entry 069") {
		t.Fatal("应保留最新、丢弃最旧")
	}
}

func TestLogChangedSignal(t *testing.T) {
	l := New(100)
	ch := l.Changed()
	select {
	case <-ch:
		t.Fatal("初始不应有信号")
	default:
	}
	l.Log("hello")
	select {
	case <-ch:
	default:
		t.Fatal("Log 后旧通道应被关闭")
	}
	// 新通道等待下一次
	ch2 := l.Changed()
	select {
	case <-ch2:
		t.Fatal("新通道不应提前关闭")
	default:
	}
	l.Log("world")
	select {
	case <-ch2:
	default:
		t.Fatal("第二次 Log 应关闭新通道")
	}
}

func TestLogConcurrent(t *testing.T) {
	l := New(200)
	done := make(chan struct{})
	for g := 0; g < 8; g++ {
		go func(g int) {
			for i := 0; i < 100; i++ {
				l.Log("g%d-%d", g, i)
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 8; g++ {
		<-done
	}
	if l.Count() != 200 {
		t.Fatalf("并发后应恰好 200 条，得到 %d", l.Count())
	}
	_ = time.Now
}
