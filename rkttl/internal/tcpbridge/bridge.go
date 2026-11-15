// Package tcpbridge 把串口以裸 TCP 字节流的方式共享出去：
// 所有 TCP 客户端都会收到串口的全部输出，也都可以向串口发送数据，
// 与网页控制台 / 本地 GUI 共享同一路串口（tmux 式多端 attach）。
//
// 用法示例：
//
//	nc 192.168.1.10 9999              # 交互
//	socat -,raw,echo=0 TCP:192.168.1.10:9999
//	socat pty,link=/tmp/ttyrkttl,raw TCP:192.168.1.10:9999   # 远端伪装成本地串口
package tcpbridge

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"rkttl/internal/hub"
	"rkttl/internal/version"
)

// Bridge 是串口 <-> 多 TCP 客户端的字节桥。
type Bridge struct {
	hub      *hub.SerialHub
	quiet    bool // 不发送横幅与状态行（纯透传，适配 socat/脚本）
	autoOpen bool // 有客户端接入且串口未打开时，按默认配置尝试打开
	log      *log.Logger

	mu        sync.Mutex
	ln        net.Listener
	conns     map[net.Conn]struct{}
	closed    bool
	done      chan struct{} // Start 成功后非 nil，桥结束时关闭
	acceptErr error
	lastWarn  time.Time // 写失败提示限速

	clientMu      sync.Mutex
	clientChanged func(n int, joined bool) // 客户端增减通知（可为 nil）
}

// New 创建桥。quiet 为 true 时不发送横幅/状态行。
func New(h *hub.SerialHub, quiet, autoOpen bool, logger *log.Logger) *Bridge {
	return &Bridge{
		hub:      h,
		quiet:    quiet,
		autoOpen: autoOpen,
		log:      logger,
		conns:    make(map[net.Conn]struct{}),
	}
}

// Start 在 addr 上同步建立监听（立即返回监听错误），接入循环在后台运行。
func (b *Bridge) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ln.Close()
		return fmt.Errorf("桥已关闭")
	}
	b.ln = ln
	done := make(chan struct{})
	b.done = done
	b.mu.Unlock()
	go func() {
		defer close(done)
		b.acceptLoop(ln)
	}()
	b.logf("TCP 桥已启动: tcp://%s (quiet=%v)", ln.Addr(), b.quiet)
	return nil
}

// Done 返回一个通道，桥结束时关闭（Start 之前为 nil）。
func (b *Bridge) Done() <-chan struct{} {
	b.mu.Lock()
	d := b.done
	b.mu.Unlock()
	return d
}

// ListenAndServe 在 addr 上监听并阻塞，直至 Close 或出错（CLI 用）。
func (b *Bridge) ListenAndServe(addr string) error {
	if err := b.Start(addr); err != nil {
		return err
	}
	<-b.Done()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acceptErr
}

func (b *Bridge) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			b.mu.Lock()
			stopped := b.closed
			if !stopped {
				b.acceptErr = err
			}
			b.mu.Unlock()
			if stopped {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		go b.handle(conn)
	}
}

// Close 停止监听并断开所有客户端。
func (b *Bridge) Close() {
	b.mu.Lock()
	b.closed = true
	ln := b.ln
	b.ln = nil
	conns := make([]net.Conn, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.conns = make(map[net.Conn]struct{})
	b.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
}

// ClientCount 返回当前连接的客户端数。
func (b *Bridge) ClientCount() int {
	b.mu.Lock()
	n := len(b.conns)
	b.mu.Unlock()
	return n
}

// SetClientNotifier 设置客户端增减通知回调（joined=true 表示接入）。
func (b *Bridge) SetClientNotifier(f func(n int, joined bool)) {
	b.clientMu.Lock()
	b.clientChanged = f
	b.clientMu.Unlock()
}

func (b *Bridge) notifyClient(n int, joined bool) {
	b.clientMu.Lock()
	f := b.clientChanged
	b.clientMu.Unlock()
	if f != nil {
		f(n, joined)
	}
}

func (b *Bridge) handle(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true) // 交互场景降低延迟
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		conn.Close()
		return
	}
	b.conns[conn] = struct{}{}
	n := len(b.conns)
	b.mu.Unlock()
	b.notifyClient(n, true)
	remove := func() {
		b.mu.Lock()
		delete(b.conns, conn)
		n := len(b.conns)
		b.mu.Unlock()
		conn.Close()
		b.notifyClient(n, false)
	}
	defer remove()

	st := b.hub.Status()
	b.sendLine(conn, fmt.Sprintf("rkttl v%s tcp bridge | serial: %s @ %d | open=%v | clients=%d",
		version.Version, serialDesc(st), st.DefaultBaud, st.Open, b.ClientCount()))

	// 串口未打开时尝试按默认配置打开（bridge 模式的常见诉求）
	if b.autoOpen && !b.hub.IsOpen() {
		if def := b.hub.Default(); def.Port != "" {
			if err := b.hub.Open(def); err != nil {
				b.sendLine(conn, "[rkttl] 自动打开串口失败: "+err.Error())
			}
		} else {
			b.sendLine(conn, "[rkttl] 串口未打开（未配置默认串口，请通过 HTTP API 打开）")
		}
	}

	// 客户端 -> 串口
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if _, werr := b.hub.Write(buf[:n]); werr != nil {
					b.warnRateLimited(conn, "[rkttl] 写串口失败: "+werr.Error())
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// 串口 -> 客户端
	ch := b.hub.Subscribe()
	defer b.hub.Unsubscribe(ch)
	for {
		select {
		case <-done:
			return
		case ev := <-ch:
			switch ev.Type {
			case "data":
				if _, err := conn.Write(ev.Data); err != nil {
					return
				}
			case "opened":
				if !b.quiet {
					b.sendLine(conn, "[rkttl] 串口已打开: "+ev.Info)
				}
			case "closed":
				if !b.quiet {
					b.sendLine(conn, "[rkttl] 串口已断开: "+ev.Info)
				}
			}
		}
	}
}

func (b *Bridge) sendLine(conn net.Conn, s string) {
	if b.quiet {
		return
	}
	_, _ = conn.Write([]byte(s + "\r\n"))
}

// warnRateLimited 限制重复告警的频率，避免刷屏。
func (b *Bridge) warnRateLimited(conn net.Conn, msg string) {
	if b.quiet {
		return
	}
	now := time.Now()
	b.mu.Lock()
	if now.Sub(b.lastWarn) < 5*time.Second {
		b.mu.Unlock()
		return
	}
	b.lastWarn = now
	b.mu.Unlock()
	b.sendLine(conn, msg)
}

func (b *Bridge) logf(format string, args ...any) {
	if b.log != nil {
		b.log.Printf(format, args...)
	}
}

func serialDesc(st hub.Status) string {
	if st.Open {
		return fmt.Sprintf("%s@%d", st.Port, st.Baud)
	}
	if st.DefaultPort != "" {
		return fmt.Sprintf("%s@%d(未打开)", st.DefaultPort, st.DefaultBaud)
	}
	return "(未配置)"
}
