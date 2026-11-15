package hub

import (
	"sync"
	"time"
)

// RingBuffer 是串口接收数据的环形缓冲区。
//
// 它按全局字节序号(seq)索引：total 记录历史累计接收的字节数，
// 缓冲区只保留最近 size 字节，满时覆盖最旧数据。
//
// 读取是非破坏性的：多个客户端可以携带各自的 since 游标
// 独立消费同一路串口数据流（类似 tmux 的多端 attach）。
type RingBuffer struct {
	mu     sync.Mutex
	notify chan struct{} // 每次写入后被 close 并替换，用于唤醒等待者

	buf       []byte
	size      int
	head      int    // 环形缓冲中最旧字节的下标
	used      int    // 当前有效字节数
	total     uint64 // 历史累计写入字节数（最新字节的 seq）
	dropTotal uint64 // 因覆盖而丢弃的最旧字节数（累计）
}

// ReadResult 是一次非破坏性读取的结果。
type ReadResult struct {
	Data      []byte // 读到的数据（since..Seq 区间，受 max 限制）
	Seq       uint64 // 本次读取窗口末端的序号，作为下一次读取的 since
	Truncated bool   // since 早于缓冲区起点：部分历史数据已被覆盖丢失
	Buffered  int    // 当前缓冲区中的字节数
}

// NewRingBuffer 创建容量为 size 的环形缓冲（最小 4096）。
func NewRingBuffer(size int) *RingBuffer {
	if size < 4096 {
		size = 4096
	}
	return &RingBuffer{
		notify: make(chan struct{}),
		buf:    make([]byte, size),
		size:   size,
	}
}

// Write 写入一批数据，返回写入后的最新 seq（累计字节数）。
func (r *RingBuffer) Write(p []byte) uint64 {
	if len(p) == 0 {
		r.mu.Lock()
		t := r.total
		r.mu.Unlock()
		return t
	}
	r.mu.Lock()
	if len(p) > r.size {
		// 数据比整个缓冲区还大：只保留最新的 size 字节
		r.dropTotal += uint64(r.used + len(p) - r.size)
		copy(r.buf, p[len(p)-r.size:])
		r.head = 0
		r.used = r.size
		r.total += uint64(len(p))
	} else {
		for len(p) > 0 {
			if r.used == r.size {
				// 满了：丢弃最旧一个字节
				r.head = (r.head + 1) % r.size
				r.used--
				r.dropTotal++
			}
			w := (r.head + r.used) % r.size
			// 单次复制的长度不能超过物理连续空闲区，也不能超过总空闲数
			n := r.size - w
			if free := r.size - r.used; n > free {
				n = free
			}
			if n > len(p) {
				n = len(p)
			}
			copy(r.buf[w:w+n], p[:n])
			r.used += n
			r.total += uint64(n)
			p = p[n:]
		}
	}
	t := r.total
	notify := r.notify
	r.notify = make(chan struct{})
	r.mu.Unlock()
	close(notify)
	return t
}

// ReadSince 非破坏性地读取 since 之后的数据。
//
//   - since 通常取上一次返回的 Seq；传 0 表示从当前缓冲区最旧的数据开始；
//   - max 限制本次返回的最大字节数（0 表示不限制）；
//   - wait 为缓冲区中尚无 since 之后数据时的最长等待时间（<=0 表示立即返回）。
//
// 若 since 早于缓冲区起点，则从起点开始返回并置 Truncated=true。
func (r *RingBuffer) ReadSince(since uint64, max int, wait time.Duration) ReadResult {
	deadline := time.Now().Add(wait)
	for {
		r.mu.Lock()
		base := r.total - uint64(r.used)
		truncated := false
		if since < base {
			since = base
			truncated = true
		}
		if since > r.total {
			since = r.total // 防御：客户端不应持有未来的 seq
		}
		if since < r.total {
			n := r.total - since
			if max > 0 && uint64(max) < n {
				n = uint64(max)
			}
			out := make([]byte, n)
			start := (r.head + int(since-base)) % r.size
			for k := 0; k < int(n); {
				w := (start + k) % r.size
				m := r.size - w
				if m > int(n)-k {
					m = int(n) - k
				}
				copy(out[k:], r.buf[w:w+m])
				k += m
			}
			res := ReadResult{Data: out, Seq: since + n, Truncated: truncated, Buffered: r.used}
			r.mu.Unlock()
			return res
		}
		// 尚无新数据
		nch := r.notify
		buffered := r.used
		r.mu.Unlock()
		if wait <= 0 || !time.Now().Before(deadline) {
			return ReadResult{Data: nil, Seq: since, Truncated: truncated, Buffered: buffered}
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-nch:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Reset 丢弃缓冲区内所有数据，但保留 seq 连续性（total 不变）。
func (r *RingBuffer) Reset() {
	r.mu.Lock()
	r.head, r.used = 0, 0
	r.mu.Unlock()
}

// Len 返回当前缓冲区中的字节数。
func (r *RingBuffer) Len() int {
	r.mu.Lock()
	n := r.used
	r.mu.Unlock()
	return n
}

// Total 返回历史累计写入的字节数（最新 seq）。
func (r *RingBuffer) Total() uint64 {
	r.mu.Lock()
	t := r.total
	r.mu.Unlock()
	return t
}

// Dropped 返回因缓冲区覆盖而丢弃的累计字节数。
func (r *RingBuffer) Dropped() uint64 {
	r.mu.Lock()
	d := r.dropTotal
	r.mu.Unlock()
	return d
}
