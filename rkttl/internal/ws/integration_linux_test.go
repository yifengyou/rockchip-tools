//go:build linux

// 集成测试：PTY 模拟串口，验证 WebSocket + HTTP API 的端到端正确性。
package ws_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"

	"rkttl/internal/hub"
	"rkttl/internal/ws"
)

// ptyPair 打开一对 PTY，返回 master 文件与 slave 路径。
// 注意：不预先打开 slave，让 hub 独占打开（避免多个 fd 竞争信号量）。
// 但 hub 打开 slave 时会按默认 termios（带 echo），这会影响测试。
// 解决方案：在 hub 打开后，通过 master 的 termios 反向设置 slave 的 termios。
func ptyPair(t *testing.T) (*os.File, string, func()) {
	t.Helper()

	masterFd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	unlock := 0
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(masterFd),
		uintptr(unix.TIOCSPTLCK), uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		t.Fatalf("unlockpt: %v", errno)
	}
	var ptn int
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(masterFd),
		uintptr(unix.TIOCGPTN), uintptr(unsafe.Pointer(&ptn))); errno != 0 {
		t.Fatalf("ioctl TIOCGPTN: %v", errno)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", ptn)

	master := os.NewFile(uintptr(masterFd), "pty-master")

	cleanup := func() {
		_ = master.Close()
	}
	return master, slavePath, cleanup
}

// makeRaw 把 fd 设置为 raw 模式（等价 cfmakeraw）。
func makeRaw(fd int) error {
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	tio.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	tio.Oflag &^= unix.OPOST
	tio.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	tio.Cflag &^= unix.CSIZE | unix.PARENB
	tio.Cflag |= unix.CS8
	tio.Cc[unix.VMIN] = 1
	tio.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, tio)
}

// setRawViaMaster 通过 master fd 把对应的 slave 设置为 raw 模式。
// master 与 slave 共享 termios，因此修改 master 的 termios 即修改 slave。
func setRawViaMaster(masterFd uintptr) error {
	tio, err := unix.IoctlGetTermios(int(masterFd), unix.TCGETS)
	if err != nil {
		return err
	}
	tio.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	tio.Oflag &^= unix.OPOST
	tio.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	tio.Cflag &^= unix.CSIZE | unix.PARENB
	tio.Cflag |= unix.CS8
	tio.Cc[unix.VMIN] = 1
	tio.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(int(masterFd), unix.TCSETS, tio)
}

func TestWebSocketEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: PTY 时序敏感")
	}
	master, portName, cleanup := ptyPair(t)
	defer cleanup()

	h := hub.New(1<<20, false, nil, false)
	cfg := hub.DefaultConfig(portName)
	h.SetDefault(cfg)
	if err := h.Open(cfg); err != nil {
		t.Fatalf("打开串口: %v", err)
	}
	defer h.Stop()

	// hub 打开 slave 后，slave 的 termios 由串口库配置（带 echo）。
	// 通过 master 的 termios 反向设置 slave 为 raw 模式。
	if err := setRawViaMaster(master.Fd()); err != nil {
		t.Logf("设置 raw 失败（继续）: %v", err)
	}

	token := "test-token-abc"
	srv := ws.New(h, token, cfg, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?token=" + token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	// 设备 -> 客户端：写 master，客户端应收到二进制帧
	// 用重试应对 PTY 信号量/调度延迟
	// 关键：在写 master 前确保 slave 已被 hub 打开并处于 raw 模式。
	// hub.Open 是同步的，返回时 slave fd 已建立。但 SetReadDeadline 在 Dial 后
	// 设置，可能错过早期的回放数据。这里在写 master 前让 hub 先读到空数据（确认 fd 就绪）。
	time.Sleep(50 * time.Millisecond) // 让 hub 的 rxLoop 开始运行
	var got []byte
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := master.Write([]byte("hello-from-device\r\n")); err != nil {
			t.Fatalf("写 PTY: %v", err)
		}
		for i := 0; i < 20; i++ {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if mt == websocket.BinaryMessage {
				got = append(got, data...)
			}
			if strings.Contains(string(got), "hello-from-device") {
				break
			}
		}
		if strings.Contains(string(got), "hello-from-device") {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		got = nil
	}
	if !strings.Contains(string(got), "hello-from-device") {
		t.Fatalf("客户端未收到设备数据: got=%q", got)
	}

	// 客户端 -> 设备：发文本 input 命令，master 端应读到（因为 slave 是 echo 模式，
	// 客户端写入 slave 后，master 会收到 echo；但 raw 模式下 master 直接读到原始字节）
	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"input","data":"ls\r"}`)); err != nil {
		t.Fatalf("写 WebSocket: %v", err)
	}
	_ = master.SetReadDeadline(time.Now().Add(5 * time.Second))
	mbuf := make([]byte, 256)
	n, err := master.Read(mbuf)
	if err != nil {
		t.Fatalf("读 master: %v", err)
	}
	if !strings.Contains(string(mbuf[:n]), "ls") {
		t.Errorf("master 未收到 ls: %q", mbuf[:n])
	}
}

func TestHTTPAPICompatibility(t *testing.T) {
	master, portName, cleanup := ptyPair(t)
	defer cleanup()

	h := hub.New(1<<20, false, nil, false)
	cfg := hub.DefaultConfig(portName)
	h.SetDefault(cfg)
	if err := h.Open(cfg); err != nil {
		t.Fatalf("打开串口: %v", err)
	}
	defer h.Stop()

	token := "http-token"
	srv := ws.New(h, token, cfg, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	base := ts.URL + "/api"

	// /api/status
	resp, err := http.Get(base + "/status?token=" + token)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var statusResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Serial hub.Status `json:"serial"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		t.Fatalf("解析 status: %v", err)
	}
	resp.Body.Close()
	if !statusResp.OK || !statusResp.Data.Serial.Open {
		t.Fatalf("status 异常: %+v", statusResp)
	}

	// /api/write
	req, _ := http.NewRequest("POST", base+"/write?token="+token,
		strings.NewReader(`{"data":"version","eol":"crlf"}`))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("write 状态码 %d", resp2.StatusCode)
	}

	// 设备端（master）应读到 version\r\n
	_ = master.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	n, err := master.Read(buf)
	if err != nil {
		t.Fatalf("读 master: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "version") {
		t.Errorf("master 未收到 version: %q", buf[:n])
	}

	// 设备输出 -> /api/read 应可读
	if _, err := master.Write([]byte("device-output\r\n")); err != nil {
		t.Fatalf("写 master: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var readResp struct {
		OK   bool `json:"ok"`
		Data struct {
			Seq  uint64 `json:"seq"`
			N    int    `json:"n"`
			Text string `json:"text"`
		} `json:"data"`
	}
	for time.Now().Before(deadline) {
		resp3, err := http.Get(base + "/read?since=0&token=" + token)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		err = json.NewDecoder(resp3.Body).Decode(&readResp)
		resp3.Body.Close()
		if err != nil {
			t.Fatalf("解析 read: %v", err)
		}
		if strings.Contains(readResp.Data.Text, "device-output") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(readResp.Data.Text, "device-output") {
		t.Fatalf("read 未含 device-output: %q", readResp.Data.Text)
	}
}

func TestAuthRejection(t *testing.T) {
	h := hub.New(1<<20, false, nil, false)
	srv := ws.New(h, "secret-token", hub.SerialConfig{}, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无 token 期望 401，得到 %d", resp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/api/status?token=wrong")
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("错误 token 期望 401，得到 %d", resp2.StatusCode)
	}

	resp3, err := http.Get(ts.URL + "/api/status?token=secret-token")
	if err != nil {
		t.Fatalf("请求: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("正确 token 期望 200，得到 %d", resp3.StatusCode)
	}
}

// TestWebSocketMultiClient 验证两个客户端同时收到同一路输出（tmux 式共享）。
func TestWebSocketMultiClient(t *testing.T) {
	master, portName, cleanup := ptyPair(t)
	defer cleanup()

	h := hub.New(1<<20, false, nil, false)
	cfg := hub.DefaultConfig(portName)
	h.SetDefault(cfg)
	if err := h.Open(cfg); err != nil {
		t.Fatalf("打开串口: %v", err)
	}
	defer h.Stop()

	srv := ws.New(h, "", cfg, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	connA, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("客户端 A 连接: %v", err)
	}
	defer connA.Close()
	connB, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("客户端 B 连接: %v", err)
	}
	defer connB.Close()

	time.Sleep(100 * time.Millisecond)
	if _, err := master.Write([]byte("shared-line\r\n")); err != nil {
		t.Fatalf("写 PTY: %v", err)
	}

	readUntil := func(c *websocket.Conn, want string) bool {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var acc []byte
		for i := 0; i < 10; i++ {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return false
			}
			if mt == websocket.BinaryMessage {
				acc = append(acc, data...)
			}
			if strings.Contains(string(acc), want) {
				return true
			}
		}
		return false
	}
	if !readUntil(connA, "shared-line") {
		t.Error("客户端 A 未收到共享输出")
	}
	if !readUntil(connB, "shared-line") {
		t.Error("客户端 B 未收到共享输出")
	}
}
