package hub

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestRingRandomStream 用随机写入验证不变量：
// 任意 since 游标按 1 字节步进读取，必须完整还原写入流。
func TestRingRandomStream(t *testing.T) {
	r := NewRingBuffer(4096)
	rng := rand.New(rand.NewSource(42))

	var stream []byte // 参考流（与写入顺序一致）
	for round := 0; round < 2000; round++ {
		n := 1 + rng.Intn(300)
		chunk := make([]byte, n)
		for i := range chunk {
			chunk[i] = byte(rng.Intn(256))
		}
		stream = append(stream, chunk...)
		r.Write(chunk)

		// 每轮从随机 since 逐块读到底，校验与参考流一致
		if total := r.Total(); total > 0 {
			base := total - uint64(r.Len())
			start := uint64(rng.Int63n(int64(total)))
			if start < base {
				start = base
			}
			var got []byte
			since := start
			for since < total {
				res := r.ReadSince(since, 0, 0)
				if len(res.Data) == 0 {
					break
				}
				got = append(got, res.Data...)
				since = res.Seq
			}
			// got 应等于 stream[start .. total)
			if !bytes.Equal(got, stream[len(stream)-int(total-start):]) {
				t.Fatalf("round %d: 流不一致 start=%d got=%dB want=%dB",
					round, start, len(got), int(total-start))
			}
		}
	}
}

// TestRingChunkedIntegrity 用小容量 + 大块写入验证覆盖路径。
func TestRingChunkedIntegrity(t *testing.T) {
	for _, size := range []int{4096, 8192} {
		for _, chunk := range []int{1, 7, 100, 4095, 4096, 4097, 9000} {
			r := NewRingBuffer(size)
			var stream []byte
			for i := 0; i < 50; i++ {
				c := bytes.Repeat([]byte{byte(i)}, chunk)
				stream = append(stream, c...)
				r.Write(c)
			}
			// 只校验缓冲内最近 size 字节与参考流尾部一致
			total := r.Total()
			base := total - uint64(r.Len())
			res := r.ReadSince(base, 0, 0)
			if !bytes.Equal(res.Data, stream[len(stream)-r.Len():]) {
				t.Fatalf("size=%d chunk=%d: 缓冲内容与参考流不一致", size, chunk)
			}
		}
	}
}
