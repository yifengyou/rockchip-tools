// Package hub 实现 rkttl 的"串口多端共享"核心。
//
// 一个串口由一个 SerialHub 统一管理：串口接收的数据会同时
//  1. 写入按全局序号(seq)索引的环形缓冲，供 HTTP API 非破坏性轮询；
//  2. 广播给所有订阅者（本地 GUI、网页控制台、TCP 桥、本地 monitor）；
//
// 任意订阅者的写入都会被送进串口 —— 达到类似 tmux 多端 attach 的效果：
// 本地与远端可以同时看到同一路输入输出，并且都能输入。
package hub

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"
)

// ErrNotOpen 表示串口当前未打开。
var ErrNotOpen = errors.New("串口未打开")

const (
	// readTimeout 是串口读超时：让接收循环周期性地有机会检查退出条件。
	readTimeout = 500 * time.Millisecond
	// readWaitCap 限制 API 读取的最长等待时间。
	readWaitCap = 60 * time.Second
)

// Event 是推送给订阅者的事件。
type Event struct {
	Type string // "data" | "opened" | "closed"
	Data []byte // 仅 data 事件携带串口原始数据
	Seq  uint64 // data 事件：该批数据末端的 seq
	Info string // opened/closed 事件的状态说明（如错误原因）
	At   time.Time
}

// SerialConfig 描述一次串口打开参数。
type SerialConfig struct {
	Port     string `json:"port"`
	Baud     int    `json:"baud"`
	DataBits int    `json:"data_bits"`
	StopBits int    `json:"stop_bits"`
	Parity   string `json:"parity"` // none|odd|even|mark|space
}

// DefaultConfig 返回适用于 Rockchip 调试串口（1500000 8N1）的默认配置。
func DefaultConfig(port string) SerialConfig {
	return SerialConfig{Port: port, Baud: 1500000, DataBits: 8, StopBits: 1, Parity: "none"}
}

// LogStatus 描述会话日志文件状态。
type LogStatus struct {
	Enabled bool   `json:"enabled"`        // 是否启用了文件记录
	Path    string `json:"path,omitempty"` // 当前（或最近一次）日志文件
	Written uint64 `json:"written"`        // 当前文件已写入字节数
}

// Status 是 hub 的对外状态快照。
type Status struct {
	Open        bool   `json:"open"`
	Port        string `json:"port,omitempty"`
	Baud        int    `json:"baud,omitempty"`
	DataBits    int    `json:"data_bits,omitempty"`
	StopBits    int    `json:"stop_bits,omitempty"`
	Parity      string `json:"parity,omitempty"`
	AutoReopen  bool   `json:"auto_reopen"`
	LastError   string `json:"last_error,omitempty"`
	OpenedAt    string `json:"opened_at,omitempty"`
	Seq         uint64 `json:"seq"`         // 累计接收字节数
	Buffered    int    `json:"buffered"`    // 缓冲区中未被覆盖的字节数
	Received    uint64 `json:"received"`    // 累计接收（与 Seq 一致）
	Sent        uint64 `json:"sent"`        // 累计发送
	Dropped     uint64 `json:"dropped"`     // 缓冲区覆盖丢弃的累计字节数
	SubDropped  uint64 `json:"sub_dropped"` // 推送队列溢出丢弃的事件批数
	Subscribers int    `json:"subscribers"` // 当前订阅者数量（GUI/网页/TCP桥/monitor）

	// 启动时配置的默认串口（提示远端"该连哪个口"）
	DefaultPort string `json:"default_port,omitempty"`
	DefaultBaud int    `json:"default_baud,omitempty"`

	// 会话日志
	Log LogStatus `json:"log"`
}

// SerialHub 管理单个串口连接的生命周期、缓冲与订阅广播。
// 它可被安全地并发使用。
type SerialHub struct {
	mu           sync.Mutex
	port         serial.Port
	cfg          SerialConfig
	defCfg       SerialConfig
	open         bool
	closing      bool // 主动关闭中（不触发自动重连）
	manualClosed bool // 手动关闭后抑制自动重连，直到下次显式打开
	stopped      bool // hub 已整体停止
	epoch        int64
	lastErr      string
	openedAt     time.Time

	writeMu sync.Mutex // 串行化并发写，避免字节交错

	ring *RingBuffer

	subsMu     sync.Mutex
	subs       map[chan Event]struct{}
	subDropped atomic.Uint64

	autoReopen bool

	received atomic.Uint64
	sent     atomic.Uint64

	// 会话日志（原始接收字节流落盘）
	logMu      sync.Mutex
	logDir     string // 日志目录（空 = 不记录）
	logFile    *os.File
	logPath    string
	logWritten uint64

	log     *log.Logger // 可为 nil
	verbose bool
}

// New 创建 SerialHub。ringSize 为接收环形缓冲大小；autoReopen 表示
// 串口异常断开（如 USB 重新插拔）后是否自动尝试重连。
func New(ringSize int, autoReopen bool, logger *log.Logger, verbose bool) *SerialHub {
	return &SerialHub{
		ring:       NewRingBuffer(ringSize),
		subs:       make(map[chan Event]struct{}),
		autoReopen: autoReopen,
		log:        logger,
		verbose:    verbose,
	}
}

// SetDefault 设置默认串口配置（GUI/serve 的启动参数）。
func (h *SerialHub) SetDefault(cfg SerialConfig) {
	cfg.Port = normalizePortName(cfg.Port)
	h.mu.Lock()
	h.defCfg = cfg
	h.mu.Unlock()
}

// SetAutoOpenEnabled 动态切换"异常断开自动重连"。
func (h *SerialHub) SetAutoReopen(enabled bool) {
	h.mu.Lock()
	h.autoReopen = enabled
	h.mu.Unlock()
}

// Default 返回默认串口配置。
func (h *SerialHub) Default() SerialConfig {
	h.mu.Lock()
	c := h.defCfg
	h.mu.Unlock()
	return c
}

// Open 打开串口；若已有连接则先关闭再按新配置打开（换口/改波特率）。
func (h *SerialHub) Open(cfg SerialConfig) error {
	cfg.Port = normalizePortName(cfg.Port)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return errors.New("服务已停止")
	}
	return h.openLocked(cfg)
}

func (h *SerialHub) openLocked(cfg SerialConfig) error {
	mode, err := cfg.mode()
	if err != nil {
		h.lastErr = err.Error()
		return err
	}
	h.stopCurrentLocked() // 主动关闭旧连接（静默）
	p, err := serial.Open(cfg.Port, mode)
	if err != nil {
		h.lastErr = err.Error()
		return fmt.Errorf("打开串口 %s 失败: %w", cfg.Port, err)
	}
	if err := p.SetReadTimeout(readTimeout); err != nil {
		p.Close()
		h.lastErr = err.Error()
		return fmt.Errorf("设置串口 %s 读超时失败: %w", cfg.Port, err)
	}
	h.port = p
	h.cfg = cfg
	h.open = true
	h.closing = false
	h.manualClosed = false
	h.lastErr = ""
	h.openedAt = time.Now()
	h.ring.Reset()
	h.epoch++
	// 会话日志：手动关闭后重新打开=新文件；异常断开自动重连=沿用同一文件
	h.logMu.Lock()
	h.openLogFileLocked()
	h.logMu.Unlock()
	epoch, port := h.epoch, p
	go h.rxLoop(port, epoch)
	h.logf("串口已打开: %s @ %d %d%s%d", cfg.Port, cfg.Baud, cfg.DataBits, parityShort(cfg.Parity), cfg.StopBits)
	h.broadcast(Event{Type: "opened", Info: fmt.Sprintf("%s @ %d", cfg.Port, cfg.Baud), At: time.Now()})
	return nil
}

// Close 手动关闭串口（不触发自动重连；会话日志文件随之结束）。
func (h *SerialHub) Close() error {
	h.mu.Lock()
	if h.port == nil {
		h.mu.Unlock()
		return nil
	}
	h.stopCurrentLocked()
	h.manualClosed = true
	h.mu.Unlock()
	h.logMu.Lock()
	h.closeLogFileLocked()
	h.logMu.Unlock()
	h.logf("串口已手动关闭")
	h.broadcast(Event{Type: "closed", Info: "已手动关闭", At: time.Now()})
	return nil
}

// Stop 停止 hub：关闭串口并抑制自动重连。用于进程退出。
func (h *SerialHub) Stop() {
	h.mu.Lock()
	h.stopped = true
	if h.port != nil {
		h.stopCurrentLocked()
	}
	h.mu.Unlock()
	h.logMu.Lock()
	h.closeLogFileLocked()
	h.logMu.Unlock()
	h.broadcast(Event{Type: "closed", Info: "服务已停止", At: time.Now()})
}

// stopCurrentLocked 关闭当前串口连接（要求已持有 h.mu）。
func (h *SerialHub) stopCurrentLocked() {
	if h.port != nil {
		h.closing = true
		p := h.port
		h.port = nil
		h.open = false
		h.epoch++ // 使旧接收循环的回调失效
		p.Close()
	}
	h.open = false
}

// IsOpen 返回串口当前是否打开。
func (h *SerialHub) IsOpen() bool {
	h.mu.Lock()
	o := h.open
	h.mu.Unlock()
	return o
}

// Config 返回当前（或最近一次）串口配置。
func (h *SerialHub) Config() SerialConfig {
	h.mu.Lock()
	c := h.cfg
	h.mu.Unlock()
	return c
}

// Write 向串口写入数据。
func (h *SerialHub) Write(b []byte) (int, error) {
	h.mu.Lock()
	p, open := h.port, h.open
	h.mu.Unlock()
	if !open || p == nil {
		return 0, ErrNotOpen
	}
	h.writeMu.Lock()
	n, err := p.Write(b)
	if err == nil {
		if derr := p.Drain(); derr != nil {
			h.vlogf("drain: %v", derr)
		}
	}
	h.writeMu.Unlock()
	h.sent.Add(uint64(n))
	return n, err
}

// Break 发送 break 信号，持续 d。
func (h *SerialHub) Break(d time.Duration) error {
	h.mu.Lock()
	p, open := h.port, h.open
	h.mu.Unlock()
	if !open || p == nil {
		return ErrNotOpen
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return p.Break(d)
}

// Read 按 seq 非破坏性地读取串口接收缓冲（见 RingBuffer.ReadSince）。
func (h *SerialHub) Read(since uint64, max int, wait time.Duration) ReadResult {
	if wait < 0 {
		wait = 0
	}
	if wait > readWaitCap {
		wait = readWaitCap
	}
	return h.ring.ReadSince(since, max, wait)
}

// FlushBuffer 丢弃缓冲区中已接收但未被覆盖的数据（seq 保持连续）。
func (h *SerialHub) FlushBuffer() {
	h.ring.Reset()
}

// Seq 返回最新 seq（累计接收字节数）。
func (h *SerialHub) Seq() uint64 { return h.ring.Total() }

// Status 返回状态快照。
func (h *SerialHub) Status() Status {
	h.mu.Lock()
	st := Status{
		AutoReopen:  h.autoReopen,
		DefaultPort: h.defCfg.Port,
		DefaultBaud: h.defCfg.Baud,
	}
	if h.open {
		st.Open = true
		st.Port = h.cfg.Port
		st.Baud = h.cfg.Baud
		st.DataBits = h.cfg.DataBits
		st.StopBits = h.cfg.StopBits
		st.Parity = h.cfg.Parity
		st.OpenedAt = h.openedAt.Format("2006-01-02 15:04:05")
	}
	if h.lastErr != "" {
		st.LastError = h.lastErr
	}
	h.mu.Unlock()

	st.Received = h.received.Load()
	st.Sent = h.sent.Load()
	st.Seq = h.ring.Total()
	st.Buffered = h.ring.Len()
	st.Dropped = h.ring.Dropped()
	st.SubDropped = h.subDropped.Load()
	h.subsMu.Lock()
	st.Subscribers = len(h.subs)
	h.subsMu.Unlock()
	st.Log = h.logStatus()
	return st
}

// Subscribe 返回一个订阅通道；不再使用时必须 Unsubscribe。
// 通道缓冲有限，消费过慢时事件会被丢弃（data 丢弃不影响环形缓冲中的数据）。
func (h *SerialHub) Subscribe() chan Event {
	ch := make(chan Event, 512)
	h.subsMu.Lock()
	h.subs[ch] = struct{}{}
	h.subsMu.Unlock()
	return ch
}

// Unsubscribe 注销订阅通道。
func (h *SerialHub) Unsubscribe(ch chan Event) {
	h.subsMu.Lock()
	delete(h.subs, ch)
	h.subsMu.Unlock()
}

func (h *SerialHub) broadcast(ev Event) {
	h.subsMu.Lock()
	defer h.subsMu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			h.subDropped.Add(1)
		}
	}
}

// rxLoop 是串口接收循环：写入环形缓冲、会话日志文件并广播给订阅者。
// 读超时返回 (0, nil)，继续循环即可。
func (h *SerialHub) rxLoop(p serial.Port, epoch int64) {
	buf := make([]byte, 4096)
	for {
		n, err := p.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			h.received.Add(uint64(n))
			seq := h.ring.Write(chunk)
			h.writeLog(chunk)
			h.broadcast(Event{Type: "data", Data: chunk, Seq: seq, At: time.Now()})
		}
		if err != nil {
			h.onRxFatal(p, epoch, err)
			return
		}
	}
}

// onRxFatal 处理接收循环的终止：区分"主动关闭"与"异常断开"，
// 后者在启用 autoReopen 时触发自动重连。
func (h *SerialHub) onRxFatal(p serial.Port, epoch int64, err error) {
	h.mu.Lock()
	if h.epoch != epoch || h.port != p {
		// 已被新的 Open/Close 接管，静默退出
		h.mu.Unlock()
		p.Close()
		return
	}
	portName := h.cfg.Port
	h.open = false
	h.port = nil
	h.lastErr = err.Error()
	intentional := h.closing
	reopen := h.autoReopen && !intentional && !h.stopped && !h.manualClosed && portName != ""
	h.mu.Unlock()
	p.Close()

	h.logf("串口 %s 断开: %v", portName, err)
	h.broadcast(Event{Type: "closed", Info: err.Error(), At: time.Now()})
	if reopen {
		go h.reopenLoop(portName)
	}
}

// reopenLoop 周期性尝试按原配置重新打开串口，直到成功或被接管。
func (h *SerialHub) reopenLoop(portName string) {
	h.logf("串口 %s 将自动重连...", portName)
	for {
		h.mu.Lock()
		if h.stopped || h.open || h.manualClosed || !h.autoReopen {
			h.mu.Unlock()
			return
		}
		cfg := h.cfg
		h.mu.Unlock()
		if err := h.Open(cfg); err == nil {
			return // 成功：openLocked 已广播 opened
		} else {
			h.vlogf("自动重连失败: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
}

func (h *SerialHub) logf(format string, args ...any) {
	if h.log != nil {
		h.log.Printf(format, args...)
	}
}

func (h *SerialHub) vlogf(format string, args ...any) {
	if h.verbose && h.log != nil {
		h.log.Printf("[debug] "+format, args...)
	}
}

// ---- 会话日志（文件记录） ----

// EnableFileLogging 启用会话日志：串口打开时在 dir 下创建
// rkttl-serial-<时间戳>.log，持续记录串口接收的原始字节流（含 ANSI）。
// 手动关闭串口后本文件结束，再次打开时创建新文件；
// 异常断开自动重连沿用同一文件。
func (h *SerialHub) EnableFileLogging(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	h.logMu.Lock()
	h.logDir = dir
	h.logMu.Unlock()
	return nil
}

// DisableFileLogging 停止文件记录并结束当前日志文件。
func (h *SerialHub) DisableFileLogging() {
	h.logMu.Lock()
	h.logDir = ""
	h.closeLogFileLocked()
	h.logMu.Unlock()
}

// openLogFileLocked 创建新的会话日志文件（要求已持有 logMu；
// 已有打开的文件或未启用时为 no-op）。秒级时间戳撞名时追加序号。
func (h *SerialHub) openLogFileLocked() {
	if h.logDir == "" || h.logFile != nil {
		return
	}
	base := filepath.Join(h.logDir, "rkttl-serial-"+time.Now().Format("20060102-150405"))
	name := base + ".log"
	for i := 1; i < 1000; i++ {
		if _, err := os.Stat(name); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s-%03d.log", base, i)
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		h.logf("创建日志文件失败: %v", err)
		return
	}
	h.logFile, h.logPath, h.logWritten = f, name, 0
	h.logf("会话日志: %s", name)
}

// closeLogFileLocked 结束当前日志文件（要求已持有 logMu）。
// logPath/logWritten 保留用于状态显示与导出。
func (h *SerialHub) closeLogFileLocked() {
	if h.logFile != nil {
		h.logFile.Close()
		h.logFile = nil
	}
}

// writeLog 把一批接收数据追加到会话日志文件。
func (h *SerialHub) writeLog(p []byte) {
	if len(p) == 0 {
		return
	}
	h.logMu.Lock()
	if h.logFile != nil {
		if _, err := h.logFile.Write(p); err != nil {
			h.logf("写日志文件失败: %v", err)
		} else {
			h.logWritten += uint64(len(p))
		}
	}
	h.logMu.Unlock()
}

// logStatus 返回日志状态快照。
func (h *SerialHub) logStatus() LogStatus {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return LogStatus{Enabled: h.logDir != "", Path: h.logPath, Written: h.logWritten}
}

// ReadLog 返回会话日志内容：优先读取日志文件（含自打开串口以来的全部
// 数据，不受环形缓冲 1MB 上限约束）；未启用文件记录时返回当前缓冲内容。
func (h *SerialHub) ReadLog() ([]byte, error) {
	h.logMu.Lock()
	path := h.logPath
	h.logMu.Unlock()
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return b, nil
		}
		// 文件读取失败（被删除等）时退回缓冲
	}
	return h.ring.ReadSince(0, 0, 0).Data, nil
}

// TruncateLog 清空当前日志文件（句柄保持，后续数据继续写入同一文件）。
func (h *SerialHub) TruncateLog() error {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if h.logFile == nil {
		return errors.New("没有正在写入的日志文件（串口未打开或未启用文件记录）")
	}
	if err := h.logFile.Truncate(0); err != nil {
		return err
	}
	h.logWritten = 0
	return nil
}

// LogFileName 返回用于导出/下载的日志文件名。
func (h *SerialHub) LogFileName() string {
	h.logMu.Lock()
	p := h.logPath
	h.logMu.Unlock()
	if p != "" {
		return filepath.Base(p)
	}
	return "rkttl-serial-" + time.Now().Format("20060102-150405.000") + ".log"
}

// ---- 配置解析辅助 ----

// mode 把配置映射为 go.bug.st/serial 的打开参数。
func (c SerialConfig) mode() (*serial.Mode, error) {
	if strings.TrimSpace(c.Port) == "" {
		return nil, errors.New("串口名为空")
	}
	if c.Baud <= 0 {
		return nil, fmt.Errorf("无效波特率 %d", c.Baud)
	}
	if c.DataBits < 5 || c.DataBits > 8 {
		return nil, fmt.Errorf("无效数据位 %d（支持 5-8）", c.DataBits)
	}
	parity, err := parseParity(c.Parity)
	if err != nil {
		return nil, err
	}
	stopBits, err := parseStopBits(c.StopBits)
	if err != nil {
		return nil, err
	}
	return &serial.Mode{
		BaudRate: c.Baud,
		DataBits: c.DataBits,
		Parity:   parity,
		StopBits: stopBits,
	}, nil
}

func parseParity(s string) (serial.Parity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "n":
		return serial.NoParity, nil
	case "odd", "o":
		return serial.OddParity, nil
	case "even", "e":
		return serial.EvenParity, nil
	case "mark":
		return serial.MarkParity, nil
	case "space":
		return serial.SpaceParity, nil
	}
	return 0, fmt.Errorf("无效校验位 %q（支持 none/odd/even/mark/space）", s)
}

func parseStopBits(n int) (serial.StopBits, error) {
	switch n {
	case 0, 1:
		return serial.OneStopBit, nil
	case 2:
		return serial.TwoStopBits, nil
	}
	return 0, fmt.Errorf("无效停止位 %d（支持 1 或 2）", n)
}

func parityShort(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "odd", "o":
		return "O"
	case "even", "e":
		return "E"
	case "mark":
		return "M"
	case "space":
		return "S"
	}
	return "N"
}

// normalizePortName 规范化串口名：去掉空白、`\\.\` 前缀（库会自动补回）与结尾冒号。
func normalizePortName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, `\\.\`)
	name = strings.TrimSuffix(name, ":")
	return name
}
