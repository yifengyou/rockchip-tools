// Package api 提供 rkttl 的 HTTP 层：JSON API、SSE 实时数据流与内嵌网页控制台。
// 远端服务器上的 AI / 浏览器 / 脚本通过该层间接控制串口，
// 与本地 GUI / monitor 共享同一路输入输出（tmux 式多端 attach）。
package api

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

	"rkttl/internal/hub"
	"rkttl/internal/version"
)

// Server 是 rkttl 的 HTTP 服务。
type Server struct {
	hub    *hub.SerialHub
	defCfg hub.SerialConfig

	tokenMu sync.RWMutex
	token   string // 为空表示关闭鉴权

	mu       sync.Mutex
	srv      *http.Server
	ln       net.Listener
	done     chan struct{} // Start 成功后非 nil，服务结束时关闭
	serveErr error

	opMu       sync.Mutex
	opNotifier func(entry string) // 远端关键操作通知（可为 nil）

	log *log.Logger // 可为 nil
}

// New 创建 HTTP 服务。token 为空字符串表示关闭鉴权（仅建议隔离网络使用）。
func New(h *hub.SerialHub, token string, defCfg hub.SerialConfig, logger *log.Logger) *Server {
	return &Server{hub: h, token: token, defCfg: defCfg, log: logger}
}

// SetToken 运行期更换访问令牌。
func (s *Server) SetToken(token string) {
	s.tokenMu.Lock()
	s.token = token
	s.tokenMu.Unlock()
}

// Token 返回当前访问令牌（可能为空）。
func (s *Server) Token() string {
	s.tokenMu.RLock()
	t := s.token
	s.tokenMu.RUnlock()
	return t
}

// SetOpNotifier 设置远端关键操作的回调（如"打开串口"），用于操作日志。
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

// Start 在 addr 上同步建立监听（立即返回监听错误），HTTP 服务在后台运行。
// 适合 GUI 场景；结束状态可用 Done() / Err() 查询。
func (s *Server) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.srv = &http.Server{Handler: s.handler()}
	s.ln = ln
	done := make(chan struct{})
	s.done = done
	s.mu.Unlock()
	go func() {
		err := s.srv.Serve(ln)
		s.mu.Lock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr = err
		}
		s.mu.Unlock()
		close(done)
	}()
	s.logf("HTTP 服务已启动: http://%s", ln.Addr())
	return nil
}

// Done 返回一个通道，服务结束时关闭（Start 之前为 nil）。
func (s *Server) Done() <-chan struct{} {
	s.mu.Lock()
	d := s.done
	s.mu.Unlock()
	return d
}

// Err 返回服务异常退出的原因（正常关闭为 nil）。
func (s *Server) Err() error {
	s.mu.Lock()
	e := s.serveErr
	s.mu.Unlock()
	return e
}

// ListenAndServe 在 addr 上启动 HTTP 服务并阻塞，直至 Close 或出错（CLI 用）。
func (s *Server) ListenAndServe(addr string) error {
	if err := s.Start(addr); err != nil {
		return err
	}
	<-s.Done()
	return s.Err()
}

// Close 停止 HTTP 服务。
func (s *Server) Close() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
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
	mux.HandleFunc("/api/stream", s.handleStream)
	return s.auth(mux)
}

// auth 校验访问令牌，支持 Authorization: Bearer <token>、
// X-Auth-Token 头以及 ?token= 查询参数（便于浏览器/SSE 使用）。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.tokenMu.RLock()
		token := s.token
		s.tokenMu.RUnlock()
		if token != "" {
			ok := false
			if h := r.Header.Get("Authorization"); h == "Bearer "+token || h == "token "+token || h == token {
				ok = true
			}
			if !ok && r.Header.Get("X-Auth-Token") == token {
				ok = true
			}
			if !ok && r.URL.Query().Get("token") == token {
				ok = true
			}
			if !ok {
				s.jsonError(w, http.StatusUnauthorized, "无效或缺失的访问令牌")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 处理器 ----

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// 控制台页面禁用缓存：确保升级 rkttl 后浏览器总是拿到最新页面，
	// 避免旧版页面与新服务端的兼容问题。
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

// GET /api/ports — 列出本机串口（含 USB VID/PID 信息）
func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 GET")
		return
	}
	ports, err := ListPorts()
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.jsonOK(w, map[string]any{"ports": ports, "count": len(ports)})
}

// GET /api/netifs — 列出本机网卡与各网卡的网页控制台地址
// （is_virtual 标记虚拟网卡：VMware/WSL/Docker 等，远端通常不可达）
func (s *Server) handleNetIfs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 GET")
		return
	}
	ifs := ListNetIfs()
	s.jsonOK(w, map[string]any{
		"netifs":       ifs,
		"console_urls": s.consoleURLs(),
	})
}

// consoleURLs 基于当前 HTTP 服务的监听地址与令牌生成各网卡的控制台地址。
func (s *Server) consoleURLs() []ConsoleURL {
	s.mu.Lock()
	addr := ""
	if s.ln != nil {
		addr = s.ln.Addr().String()
	}
	s.mu.Unlock()
	if addr == "" {
		return nil
	}
	return ConsoleURLs(addr, s.Token())
}

// GET /api/status — 服务与串口状态
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 GET")
		return
	}
	s.jsonOK(w, map[string]any{
		"version": version.Version,
		"auth":    s.Token() != "",
		"serial":  s.hub.Status(),
	})
}

// POST /api/open — 打开串口 {port, baud?, data_bits?, stop_bits?, parity?}
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
	if err := s.readBody(w, r, &req); err != nil {
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

// POST /api/close — 关闭串口
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

// POST /api/write — 写入串口 {data, hex?, eol?}
//
//	data: 文本（UTF-8）或 hex=true 时的十六进制字符串（可含空格/冒号）
//	eol:  追加行尾，"crlf" | "lf" | "cr" | "none"/""（默认不追加）
func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 POST")
		return
	}
	var req struct {
		Data string `json:"data"`
		Hex  bool   `json:"hex"`
		EOL  string `json:"eol"`
		CRLF bool   `json:"crlf"` // 兼容写法：等价 eol=crlf
	}
	if err := s.readBody(w, r, &req); err != nil {
		return
	}
	var b []byte
	var err error
	if req.Hex {
		b, err = decodeHex(req.Data)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		b = []byte(req.Data)
	}
	if req.EOL == "" && req.CRLF {
		req.EOL = "crlf"
	}
	switch strings.ToLower(req.EOL) {
	case "", "none":
	case "crlf":
		b = append(b, '\r', '\n')
	case "lf":
		b = append(b, '\n')
	case "cr":
		b = append(b, '\r')
	default:
		s.jsonError(w, http.StatusBadRequest, fmt.Sprintf("无效 eol %q（支持 crlf/lf/cr/none）", req.EOL))
		return
	}
	if len(b) == 0 {
		s.jsonError(w, http.StatusBadRequest, "data 为空")
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

// GET /api/read?since=&max=&wait_ms= — 非破坏性读取串口接收数据。
//
//	since:  上一次返回的 seq；缺省表示"取最近 4096 字节"；0 表示从缓冲区最旧开始
//	max:    本次最多返回的字节数（缺省不限）
//	wait_ms: 无新数据时的最长等待（默认 0；-1 等价 60000，上限 60000）
//
// 返回 {seq, truncated, n, b64, text, hex, buffered}。
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 GET")
		return
	}
	q := r.URL.Query()
	var since uint64
	var haveSince bool
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
	waitMs := 0
	if v := q.Get("wait_ms"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.jsonError(w, http.StatusBadRequest, "wait_ms 需为整数")
			return
		}
		waitMs = n
	}
	wait := time.Duration(waitMs) * time.Millisecond
	if waitMs < 0 {
		wait = 60 * time.Second
	}

	if !haveSince {
		// 缺省：只看"最近的屏幕" —— 取缓冲区尾部（默认 4KB，或 max 与之较小者）
		tail := 4096
		if max > 0 && max < tail {
			tail = max
		}
		cur := s.hub.Seq()
		lo := uint64(tail)
		if cur > lo {
			lo = cur - lo
		} else {
			lo = 0
		}
		res := s.hub.Read(lo, tail, 0)
		s.writeReadResp(w, res)
		return
	}
	res := s.hub.Read(since, max, wait)
	s.writeReadResp(w, res)
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
		resp["text"] = toText(res.Data)
		resp["hex"] = hex.EncodeToString(res.Data)
	}
	s.jsonOK(w, resp)
}

// POST /api/flush — 清空接收缓冲（seq 保持连续）。
// 可选 body {"file": true} 同时清空会话日志文件。
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
			_ = json.Unmarshal(body, &req) // 解析失败按空 body 处理（兼容）
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
		s.notifyOp("远端(%s) 清空%s", r.RemoteAddr, func() string {
			if resp["file_truncated"] == true {
				return "接收缓冲与会话日志"
			}
			return "接收缓冲"
		}())
	}
	s.jsonOK(w, resp)
}

// GET /api/export — 下载会话日志（日志文件优先，未启用文件记录时为
// 当前接收缓冲内容）。返回原始字节流（application/octet-stream）。
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 GET")
		return
	}
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

// POST /api/break — 发送 break 信号 {ms?}
func (s *Server) handleBreak(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.jsonError(w, http.StatusMethodNotAllowed, "需要 POST")
		return
	}
	var req struct {
		MS int `json:"ms"`
	}
	if err := s.readBody(w, r, &req); err != nil {
		return
	}
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

// GET /api/stream — SSE 实时流：每个连接都会收到串口的全部输出（多端共享）。
//
// 连接建立时先回放接收缓冲中的历史数据（作为带 seq 的 data 事件，
// 16KB 分块），随后转发实时事件并按 seq 去重 —— 新接入的网页控制台
// 也能看到历史窗口，且不重不漏。查询参数：
//
//	history=N   只回放最近 N 字节（默认全部缓冲；0 = 关闭回放）
//
// 事件类型：data({seq,b64}) / status({type,open,info})；每 15s 发送一次注释保活。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.jsonError(w, http.StatusInternalServerError, "当前连接不支持流式响应")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	writeEvent := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// 连接建立时先推一次当前状态
	st := s.hub.Status()
	if !writeEvent("status", map[string]any{"type": "hello", "open": st.Open, "info": statusLine(st)}) {
		return
	}

	// 历史回放：先读缓冲（Subscribe 之后读取，覆盖到读取时刻为止），
	// 之后到达的实时事件 seq 必然大于回放末端，据此去重。
	histLimit := -1 // -1 = 全部缓冲，0 = 关闭，>0 = 最近 N 字节
	if v := r.URL.Query().Get("history"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			histLimit = n
		}
	}
	lastSeq := uint64(0) // 去重基线；0 表示不去重（history=0 时全部转发）
	if histLimit != 0 {
		var res hub.ReadResult
		if histLimit > 0 {
			cur := s.hub.Seq()
			lo := uint64(histLimit)
			if cur > lo {
				lo = cur - lo
			} else {
				lo = 0
			}
			res = s.hub.Read(lo, 0, 0)
		} else {
			res = s.hub.Read(0, 0, 0)
		}
		const chunkSize = 16 * 1024
		for off := 0; off < len(res.Data); {
			n := len(res.Data) - off
			if n > chunkSize {
				n = chunkSize
			}
			block := res.Data[off : off+n]
			seq := res.Seq - uint64(len(res.Data)-off-n)
			if !writeEvent("data", map[string]any{"seq": seq, "b64": base64.StdEncoding.EncodeToString(block)}) {
				return
			}
			off += n
		}
		lastSeq = res.Seq
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-ch:
			switch ev.Type {
			case "data":
				if ev.Seq <= lastSeq {
					continue // 已包含在历史回放中（订阅与回放间隙的重复）
				}
				lastSeq = ev.Seq
				if !writeEvent("data", map[string]any{"seq": ev.Seq, "b64": base64.StdEncoding.EncodeToString(ev.Data)}) {
					return
				}
			case "opened", "closed":
				if !writeEvent("status", map[string]any{"type": ev.Type, "open": s.hub.IsOpen(), "info": ev.Info}) {
					return
				}
			}
		}
	}
}

// ---- 辅助 ----

func (s *Server) readBody(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		s.jsonError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return err
	}
	if len(body) == 0 {
		s.jsonError(w, http.StatusBadRequest, "请求体为空（需要 JSON）")
		return errors.New("empty body")
	}
	if err := json.Unmarshal(body, v); err != nil {
		s.jsonError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return err
	}
	return nil
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

// toText 把字节按 UTF-8 尽力解码（非法字节替换为 U+FFFD）。
func toText(b []byte) string {
	return strings.ToValidUTF8(string(b), string([]byte{0xEF, 0xBF, 0xBD}))
}

// decodeHex 解析十六进制字符串，容忍空格、冒号与 0x 前缀。
func decodeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return nil, errors.New("hex 数据为空")
	}
	if len(s)%2 != 0 {
		return nil, errors.New("hex 长度必须为偶数")
	}
	return hex.DecodeString(s)
}

func statusLine(st hub.Status) string {
	if st.Open {
		return fmt.Sprintf("%s @ %d 已连接", st.Port, st.Baud)
	}
	if st.LastError != "" {
		return "串口未打开: " + st.LastError
	}
	return "串口未打开"
}
