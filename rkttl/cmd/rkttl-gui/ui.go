//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"rkttl/internal/ansi"
	"rkttl/internal/api"
	"rkttl/internal/hub"
	"rkttl/internal/tcpbridge"
	"rkttl/internal/version"
)

// ---- 应用状态 ----

type app struct {
	mw *walk.MainWindow

	hub    *hub.SerialHub
	apiSrv *api.Server
	bridge *tcpbridge.Bridge

	apiRunning bool
	apiAddr    string
	bridgeRun  bool
	bridgeAddr string
	consoleURL string // 当前对外展示的控制台地址（由所选网卡决定）

	// 部件
	portCombo  *walk.ComboBox
	baudEdit   *walk.NumberEdit
	ansiChk    *walk.CheckBox
	autoChk    *walk.CheckBox
	btnOpen    *walk.PushButton
	logView    *walk.TextEdit
	inputEdit  *walk.LineEdit
	eolCombo   *walk.ComboBox
	hexChk     *walk.CheckBox
	httpChk    *walk.CheckBox
	httpAddr   *walk.LineEdit
	httpState  *walk.Label
	tcpChk     *walk.CheckBox
	tcpAddr    *walk.LineEdit
	tcpState   *walk.Label
	nicCombo   *walk.ComboBox
	nicIfs     []api.NetIf // nicCombo 的模型（下标 0 为"全部网卡"）
	tokenEdit  *walk.LineEdit
	urlHint    *walk.Label
	logHint    *walk.Label
	statusItem *walk.StatusBarItem

	logInitErr string // 文件日志初始化错误（窗口创建后提示）

	logLen      int // 终端区近似字节数（用于滚动上限裁剪）
	portDetails []api.PortInfo
	doneCh      chan struct{}
	uiAlive     atomic.Bool // 消息循环存活标记
}

func newApp() (*app, error) {
	a := &app{
		doneCh: make(chan struct{}),
	}
	a.hub = hub.New(1<<20, true, nil, false) // 1MB 缓冲；断线自动重连默认开启
	cfg := hub.DefaultConfig("")
	a.hub.SetDefault(cfg)
	a.apiSrv = api.New(a.hub, genToken(), cfg, nil)
	// 一旦打开串口即把原始字节流记录到 exe 同目录 logs/ 下
	if dir, err := logDirDefault(); err != nil {
		a.logInitErr = "确定日志目录失败: " + err.Error()
	} else if err := a.hub.EnableFileLogging(dir); err != nil {
		a.logInitErr = "启用日志记录失败: " + err.Error()
	}
	return a, nil
}

// logDirDefault 返回 GUI 默认会话日志目录：<exe目录>/logs。
func logDirDefault() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "logs"), nil
}

func genToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "rkttl-local"
	}
	return hex.EncodeToString(b)
}

// logWriter 返回一个把内容同时写入 GUI 终端区的 io.Writer。
func (a *app) logWriter() io.Writer { return &guiLogWriter{a: a} }

type guiLogWriter struct{ a *app }

func (g *guiLogWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")
	if line == "" {
		return len(p), nil
	}
	a := g.a
	if a.uiAlive.Load() {
		a.mw.Synchronize(func() { a.appendConsole("[log] "+line+"\n", false) })
	}
	return len(p), nil
}

// ---- UI 定义 ----

func (a *app) run() error {
	var mw *walk.MainWindow

	err := (MainWindow{
		AssignTo: &mw,
		Title:    "rkttl — Rockchip TTL 串口多端共享 (v" + version.Version + ")",
		MinSize:  Size{Width: 880, Height: 580},
		Size:     Size{Width: 1060, Height: 700},
		Font:     Font{Family: "Microsoft YaHei UI", PointSize: 9},
		Layout:   VBox{Margins: Margins{Left: 8, Top: 8, Right: 8, Bottom: 4}, Spacing: 8},
		StatusBarItems: []StatusBarItem{
			{AssignTo: &a.statusItem, Text: "正在初始化...", Width: 900},
		},
		MenuItems: []MenuItem{
			Menu{
				Text: "日志(&L)",
				Items: []MenuItem{
					Action{Text: "导出日志...", OnTriggered: a.exportLog},
					Action{Text: "清空日志（含接收缓冲）", OnTriggered: a.clearLog},
					Separator{},
					Action{Text: "打开日志目录", OnTriggered: a.openLogDir},
				},
			},
		},
		Children: []Widget{
			// —— 串口工具条 ——
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "串口:"},
					ComboBox{AssignTo: &a.portCombo, MinSize: Size{Width: 170}},
					PushButton{Text: "刷新", OnClicked: a.refreshPorts},
					Label{Text: "波特率:"},
					NumberEdit{
						// 注意：declarative 的 Value/Text/ReadOnly 等字段类型是
						// Property(interface{})。Value 必须写 float64 字面量
						//（1500000.0）——写成 int 会被 assertFloat64Or 静默回退为
						// 0，再被 [MinValue,MaxValue] 判为 "value out of range"，
						// 导致 Create 失败、程序启动即崩。
						AssignTo: &a.baudEdit, Value: 1500000.0,
						MinValue: 300, MaxValue: 4000000, Decimals: 0,
						SpinButtonsVisible: true, MinSize: Size{Width: 120},
						ToolTipText: "Rockchip 调试串口通常为 1500000",
					},
					CheckBox{AssignTo: &a.ansiChk, Text: "过滤ANSI", Checked: true,
						ToolTipText: "终端区不解析颜色转义序列，仅清理显示"},
					CheckBox{AssignTo: &a.autoChk, Text: "断线自动重连", Checked: true,
						ToolTipText: "USB 重新插拔后自动恢复串口连接"},
					PushButton{AssignTo: &a.btnOpen, Text: "打开串口", OnClicked: a.toggleSerial},
					PushButton{Text: "清空缓冲", OnClicked: func() {
						a.hub.FlushBuffer()
						a.appendConsole("[sys] 接收缓冲已清空\n", true)
					}},
				},
			},
			// —— 终端区 ——
			TextEdit{
				AssignTo: &a.logView, ReadOnly: true, VScroll: true,
				MaxLength: 0x7FFFFFF0, StretchFactor: 1,
				Font:        Font{Family: "Consolas", PointSize: 10},
				ToolTipText: "本地与所有远端共享的串口输出",
			},
			// —— 输入行 ——
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					LineEdit{
						AssignTo:      &a.inputEdit,
						CueBanner:     "输入命令后回车发送（如 boot_help / ls / reset）",
						StretchFactor: 1,
						OnKeyDown: func(key walk.Key) {
							if key == walk.KeyReturn {
								a.sendInput()
							}
						},
					},
					ComboBox{AssignTo: &a.eolCombo, Model: []string{"CRLF", "LF", "不追加"},
						MinSize: Size{Width: 90}},
					CheckBox{AssignTo: &a.hexChk, Text: "HEX"},
					PushButton{Text: "Ctrl+C", OnClicked: a.sendCtrlC,
						ToolTipText: "发送 0x03 中断信号"},
					PushButton{Text: "发送", OnClicked: a.sendInput},
				},
			},
			// —— 网络共享 ——
			GroupBox{
				Title: " 网络共享（远端与本机同时收发，tmux 式 attach） ",
				Layout: Grid{Columns: 3, Spacing: 6,
					Margins: Margins{Left: 8, Top: 4, Right: 8, Bottom: 8}},
				Children: []Widget{
					CheckBox{AssignTo: &a.httpChk, Text: "HTTP API / 网页控制台", Checked: true,
						Column: 0, Row: 0,
						ToolTipText: "远端浏览器打开控制台；AI 通过 /api/* 控制"},
					LineEdit{AssignTo: &a.httpAddr, Text: "0.0.0.0:7373", Column: 1, Row: 0},
					Label{AssignTo: &a.httpState, Text: "未启动", Column: 2, Row: 0},

					CheckBox{AssignTo: &a.tcpChk, Text: "TCP 桥 (nc/socat)", Column: 0, Row: 1,
						ToolTipText: "裸 TCP 字节流，多客户端共享"},
					LineEdit{AssignTo: &a.tcpAddr, Text: "0.0.0.0:9999", Column: 1, Row: 1},
					Label{AssignTo: &a.tcpState, Text: "未启动", Column: 2, Row: 1},

					Label{Text: "对外网卡:", Column: 0, Row: 2,
						ToolTipText: "选择对远端有效的网卡；选中具体网卡时监听地址自动绑定该 IP"},
					ComboBox{AssignTo: &a.nicCombo, Column: 1, Row: 2, MinSize: Size{Width: 220},
						ToolTipText:           "虚拟网卡（VMware/WSL/Docker 等）的地址远端通常不可达；列表按物理网卡优先排序",
						OnCurrentIndexChanged: a.onNicChanged},
					Composite{Layout: HBox{MarginsZero: true, Spacing: 4}, Column: 2, Row: 2,
						Children: []Widget{
							PushButton{Text: "刷新", OnClicked: a.refreshNetIfs,
								ToolTipText: "重新枚举网卡（如插拔了网线/USB 网卡）"},
						}},

					Label{Text: "访问令牌:", Column: 0, Row: 3},
					LineEdit{AssignTo: &a.tokenEdit, ReadOnly: true, Column: 1, Row: 3},
					Composite{Layout: HBox{MarginsZero: true, Spacing: 4}, Column: 2, Row: 3,
						Children: []Widget{
							PushButton{Text: "重新生成", OnClicked: a.regenToken},
						}},

					Label{Text: "控制台地址:", Column: 0, Row: 4},
					Label{AssignTo: &a.urlHint, Text: "（启动后显示）", Column: 1, Row: 4, ColumnSpan: 2},

					Label{Text: "会话日志:", Column: 0, Row: 5,
						ToolTipText: "串口打开时自动记录原始字节流到 logs/ 目录；网络端连接时可回放/导出"},
					Label{AssignTo: &a.logHint, Text: "串口打开后开始记录", Column: 1, Row: 5, ColumnSpan: 2},

					Composite{Layout: HBox{MarginsZero: true, Spacing: 4}, Column: 0, Row: 6, ColumnSpan: 3,
						Children: []Widget{
							PushButton{Text: "应用网络设置", OnClicked: a.applyNetwork},
							PushButton{Text: "复制控制台URL", OnClicked: a.copyURL},
							PushButton{Text: "打开网页控制台", OnClicked: a.openConsole},
						}},
				},
			},
		},
	}).Create()
	if err != nil {
		return err
	}
	a.mw = mw

	// 状态变化事件驱动 UI（按钮文字/状态栏）
	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		a.uiAlive.Store(false)
	})

	a.uiAlive.Store(true)
	a.refreshPorts()
	a.refreshNetIfs()
	a.updateSerialUI()
	if a.eolCombo != nil {
		a.eolCombo.SetCurrentIndex(0)
	}
	a.tokenEdit.SetText(a.apiSrv.Token())
	a.startSubscribers()
	go a.statusLoop()
	if a.httpChk.Checked() {
		a.applyNetwork() // 默认开箱即用：自动启动 HTTP 服务
	}

	a.appendConsole(fmt.Sprintf("[sys] rkttl v%s 就绪。选择串口后点击“打开串口”；远端可通过下方地址接入。\n", version.Version), true)
	if a.logInitErr != "" {
		a.appendConsole("[sys] 会话日志不可用: "+a.logInitErr+"（串口功能不受影响）\n", true)
	} else if dir, _ := logDirDefault(); dir != "" {
		a.appendConsole("[sys] 已启用会话日志：打开串口后原始字节流将记录到 "+dir+" 目录（菜单“日志”可导出/清空）。\n", true)
	}
	mw.Run()
	return nil
}

func (a *app) shutdown() {
	a.uiAlive.Store(false)
	select {
	case <-a.doneCh:
	default:
		close(a.doneCh)
	}
	a.hub.Stop()
	a.apiSrv.Close()
	if a.bridge != nil {
		a.bridge.Close()
	}
}

// ---- 订阅与终端区 ----

// startSubscribers 把 hub 事件批量（50ms 合并）投递到 UI 线程。
func (a *app) startSubscribers() {
	ch := a.hub.Subscribe()
	go func() {
		defer a.hub.Unsubscribe(ch)
		var pending []byte
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-a.doneCh:
				return
			case ev := <-ch:
				switch ev.Type {
				case "data":
					pending = append(pending, ev.Data...)
					if len(pending) >= 16*1024 {
						a.flushPending(&pending)
					}
				case "opened", "closed":
					a.flushPending(&pending)
					if a.uiAlive.Load() {
						a.mw.Synchronize(a.updateSerialUI)
						a.mw.Synchronize(func() {
							a.appendConsole(fmt.Sprintf("[sys] 串口%s: %s\n", map[bool]string{true: "已打开", false: "已断开"}[ev.Type == "opened"], ev.Info), true)
						})
					}
				}
			case <-ticker.C:
				a.flushPending(&pending)
			}
		}
	}()
}

func (a *app) flushPending(pending *[]byte) {
	if len(*pending) == 0 {
		return
	}
	data := *pending
	*pending = nil
	if !a.uiAlive.Load() {
		return
	}
	a.mw.Synchronize(func() {
		text := string(data)
		if a.ansiChk.Checked() {
			text = ansi.Strip(text)
		}
		a.appendConsole(normalizeEOL(text), false)
	})
}

// appendConsole 追加文本到终端区并滚动到底部，超过上限裁剪最旧内容。
func (a *app) appendConsole(text string, isSys bool) {
	if a.logView == nil || text == "" {
		return
	}
	const cap = 512 * 1024
	a.logLen += len(text)
	a.logView.AppendText(text)
	if a.logLen > cap {
		s := a.logView.Text()
		cut := s[len(s)-cap/2:]
		if i := strings.IndexByte(cut, '\n'); i >= 0 {
			cut = cut[i+1:]
		}
		a.logView.SetText(cut)
		a.logLen = len(cut)
		a.appendSysNote("[sys] 终端区已达上限，已裁剪最旧内容\n")
	}
	a.logView.SendMessage(win.WM_VSCROLL, win.SB_BOTTOM, 0)
}

func (a *app) appendSysNote(s string) {
	a.logView.AppendText(s)
	a.logLen += len(s)
}

// normalizeEOL 把 \n / \r\n 统一为 Windows EDIT 控件需要的 \r\n。
func normalizeEOL(s string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// ---- 状态栏 / 串口状态 ----

func (a *app) statusLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.doneCh:
			return
		case <-ticker.C:
			if a.uiAlive.Load() {
				a.mw.Synchronize(func() {
					a.updateStatusItem()
					a.updateLogHint()
				})
			}
		}
	}
}

// updateLogHint 刷新"会话日志"行（文件名与大小）。
func (a *app) updateLogHint() {
	if a.logHint == nil {
		return
	}
	st := a.hub.Status().Log
	switch {
	case a.logInitErr != "":
		a.logHint.SetText("不可用: " + a.logInitErr)
	case !st.Enabled:
		a.logHint.SetText("（未启用）")
	case st.Path == "":
		a.logHint.SetText("串口打开后开始记录（目录 logs/）")
	default:
		a.logHint.SetText(fmt.Sprintf("%s  ·  %s  ·  含全部历史（网页端可回放/导出）",
			filepath.Base(st.Path), human(st.Written)))
	}
}

func (a *app) updateStatusItem() {
	st := a.hub.Status()
	var b strings.Builder
	if st.Open {
		fmt.Fprintf(&b, "串口 %s @ %d 已连接", st.Port, st.Baud)
	} else if st.LastError != "" {
		fmt.Fprintf(&b, "串口未打开 (%s)", st.LastError)
	} else {
		b.WriteString("串口未打开")
	}
	fmt.Fprintf(&b, "  ·  RX %s / TX %s / 缓冲 %s", human(st.Received), human(st.Sent), human(uint64(st.Buffered)))
	fmt.Fprintf(&b, "  ·  共享端 %d", st.Subscribers)
	if a.apiRunning {
		b.WriteString("  ·  HTTP ✓")
	} else {
		b.WriteString("  ·  HTTP ✗")
	}
	if a.bridgeRun {
		fmt.Fprintf(&b, "  ·  TCP桥 ✓(%d)", a.bridge.ClientCount())
	} else {
		b.WriteString("  ·  TCP桥 ✗")
	}
	a.statusItem.SetText(b.String())
}

func (a *app) updateSerialUI() {
	st := a.hub.Status()
	if a.btnOpen != nil {
		if st.Open {
			a.btnOpen.SetText("关闭串口")
		} else {
			a.btnOpen.SetText("打开串口")
		}
	}
	a.updateStatusItem()
}

func human(n uint64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2fMB", float64(n)/(1<<20))
	}
}

// ---- 操作 ----

func (a *app) refreshPorts() {
	ports, err := api.ListPorts()
	if err != nil {
		a.appendConsole("[sys] 枚举串口失败: "+err.Error()+"\n", true)
		return
	}
	a.portDetails = ports
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		label := p.Name
		if p.Product != "" {
			label += " · " + p.Product
		}
		names = append(names, label)
	}
	_ = a.portCombo.SetModel(names)
	// 自动选择：默认串口 > 第一个 CH340 > 第一项
	idx := -1
	want := a.hub.Default().Port
	for i, p := range ports {
		if want != "" && p.Name == want {
			idx = i
			break
		}
		if idx < 0 && p.VID == "1A86" { // 沁恒微 CH340/CH34x
			idx = i
		}
	}
	if idx < 0 && len(ports) > 0 {
		idx = 0
	}
	if idx >= 0 {
		_ = a.portCombo.SetCurrentIndex(idx)
	}
	if len(ports) == 0 {
		a.appendConsole("[sys] 未发现串口：请插入 USB 转 TTL 适配器并确认已安装驱动（CH340 驱动见仓库 DriverAssitant）。\n", true)
	}
}

// currentPortName 从下拉框取原始串口名（剥掉 " · 产品" 后缀）。
func (a *app) currentPortName() string {
	t := a.portCombo.Text()
	if i := strings.Index(t, " · "); i > 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

func (a *app) toggleSerial() {
	if a.hub.IsOpen() {
		_ = a.hub.Close()
		return
	}
	port := a.currentPortName()
	if port == "" {
		a.msgBoxError("请先选择串口（点击“刷新”重新枚举）")
		return
	}
	cfg := hub.DefaultConfig(port)
	if a.baudEdit != nil {
		cfg.Baud = int(a.baudEdit.Value())
	}
	a.hub.SetAutoReopen(a.autoChk.Checked())
	a.hub.SetDefault(cfg)
	if err := a.hub.Open(cfg); err != nil {
		a.msgBoxError(err.Error())
	}
	a.updateSerialUI()
}

func (a *app) sendInput() {
	text := a.inputEdit.Text()
	if text == "" {
		return
	}
	var b []byte
	if a.hexChk.Checked() {
		h, err := decodeHexLoose(text)
		if err != nil {
			a.appendConsole("[sys] HEX 解析失败: "+err.Error()+"\n", true)
			return
		}
		b = h
	} else {
		b = []byte(text)
		switch a.eolCombo.CurrentIndex() {
		case 0:
			b = append(b, '\r', '\n')
		case 1:
			b = append(b, '\n')
		case 2: // 不追加
		}
	}
	if _, err := a.hub.Write(b); err != nil {
		a.appendConsole("[sys] 发送失败: "+err.Error()+"\n", true)
		return
	}
	a.inputEdit.SetText("")
}

func (a *app) sendCtrlC() {
	if _, err := a.hub.Write([]byte{0x03}); err != nil {
		a.appendConsole("[sys] 发送失败: "+err.Error()+"\n", true)
	}
}

// decodeHexLoose 解析十六进制（容忍空格/冒号/0x 前缀）。
func decodeHexLoose(s string) ([]byte, error) {
	s = strings.NewReplacer(" ", "", ":", "", "\t", "").Replace(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return nil, fmt.Errorf("HEX 为空")
	}
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("HEX 长度必须为偶数")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := hexVal(s[2*i])
		lo, ok2 := hexVal(s[2*i+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("非法 HEX 字符 %q", s[2*i:2*i+2])
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ---- 网络共享 ----

func (a *app) applyNetwork() {
	// HTTP
	addr := strings.TrimSpace(a.httpAddr.Text())
	if a.httpChk.Checked() {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			a.msgBoxError("HTTP 监听地址无效: " + err.Error())
			return
		}
		if a.apiRunning && a.apiAddr != addr {
			a.apiSrv.Close()
			a.apiRunning = false
		}
		if !a.apiRunning {
			if err := a.apiSrv.Start(addr); err != nil {
				a.httpState.SetText("启动失败: " + err.Error())
				a.msgBoxError("HTTP 服务启动失败: " + err.Error())
			} else {
				a.apiRunning, a.apiAddr = true, addr
				a.httpState.SetText("运行中: " + addr)
			}
		}
	} else if a.apiRunning {
		a.apiSrv.Close()
		a.apiRunning = false
		a.httpState.SetText("未启动")
	}

	// TCP 桥
	addr = strings.TrimSpace(a.tcpAddr.Text())
	if a.tcpChk.Checked() {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			a.msgBoxError("TCP 监听地址无效: " + err.Error())
			return
		}
		if a.bridgeRun && a.bridgeAddr != addr && a.bridge != nil {
			a.bridge.Close()
			a.bridgeRun = false
		}
		if !a.bridgeRun {
			if a.bridge == nil {
				a.bridge = tcpbridge.New(a.hub, false, true, nil)
			}
			if err := a.bridge.Start(addr); err != nil {
				a.tcpState.SetText("启动失败: " + err.Error())
				a.msgBoxError("TCP 桥启动失败: " + err.Error())
			} else {
				a.bridgeRun, a.bridgeAddr = true, addr
				a.tcpState.SetText("运行中: " + addr)
			}
		}
	} else if a.bridgeRun && a.bridge != nil {
		a.bridge.Close()
		a.bridgeRun = false
		a.tcpState.SetText("未启动")
	}

	a.updateURLHint()
	a.updateStatusItem()
}

// refreshNetIfs 重新枚举网卡并填充"对外网卡"下拉框。
// 下标 0 为"全部网卡 (0.0.0.0)"，其后为具体网卡（物理在前，虚拟在后）。
func (a *app) refreshNetIfs() {
	ifs := api.ListNetIfs()
	a.nicIfs = ifs

	labels := make([]string, 0, len(ifs)+1)
	labels = append(labels, "全部网卡 (0.0.0.0)")
	for _, n := range ifs {
		label := n.Name + " (" + n.IP + ")"
		if n.IsVirtual {
			label += " [虚拟]"
		}
		labels = append(labels, label)
	}

	// 记住之前的选择（按 IP 匹配），网卡消失时退回"全部网卡"
	prevIP := ""
	if idx := a.nicCombo.CurrentIndex(); idx > 0 && idx-1 < len(a.nicIfs) {
		prevIP = a.nicIfs[idx-1].IP
	}
	_ = a.nicCombo.SetModel(labels)
	sel := 0
	if prevIP != "" {
		for i, n := range ifs {
			if n.IP == prevIP {
				sel = i + 1
				break
			}
		}
	}
	_ = a.nicCombo.SetCurrentIndex(sel)
}

// onNicChanged 在选择网卡后把 HTTP 监听地址同步为所选 IP
// （选"全部网卡"时为 0.0.0.0），端口保留用户当前设置；
// 点"应用网络设置"后生效。
func (a *app) onNicChanged() {
	idx := a.nicCombo.CurrentIndex()
	if idx < 0 {
		return
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(a.httpAddr.Text()))
	if err != nil || port == "" {
		port = "7373"
	}
	host := "0.0.0.0"
	if idx > 0 && idx-1 < len(a.nicIfs) {
		host = a.nicIfs[idx-1].IP
	}
	a.httpAddr.SetText(net.JoinHostPort(host, port))
}

// updateURLHint 计算当前对外的控制台地址：
//   - HTTP 监听具体 IP 时就是该 IP；
//   - 监听 0.0.0.0 时取所选网卡；选"全部网卡"时取最佳物理网卡
//     （避免把 VMware/WSL/Docker 等虚拟网卡的地址当成对外地址）。
func (a *app) updateURLHint() {
	if !a.apiRunning {
		a.consoleURL = ""
		a.urlHint.SetText("（未启动）")
		return
	}
	token := a.apiSrv.Token()
	suffix := ""
	if token != "" {
		suffix = "/?token=" + token
	}
	host, port, err := net.SplitHostPort(a.apiAddr)
	if err != nil || port == "" {
		host, port = a.apiAddr, "7373"
	}
	if host != "0.0.0.0" && host != "::" && host != "" {
		a.consoleURL = "http://" + net.JoinHostPort(host, port) + suffix
		a.urlHint.SetText(a.consoleURL)
		return
	}
	ip := ""
	if idx := a.nicCombo.CurrentIndex(); idx > 0 && idx-1 < len(a.nicIfs) {
		ip = a.nicIfs[idx-1].IP // 用户显式选择的网卡
	} else if n, ok := api.PrimaryNetIf(); ok {
		ip = n.IP // 最佳物理网卡
	}
	if ip == "" {
		a.consoleURL = ""
		a.urlHint.SetText("(未找到可用网卡)")
		return
	}
	a.consoleURL = "http://" + net.JoinHostPort(ip, port) + suffix
	a.urlHint.SetText(a.consoleURL)
}

func (a *app) regenToken() {
	tok := genToken()
	a.apiSrv.SetToken(tok)
	a.tokenEdit.SetText(tok)
	a.updateURLHint()
	a.appendConsole("[sys] 访问令牌已更换，远端需要使用新令牌。\n", true)
}

func (a *app) copyURL() {
	a.updateURLHint()
	if a.consoleURL == "" {
		a.msgBoxError("没有可用的控制台地址（HTTP 服务未启动或未找到网卡）")
		return
	}
	if err := walk.Clipboard().SetText(a.consoleURL); err != nil {
		a.msgBoxError("写入剪贴板失败: " + err.Error())
		return
	}
	a.appendConsole("[sys] 已复制: "+a.consoleURL+"\n", true)
}

func (a *app) openConsole() {
	a.updateURLHint()
	if a.consoleURL == "" {
		a.msgBoxError("没有可用的控制台地址（HTTP 服务未启动或未找到网卡）")
		return
	}
	if err := openBrowser(a.consoleURL); err != nil {
		a.msgBoxError("打开浏览器失败: " + err.Error())
	}
}

// ---- 会话日志 ----

// exportLog 导出会话日志（日志文件优先，含自打开串口以来的全部数据）。
func (a *app) exportLog() {
	data, err := a.hub.ReadLog()
	if err != nil {
		a.msgBoxError("读取日志失败: " + err.Error())
		return
	}
	if len(data) == 0 {
		a.msgBoxError("当前没有日志内容（串口打开并收到数据后开始记录）")
		return
	}
	dlg := new(walk.FileDialog)
	dlg.Title = "导出串口会话日志"
	dlg.FilePath = a.hub.LogFileName()
	accepted, err := dlg.ShowSave(a.mw)
	if err != nil {
		a.msgBoxError("打开保存对话框失败: " + err.Error())
		return
	}
	if !accepted {
		return
	}
	if err := os.WriteFile(dlg.FilePath, data, 0o644); err != nil {
		a.msgBoxError("写入文件失败: " + err.Error())
		return
	}
	a.appendConsole(fmt.Sprintf("[sys] 日志已导出: %s（%s）\n", dlg.FilePath, human(uint64(len(data)))), true)
}

// clearLog 清空接收缓冲并截断会话日志文件（文件句柄保持，继续记录新数据）。
func (a *app) clearLog() {
	a.hub.FlushBuffer()
	if err := a.hub.TruncateLog(); err != nil {
		a.appendConsole("[sys] 接收缓冲已清空（日志文件: "+err.Error()+"）\n", true)
		return
	}
	a.appendConsole("[sys] 接收缓冲与日志文件已清空\n", true)
	a.updateLogHint()
}

// openLogDir 在资源管理器中打开日志目录。
func (a *app) openLogDir() {
	dir := filepath.Dir(a.hub.Status().Log.Path)
	if dir == "." || dir == "" {
		if d, err := logDirDefault(); err == nil {
			dir = d
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	if err := exec.Command("explorer", dir).Start(); err != nil {
		a.msgBoxError("打开目录失败: " + err.Error())
	}
}

func (a *app) msgBoxError(msg string) {
	a.appendConsole("[sys] "+msg+"\n", true)
	_ = walk.MsgBox(a.mw, "rkttl", msg, walk.MsgBoxIconWarning)
}
