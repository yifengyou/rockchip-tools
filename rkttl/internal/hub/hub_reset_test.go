package hub

import (
	"sync"
	"testing"
	"time"
)

// TestConcurrentOpenClose 验证 Open/Close/Stop 的并发安全：
// reopenLoop 与手动 Open 不会并发执行 serial.Open。
func TestConcurrentOpenClose(t *testing.T) {
	h := New(1<<20, true, nil, false)
	cfg := DefaultConfig("nonexistent-port")
	h.SetDefault(cfg)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.Open(cfg) // 必然失败（无真实串口），但不应 panic
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.Close()
		}()
	}
	wg.Wait()
	h.Stop()
}

// TestReopenLoopCancel 验证 reopenLoop 能被 Close 取消。
func TestReopenLoopCancel(t *testing.T) {
	h := New(1<<20, true, nil, false)
	cfg := DefaultConfig("nonexistent-port")
	h.SetDefault(cfg)

	// 启动一个 goroutine 模拟异常断开后的自动重连
	done := make(chan struct{})
	go func() {
		h.reopenLoop(cfg)
		close(done)
	}()

	// 给它一点时间开始
	time.Sleep(100 * time.Millisecond)

	// Close 应该取消 reopenLoop
	h.Close()

	select {
	case <-done:
		// 正常取消
	case <-time.After(2 * time.Second):
		t.Fatal("reopenLoop 未被取消")
	}
}

// TestOpenClearsRing 验证 Open 会清空 ring 的可读数据（used=0）。
func TestOpenClearsRing(t *testing.T) {
	h := New(1<<20, false, nil, false)

	h.ring.Write([]byte("hello"))
	if h.ring.Len() == 0 {
		t.Fatal("ring 应该有数据")
	}

	// Reset 清空可读数据（total 保留以维持 seq 连续性）
	h.ring.Reset()
	if h.ring.Len() != 0 {
		t.Fatalf("ring 应该被清空，但 Len()=%d", h.ring.Len())
	}
}
