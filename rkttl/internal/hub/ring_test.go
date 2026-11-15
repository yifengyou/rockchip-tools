package hub

import (
	"bytes"
	"testing"
	"time"
)

func TestRingBasicWriteRead(t *testing.T) {
	r := NewRingBuffer(4096)
	if got := r.Write([]byte("hello")); got != 5 {
		t.Fatalf("total after write = %d, want 5", got)
	}
	res := r.ReadSince(0, 0, 0)
	if string(res.Data) != "hello" {
		t.Fatalf("data = %q, want hello", res.Data)
	}
	if res.Seq != 5 {
		t.Fatalf("seq = %d, want 5", res.Seq)
	}
	if res.Truncated {
		t.Fatal("不应 truncated")
	}
	// 再读一次应无新数据（非破坏性）
	res = r.ReadSince(5, 0, 0)
	if len(res.Data) != 0 {
		t.Fatalf("重复读取得到 %q", res.Data)
	}
	// since=0 仍能读到全部
	res = r.ReadSince(0, 0, 0)
	if string(res.Data) != "hello" {
		t.Fatalf("重读历史 = %q", res.Data)
	}
}

func TestRingMultipleIndependentReaders(t *testing.T) {
	// 模拟 tmux 多端 attach：两个读者各自持游标，互不影响
	r := NewRingBuffer(4096)
	r.Write([]byte("abc"))
	r.Write([]byte("def"))

	a := r.ReadSince(0, 0, 0) // 读者 A 从头读
	if string(a.Data) != "abcdef" {
		t.Fatalf("A 读到 %q", a.Data)
	}
	b := r.ReadSince(3, 0, 0) // 读者 B 只读后半
	if string(b.Data) != "def" {
		t.Fatalf("B 读到 %q", b.Data)
	}
	// 新数据后两个游标各自推进
	r.Write([]byte("ghi"))
	if got := string(r.ReadSince(a.Seq, 0, 0).Data); got != "ghi" {
		t.Fatalf("A 续读 %q", got)
	}
	if got := string(r.ReadSince(b.Seq, 0, 0).Data); got != "ghi" {
		t.Fatalf("B 续读 %q", got)
	}
}

func TestRingOverwriteOldest(t *testing.T) {
	r := NewRingBuffer(4096)
	// 预填满
	fill := bytes.Repeat([]byte("x"), 4096)
	r.Write(fill)
	// 覆盖 10 字节最旧数据
	r.Write([]byte("0123456789"))

	res := r.ReadSince(0, 0, 0)
	if !res.Truncated {
		t.Fatal("应 truncated")
	}
	if res.Seq != 4096+10 {
		t.Fatalf("seq = %d", res.Seq)
	}
	// 数据应从"被覆盖后"开始，且为 y*4086 + 0123456789
	want := append(bytes.Repeat([]byte("x"), 4086), []byte("0123456789")...)
	if !bytes.Equal(res.Data, want) {
		t.Fatalf("数据长度=%d want=%d；尾部=%q", len(res.Data), len(want), res.Data[len(res.Data)-10:])
	}
}

func TestRingOversizeWrite(t *testing.T) {
	r := NewRingBuffer(4096)
	big := bytes.Repeat([]byte("z"), 5000)
	if got := r.Write(big); got != 5000 {
		t.Fatalf("seq = %d, want 5000", got)
	}
	res := r.ReadSince(0, 0, 0)
	if !res.Truncated {
		t.Fatal("应 truncated")
	}
	if len(res.Data) != 4096 {
		t.Fatalf("保留 %d 字节, want 4096", len(res.Data))
	}
	if !bytes.Equal(res.Data, big[904:]) {
		t.Fatal("应保留最新 4096 字节")
	}
}

func TestRingMaxLimit(t *testing.T) {
	r := NewRingBuffer(4096)
	r.Write([]byte("abcdef"))
	res := r.ReadSince(0, 3, 0)
	if string(res.Data) != "abc" || res.Seq != 3 {
		t.Fatalf("data=%q seq=%d", res.Data, res.Seq)
	}
	res = r.ReadSince(3, 0, 0)
	if string(res.Data) != "def" {
		t.Fatalf("续读 %q", res.Data)
	}
}

func TestRingWaitForNewData(t *testing.T) {
	r := NewRingBuffer(4096)
	// 无数据、不等待 → 立即空返回
	res := r.ReadSince(0, 0, 0)
	if len(res.Data) != 0 {
		t.Fatal("应无数据")
	}
	// 无数据、等待 200ms → 超时返回
	start := time.Now()
	res = r.ReadSince(0, 0, 200*time.Millisecond)
	if len(res.Data) != 0 || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("应等到超时: n=%d dt=%v", len(res.Data), time.Since(start))
	}
	// 等待期间写入 → 立刻唤醒
	go func() {
		time.Sleep(50 * time.Millisecond)
		r.Write([]byte("wake"))
	}()
	start = time.Now()
	res = r.ReadSince(0, 0, 2*time.Second)
	if string(res.Data) != "wake" {
		t.Fatalf("data=%q", res.Data)
	}
	if time.Since(start) > time.Second {
		t.Fatal("应被写入唤醒而非等满")
	}
}

func TestRingResetKeepsSeq(t *testing.T) {
	r := NewRingBuffer(4096)
	r.Write([]byte("abc"))
	r.Reset()
	if r.Len() != 0 {
		t.Fatal("缓冲应清空")
	}
	if r.Total() != 3 {
		t.Fatal("total(seq) 应保持连续")
	}
	res := r.ReadSince(0, 0, 0)
	if len(res.Data) != 0 {
		t.Fatal("清空后无数据")
	}
	r.Write([]byte("d"))
	res = r.ReadSince(0, 0, 0)
	if string(res.Data) != "d" {
		t.Fatalf("清空后老游标只应看到新数据, got %q", res.Data)
	}
}
