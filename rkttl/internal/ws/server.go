// Package ws 的 HTTP/WebSocket 服务实现。
package ws

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"rkttl/internal/hub"
	"rkttl/internal/version"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = 25 * time.Second // 必须小于 pongWait
	maxMessageSize = 1 << 20          // 1MiB 单帧上限
	rtxQueueSize   = 256              // 每个客户端的待发队列
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 8192,
	// 本工具面向内网/VPN 场景，鉴权由 token 完成；跨域检查放行。
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server 是 rkttl 的 HTTP + WebSocket 服务。
type Server struct {
	hub    *hub.SerialHub
	defCfg hub.SerialConfig

	tokenMu sync.RWMutex
	token   string

	mu       sync.Mutex
	srv      *http.Server
	ln       net.Listener
	done     chan struct{}
	serveErr error

	opMu       sync.Mutex
	opNotifier func(entry string)

	log *log.Logger
}

// New 创建服务。token 为空表示关闭鉴权（仅建议隔离网络使用）。
func New(h *hub.SerialHub, token string, defCfg hub.SerialConfig, logger *log.Logger) *Server {
	return &Server{hub: h, token: token, defCfg: defCfg, log: logger}
}

// SetToken 运行期更换访问令牌。
func (s *Server) SetToken(token string) {
	s.tokenMu.Lock()
	s.token = token
	s.tokenMu.Unlock()
}

// Token 返回当前访问令牌。
func (s *Server) Token() string {
	s.tokenMu.RLock()
	t := s.token
	s.tokenMu.RUnlock()
	return t
}

// SetOpNotifier 设置远端关键操作回调（用于 GUI 操作日志）。
func (s *Server) SetOpNotifier(f func(entry string)) {
	s.opMu.Lock()
	s.opNotifier = f
	s.opMu.Unlock()
}

func (s *Server) notifyOp(format string, args ...any) {
	s.opMu.Lock()
	f := s.opNotifier
	s.opMu.Unlock()
	if f != nil {
		f(fmt.Sprintf(format, args...))
	}
}

// Start 在 addr 上同步监听（立即返回监听错误），HTTP 服务在后台运行。
func (s *Server) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.srv != nil {
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("服务已启动")
	}
	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.srv = srv
	s.ln = ln
	done := make(chan struct{})
	s.done = done
	s.mu.Unlock()

	go func() {
		err := srv.Serve(ln)
		s.mu.Lock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr = err
			s.logf("HTTP 服务异常退出: %v", err)
		}
		s.mu.Unlock()
		close(done)
	}()
	s.logf("HTTP+WebSocket 服务已启动: http://%s", ln.Addr())
	return nil
}

// Done 返回服务结束信号（Start 之前为 nil）。
func (s *Server) Done() <-chan struct{} {
	s.mu.Lock()
	d := s.done
	s.mu.Unlock()
	return d
}

// Err 返回服务异常退出原因。
func (s *Server) Err() error {
	s.mu.Lock()
	e := s.serveErr
	s.mu.Unlock()
	return e
}

// ListenAndServe 启动并阻塞，直至 Close 或出错（CLI 用）。
func (s *Server) ListenAndServe(addr string) error {
	if err := s.Start(addr); err != nil {
		return err
	}
	<-s.Done()
	return s.Err()
}

// ServeHTTP 让 Server 满足 http.Handler（便于测试包装或复用）。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler().ServeHTTP(w, r)
}

// Close 停止服务。可重复调用；停止后可再次 Start（GUI 切换监听地址）。
func (s *Server) Close() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	s.mu.Lock()
	// 清空句柄，允许后续 Start 重新监听（GUI 切换地址/重启动）
	s.srv = nil
	s.ln = nil
	s.done = nil
	s.mu.Unlock()
}

// ListenAddr 返回实际监听地址（未启动时为空）。
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/ws", s.handleWS)

	// 静态资源（xterm.js 等）：带 ETag/长缓存
	mux.HandleFunc("/static/", s.handleStatic)

	// HTTP JSON API（保留原接口，便于脚本/AI 使用）
	mux.HandleFunc("/api/ports", s.handlePorts)
	mux.HandleFunc("/api/netifs", s.handleNetIfs)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/open", s.handleOpen)
	mux.HandleFunc("/api/close", s.handleClose)
	mux.HandleFunc("/api/write", s.handleWrite)
	mux.HandleFunc("/api/read", s.handleRead)
	mux.HandleFunc("/api/flush", s.handleFlush)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/break", s.handleBreak)
	return s.auth(mux)
}

// auth 校验访问令牌：Authorization: Bearer、X-Auth-Token、?token=。
// 静态资源不做鉴权（非敏感，且需要被浏览器直接加载）。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		s.tokenMu.RLock()
		token := s.token
		s.tokenMu.RUnlock()
		if token != "" && !tokenMatches(r, token) {
			// 网页本身需要能加载（否则无法输入令牌）；页面内的 API/WS
			// 调用会带上令牌。因此对 GET / 放行，仅对数据接口强制鉴权。
			if r.URL.Path == "/" && r.Method == http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}
			s.jsonError(w, http.StatusUnauthorized, "无效或缺失的访问令牌")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tokenMatches(r *http.Request, token string) bool {
	if v := r.URL.Query().Get("token"); v == token {
		return true
	}
	if v := r.Header.Get("X-Auth-Token"); v == token {
		return true
	}
	h := r.Header.Get("Authorization")
	if h == token || h == "Bearer "+token || h == "token "+token {
		return true
	}
	// WebSocket 浏览器客户端无法自定义头，允许从 Sec-WebSocket-Protocol 携带
	for _, p := range websocket.Subprotocols(r) {
		if p == "rkttl.token."+token {
			return true
		}
	}
	return false
}

// ---- 页面与静态资源 ----

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	data, ctype, ok := staticAsset(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

// ---- WebSocket ----

// wsClient 表示一个网页客户端连接。
type wsClient struct {
	conn    *websocket.Conn
	send    chan wsFrame
	hub     *hub.SerialHub
	srv     *Server
	closed  chan struct{}
	once    sync.Once
	lastSeq uint64 // 最近一次已发送数据的 seq（用于缺口补齐）
}

// wsFrame 是待发送的一帧。
type wsFrame struct {
	kind int // websocket.BinaryMessage | websocket.TextMessage
	data []byte
}

func (c *wsClient) close() {
	c.once.Do(func() { close(c.closed) })
}

// handleWS 处理 WebSocket 连接：全双工收发。
//
// 客户端消息协议：
//   - 二进制帧：原样写入串口
//   - 文本帧：JSON {type:"input",data:"..."} 或 {type:"ping"}
//
// 服务器消息协议：
//   - 二进制帧：串口原始字节（顺序严格，seq 由服务器维护）
//   - 文本帧：JSON 状态/错误/序号通知
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// 令牌可通过 Sec-WebSocket-Protocol 传递，回显一个子协议以满足握手
	respHeader := http.Header{}
	proto := ""
	if s.Token() != "" {
		for _, p := range websocket.Subprotocols(r) {
			if p == "rkttl.v1" {
				proto = p
			}
		}
	}
	if proto != "" {
		respHeader.Set("Sec-WebSocket-Protocol", proto)
	}

	conn, err := upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		s.logf("WebSocket 升级失败: %v", err)
		return
	}

	// 解析可选的 since 游标：客户端可指定从哪个 seq 开始补历史
	since, haveSince := uint64(0), false
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			since, haveSince = n, true
		}
	}
	histLimit := -1
	if v := r.URL.Query().Get("history"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			histLimit = n
		}
	}

	c := &wsClient{
		conn:   conn,
		send:   make(chan wsFrame, rtxQueueSize),
		hub:    s.hub,
		srv:    s,
		closed: make(chan struct{}),
	}

	// 订阅必须在回放之前，避免"回放与实时之间"的窗口丢数据。
	sub := s.hub.Subscribe()
	defer s.hub.Unsubscribe(sub)

	go c.writePump()

	// 1) 先回放历史（带 seq 对齐），再进入实时转发
	s.replay(c, since, haveSince, histLimit)

	// 2) 实时转发
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		s.forward(c, sub)
	}()

	// 3) 读循环（直到对端关闭）
	s.readLoop(c)
	c.close()
	_ = conn.Close()
	<-pumpDone
}

// replay 回放环形缓冲中的历史数据。
func (s *Server) replay(c *wsClient, since uint64, haveSince bool, histLimit int) {
	if histLimit == 0 {
		// 客户端不需要历史
		c.pushStatus()
		return
	}
	var res hub.ReadResult
	switch {
	case haveSince:
		res = s.hub.Read(since, 0, 0)
	case histLimit > 0:
		cur := s.hub.Seq()
		lo := uint64(histLimit)
		if cur > lo {
			lo = cur - lo
		} else {
			lo = 0
		}
		res = s.hub.Read(lo, 0, 0)
	default:
		res = s.hub.Read(0, 0, 0)
	}
	if len(res.Data) > 0 {
		c.enqueueData(res.Data)
	}
	c.pushMeta(res.Seq, res.Truncated)
	c.pushStatus()
}

// forward 把订阅事件转发给客户端；seq 跳跃时从缓冲补齐（防丢）。
func (s *Server) forward(c *wsClient, sub chan hub.Event) {
	lastSeq := c.lastSeq
	for {
		select {
		case <-c.closed:
			return
		case ev, ok := <-sub:
			if !ok {
				return
			}
			switch ev.Type {
			case "opened":
				// 串口重新打开：seq 从 0 重新计（hub.Open 会 ring.Reset()），
				// 必须重置 lastSeq，否则新连接的早期数据（seq < 旧 lastSeq）
				// 会被全部丢弃 —— 这正是 reset 后 Web 端看不到输出的根因。
				lastSeq = 0
				c.lastSeq = 0
				c.pushStatusWithInfo(ev.Type, ev.Info)
			case "closed":
				c.pushStatusWithInfo(ev.Type, ev.Info)
			case "data":
				// 若事件 seq 与期望不符（订阅队列曾溢出），先补缓冲
				if ev.Seq > lastSeq+uint64(len(ev.Data)) {
					if gap := s.hub.Read(lastSeq, 0, 0); len(gap.Data) > 0 && gap.Seq > lastSeq {
						c.enqueueData(gap.Data)
						lastSeq = gap.Seq
					}
				}
				if ev.Seq <= lastSeq {
					continue
				}
				c.enqueueData(ev.Data)
				lastSeq = ev.Seq
				c.lastSeq = lastSeq
			}
		}
	}
}

func (c *wsClient) enqueueData(b []byte) {
	if len(b) == 0 {
		return
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case c.send <- wsFrame{kind: websocket.BinaryMessage, data: cp}:
		c.srv.logf("enqueueData: 已投递 %d 字节到 send 通道", len(b))
	case <-c.closed:
		c.srv.logf("enqueueData: 连接已关闭，丢弃 %d 字节", len(b))
	default:
		// 待发队列满：丢弃并标记，让客户端通过 seq 补齐。
		// 这里不阻塞串口读循环。
		c.srv.logf("WebSocket 客户端发送队列满，丢弃 %d 字节", len(b))
	}
}

func (c *wsClient) enqueueJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	select {
	case c.send <- wsFrame{kind: websocket.TextMessage, data: b}:
	case <-c.closed:
	default:
	}
}

func (c *wsClient) pushStatus() {
	st := c.hub.Status()
	c.lastSeq = st.Seq
	c.enqueueJSON(map[string]any{
		"type":        "status",
		"open":        st.Open,
		"port":        st.Port,
		"baud":        st.Baud,
		"seq":         st.Seq,
		"rx":          st.Received,
		"tx":          st.Sent,
		"subscribers": st.Subscribers,
		"auto_reopen": st.AutoReopen,
		"last_error":  st.LastError,
	})
}

func (c *wsClient) pushStatusWithInfo(evType, info string) {
	st := c.hub.Status()
	c.enqueueJSON(map[string]any{
		"type":        "status",
		"event":       evType,
		"info":        info,
		"open":        st.Open,
		"port":        st.Port,
		"baud":        st.Baud,
		"seq":         st.Seq,
		"rx":          st.Received,
		"tx":          st.Sent,
		"subscribers": st.Subscribers,
	})
}

func (c *wsClient) pushMeta(seq uint64, truncated bool) {
	c.enqueueJSON(map[string]any{
		"type":      "meta",
		"seq":       seq,
		"truncated": truncated,
	})
}

// writePump 是唯一的写 goroutine（gorilla/websocket 不允许并发写）。
func (c *wsClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	defer c.close()

	for {
		select {
		case <-c.closed:
			return
		case f, ok := <-c.send:
			if !ok {
				return
			}
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(f.kind, f.data); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readLoop 读取客户端消息并写入串口。
func (s *Server) readLoop(c *wsClient) {
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))

		switch mt {
		case websocket.BinaryMessage:
			// 原始字节：直接写串口
			if _, werr := c.hub.Write(data); werr != nil {
				c.enqueueJSON(map[string]any{"type": "error", "message": "写串口失败: " + werr.Error()})
			}
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
				Data string `json:"data"`
				Hex  bool   `json:"hex"`
				EOL  string `json:"eol"`
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				c.enqueueJSON(map[string]any{"type": "error", "message": "消息格式错误"})
				continue
			}
			switch msg.Type {
			case "ping":
				c.enqueueJSON(map[string]any{"type": "pong", "t": time.Now().UnixMilli()})
			case "input", "":
				payload, perr := buildPayload(msg.Data, msg.Hex, msg.EOL)
				if perr != nil {
					c.enqueueJSON(map[string]any{"type": "error", "message": perr.Error()})
					continue
				}
				if _, werr := c.hub.Write(payload); werr != nil {
					c.enqueueJSON(map[string]any{"type": "error", "message": "写串口失败: " + werr.Error()})
				}
			default:
				c.enqueueJSON(map[string]any{"type": "error", "message": "未知消息类型: " + msg.Type})
			}
		}
	}
}

// ---- HTTP API ----

func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	ports, err := ListPorts()
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.jsonOK(w, map[string]any{"ports": ports, "count": len(ports)})
}

func (s *Server) handleNetIfs(w http.ResponseWriter, r *http.Request) {
	s.jsonOK(w, map[string]any{
		"netifs":       ListNetIfs(),
		"console_urls": ConsoleURLs(s.ListenAddr(), s.Token()),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.jsonOK(w, map[string]any{
		"version": version.Version,
		"auth":    s.Token() != "",
		"serial":  s.hub.Status(),
	})
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 POST")
		return
	}
	var req struct {
		Port     string `json:"port"`
		Baud     int    `json:"baud"`
		DataBits int    `json:"data_bits"`
		StopBits int    `json:"stop_bits"`
		Parity   string `json:"parity"`
	}
	if err := readJSONBody(w, r, &req); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := s.defCfg
	if strings.TrimSpace(req.Port) != "" {
		cfg.Port = req.Port
	}
	if req.Baud != 0 {
		cfg.Baud = req.Baud
	}
	if req.DataBits != 0 {
		cfg.DataBits = req.DataBits
	}
	if req.StopBits != 0 {
		cfg.StopBits = req.StopBits
	}
	if strings.TrimSpace(req.Parity) != "" {
		cfg.Parity = req.Parity
	}
	if err := s.hub.Open(cfg); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.notifyOp("远端(%s) 打开串口 %s @ %d", r.RemoteAddr, cfg.Port, cfg.Baud)
	s.jsonOK(w, map[string]any{"serial": s.hub.Status()})
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 POST")
		return
	}
	if err := s.hub.Close(); err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyOp("远端(%s) 关闭串口", r.RemoteAddr)
	s.jsonOK(w, map[string]any{"serial": s.hub.Status()})
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 POST")
		return
	}
	var req struct {
		Data string `json:"data"`
		Hex  bool   `json:"hex"`
		EOL  string `json:"eol"`
		CRLF bool   `json:"crlf"`
	}
	if err := readJSONBody(w, r, &req); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.EOL == "" && req.CRLF {
		req.EOL = "crlf"
	}
	b, err := buildPayload(req.Data, req.Hex, req.EOL)
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	n, werr := s.hub.Write(b)
	if werr != nil {
		if errors.Is(werr, hub.ErrNotOpen) {
			s.jsonError(w, http.StatusConflict, werr.Error())
		} else {
			s.jsonError(w, http.StatusInternalServerError, werr.Error())
		}
		return
	}
	s.jsonOK(w, map[string]any{"written": n})
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since uint64
	haveSince := false
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "since 需为非负整数")
			return
		}
		since, haveSince = n, true
	}
	max := 0
	if v := q.Get("max"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.jsonError(w, http.StatusBadRequest, "max 需为非负整数")
			return
		}
		max = n
	}
	var wait time.Duration
	if v := q.Get("wait_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "wait_ms 需为整数")
			return
		}
		if n < 0 {
			wait = 60 * time.Second
		} else {
			wait = time.Duration(n) * time.Millisecond
		}
	}

	if !haveSince {
		tail := 4096
		if max > 0 && max < tail {
			tail = max
		}
		cur := s.hub.Seq()
		lo := cur
		if cur > uint64(tail) {
			lo = cur - uint64(tail)
		} else {
			lo = 0
		}
		s.writeReadResp(w, s.hub.Read(lo, tail, 0))
		return
	}
	s.writeReadResp(w, s.hub.Read(since, max, wait))
}

func (s *Server) writeReadResp(w http.ResponseWriter, res hub.ReadResult) {
	resp := map[string]any{
		"seq":       res.Seq,
		"truncated": res.Truncated,
		"n":         len(res.Data),
		"buffered":  res.Buffered,
	}
	if len(res.Data) > 0 {
		resp["b64"] = base64.StdEncoding.EncodeToString(res.Data)
		resp["text"] = strings.ToValidUTF8(string(res.Data), "\uFFFD")
		resp["hex"] = hex.EncodeToString(res.Data)
	}
	s.jsonOK(w, resp)
}

func (s *Server) handleFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 POST")
		return
	}
	var req struct {
		File bool `json:"file"`
	}
	if r.ContentLength > 0 {
		body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
		if len(body) > 0 {
			_ = json.Unmarshal(body, &req)
		}
	}
	s.hub.FlushBuffer()
	resp := map[string]any{"seq": s.hub.Seq()}
	if req.File {
		if err := s.hub.TruncateLog(); err != nil {
			resp["file_error"] = err.Error()
		} else {
			resp["file_truncated"] = true
		}
	}
	s.jsonOK(w, resp)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	data, err := s.hub.ReadLog()
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+s.hub.LogFileName()+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	s.notifyOp("远端(%s) 导出会话日志（%d 字节）", r.RemoteAddr, len(data))
	_, _ = w.Write(data)
}

func (s *Server) handleBreak(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 POST")
		return
	}
	var req struct {
		MS int `json:"ms"`
	}
	_ = readJSONBody(w, r, &req) // 空 body 用默认值
	if req.MS <= 0 {
		req.MS = 100
	}
	if req.MS > 5000 {
		req.MS = 5000
	}
	if err := s.hub.Break(time.Duration(req.MS) * time.Millisecond); err != nil {
		if errors.Is(err, hub.ErrNotOpen) {
			s.jsonError(w, http.StatusConflict, err.Error())
		} else {
			s.jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.jsonOK(w, map[string]any{"break_ms": req.MS})
}

// ---- 辅助 ----

func readJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("读取请求体失败: %w", err)
	}
	if len(body) == 0 {
		return nil // 允许空 body（全部使用默认值）
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	return nil
}

func buildPayload(data string, isHex bool, eol string) ([]byte, error) {
	var b []byte
	if isHex {
		decoded, err := decodeHex(data)
		if err != nil {
			return nil, err
		}
		b = decoded
	} else {
		b = []byte(data)
	}
	switch strings.ToLower(strings.TrimSpace(eol)) {
	case "", "none":
	case "crlf":
		b = append(b, '\r', '\n')
	case "lf":
		b = append(b, '\n')
	case "cr":
		b = append(b, '\r')
	default:
		return nil, fmt.Errorf("无效 eol %q（支持 crlf/lf/cr/none）", eol)
	}
	if len(b) == 0 {
		return nil, errors.New("data 为空")
	}
	return b, nil
}

func decodeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	repl := strings.NewReplacer(" ", "", ":", "", "\t", "", "\n", "", "\r", "")
	s = repl.Replace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if s == "" {
		return nil, errors.New("hex 数据为空")
	}
	if len(s)%2 != 0 {
		return nil, errors.New("hex 长度必须为偶数")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("hex 解析失败: %w", err)
	}
	return b, nil
}

func (s *Server) jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": v})
}

func (s *Server) jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

func (s *Server) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}
