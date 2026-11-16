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
	"rkttl/internal/oplog"
	"rkttl/internal/tcpbridge"
	"rkttl/internal/version"
	"rkttl/internal/vt"
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
	portCombo *walk.ComboBox
	baudEdit  *walk.NumberEdit
	ansiChk   *walk.CheckBox
	autoChk   *walk.CheckBox
	btnOpen   *walk.PushButton // 工具条"打开/关闭串口"
	actNoSnap *walk.Action     // 诊断：禁用全屏快照模式
	logView   *walk.TextEdit
	opChk     *walk.CheckBox // 操作日志折叠开关
	opView    *walk.TextEdit // 操作日志视图
	httpChk   *walk.CheckBox
	httpAddr  *walk.LineEdit
	httpState *walk.Label
	tcpChk    *walk.CheckBox
	tcpAddr   *walk.LineEdit
	tcpState  *walk.Label
	nicCombo  *walk.ComboBox
	nicIfs    []api.NetIf // nicCombo 的模型（下标 0 为"全部网卡"）
	tokenEdit *walk.LineEdit
	urlHint   *walk.Label
	logHint   *walk.Label
	// 分段状态栏（XShell 风格）
	stSerial *walk.StatusBarItem
	stRx     *walk.StatusBarItem
	stTx     *walk.StatusBarItem
	stSub    *walk.StatusBarItem
	stNet    *walk.StatusBarItem
	stTheme  *walk.StatusBarItem

	// xshell 式终端状态
	inputAnchor16  int    // 本地输入行起点的 UTF-16 索引（其前为串口输出/提示符）
	pendingLine    string // 已提交待清理的本地行内容
	pendingClean16 int    // 待清理的 UTF-16 单元数（行 + EDIT 插入的 \r\n）
	oplog          *oplog.Logger
	logInitErr     string // 文件日志初始化错误（窗口创建后提示）

	// 终端主题
	curTheme     string
	themeActions []*walk.Action
	themeBrush   walk.Brush

	// 全屏应用（htop/top）支持：VT 屏幕模拟 + 快照渲染模式
	vtScreen     *vt.Screen
	snapMode     bool      // 当前处于快照渲染模式
	lastSnap     time.Time // 快照节流
	snapSaved    string    // 进入快照时的流式文本（退出时恢复）
	snapEnterDmp string    // 进入快照时的 VT 主屏基线（退出时增量接回）
	snapAnchor16 int       // 进入快照时的输入锚点

	logLen      int // 终端区近似字节数（用于滚动上限裁剪）
	portDetails []api.PortInfo
	doneCh      chan struct{}
	uiAlive     atomic.Bool // 消息循环存活标记
}

func newApp() (*app, error) {
	a := &app{
		doneCh:   make(chan struct{}),
		oplog:    oplog.New(500),
		vtScreen: vt.NewScreen(80, 24),
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
		MinSize:  Size{Width: 900, Height: 600},
		Size:     Size{Width: 1100, Height: 720},
		Font:     Font{Family: "Microsoft YaHei UI", PointSize: 9},
		Layout:   VBox{Margins: Margins{Left: 6, Top: 4, Right: 6, Bottom: 2}, Spacing: 6},
		// XShell 风格分段状态栏
		StatusBarItems: []StatusBarItem{
			{AssignTo: &a.stSerial, Text: "● 未连接", Width: 190},
			{AssignTo: &a.stRx, Text: "RX 0B", Width: 96},
			{AssignTo: &a.stTx, Text: "TX 0B", Width: 96},
			{AssignTo: &a.stSub, Text: "共享端 0", Width: 90},
			{AssignTo: &a.stNet, Text: "HTTP ✗ TCP ✗", Width: 150},
			{AssignTo: &a.stTheme, Text: "主题: 默认深色", Width: 150},
		},
		// XShell 风格工具条：Composite+PushButton 自绘（walk ToolBar 对
		// 无图标按钮渲染空白，PushButton 显示文本可靠）
		MenuItems: []MenuItem{
			Menu{
				Text: "诊断(&D)",
				Items: []MenuItem{
					Action{Text: "禁用全屏快照模式（排查 reset 卡死）", Checkable: true,
						AssignTo: &a.actNoSnap, OnTriggered: a.toggleNoSnap},
					Action{Text: "打开配置目录", OnTriggered: a.openLogDir},
				},
			},
			Menu{
				Text: "日志(&L)",
				Items: []MenuItem{
					Action{Text: "导出日志...", OnTriggered: a.exportLog},
					Action{Text: "导出屏幕内容...", OnTriggered: a.exportScreen},
					Action{Text: "清空日志（含接收缓冲）", OnTriggered: a.clearLog},
					Separator{},
					Action{Text: "打开日志目录", OnTriggered: a.openLogDir},
				},
			},
			Menu{
				Text: "视图(&V)",
				Items: append([]MenuItem{
					Action{Text: "终端主题", Enabled: false}, // 分组标题（禁用项）
					Separator{},
				}, themeMenuItems(a)...),
			},
			Menu{
				Text: "工具(&T)",
				Items: []MenuItem{
					Action{Text: "发送 HEX（解析当前输入行）", OnTriggered: a.sendHexLine},
					Action{Text: "发送 Ctrl+C (0x03)", OnTriggered: func() { a.writeAsync([]byte{0x03}, "") }},
					Action{Text: "发送 Break (100ms)", OnTriggered: func() {
						go func() {
							if err := a.hub.Break(100 * time.Millisecond); err != nil {
								if a.uiAlive.Load() {
									a.mw.Synchronize(func() {
										a.appendSys("发送 Break 失败: " + err.Error() + "\n")
									})
								}
							}
						}()
					}},
				},
			},
		},
		Children: []Widget{
			// —— 工具条（XShell 风格：核心操作一键直达） ——
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 4},
				Children: []Widget{
					PushButton{AssignTo: &a.btnOpen, Text: "打开串口", OnClicked: a.toggleSerial,
						MinSize: Size{Width: 84}},
					PushButton{Text: "刷新端口", OnClicked: a.refreshPorts},
					Label{Text: "│", TextColor: walk.RGB(128, 128, 128)},
					PushButton{Text: "导出日志", OnClicked: a.exportLog},
					PushButton{Text: "导出屏幕", OnClicked: a.exportScreen},
					PushButton{Text: "清空日志", OnClicked: a.clearLog},
					Label{Text: "│", TextColor: walk.RGB(128, 128, 128)},
					PushButton{Text: "HEX", OnClicked: a.sendHexLine,
						ToolTipText: "把当前输入行按十六进制解析并发送"},
					PushButton{Text: "Ctrl+C", OnClicked: func() { a.writeAsync([]byte{0x03}, "") },
						ToolTipText: "发送 0x03 中断信号"},
					PushButton{Text: "帮助", OnClicked: a.showTcpHelp},
				},
			},
			// —— 串口设置行（打开/关闭在工具条） ——
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					Label{Text: "串口:"},
					ComboBox{AssignTo: &a.portCombo, MinSize: Size{Width: 190}},
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
				},
			},
			// —— 终端区（xshell 式：直接在终端末尾输入，Enter 发送） ——
			TextEdit{
				AssignTo: &a.logView, VScroll: true,
				MaxLength: 0x7FFFFFF0, StretchFactor: 1,
				Font:          Font{Family: "Consolas", PointSize: 10},
				OnKeyDown:     a.termKeyDown,
				OnTextChanged: a.termChanged,
			},
			// —— 操作日志（可折叠） ——
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					CheckBox{AssignTo: &a.opChk, Text: "操作日志", OnClicked: a.toggleOpLog,
						ToolTipText: "记录串口/网络/远端操作（打开关闭、客户端增减、远端导出等）"},
					Label{Text: "（勾选展开；记录本机与远端的关键操作）",
						TextColor: walk.RGB(128, 128, 128)},
				},
			},
			TextEdit{
				AssignTo: &a.opView, ReadOnly: true, VScroll: true, Visible: false,
				MinSize: Size{Height: 96}, MaxLength: 0x7FFFFFF0,
				Font: Font{Family: "Consolas", PointSize: 9},
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
					Composite{Layout: HBox{MarginsZero: true, Spacing: 4}, Column: 2, Row: 1,
						Children: []Widget{
							Label{AssignTo: &a.tcpState, Text: "未启动"},
							PushButton{Text: "帮助", OnClicked: a.showTcpHelp,
								ToolTipText: "Linux / 远端如何连接 TCP 桥（nc / socat）—— 点击查看具体命令"},
						}},

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
	if loadDisableSnapshot() {
		vtDisableSnapshot.Store(true)
		if a.actNoSnap != nil {
			_ = a.actNoSnap.SetChecked(true)
		}
	}
	a.applyTheme(loadThemeConfig()) // 应用上次的终端主题（并持久化后续切换）
	a.tokenEdit.SetText(a.apiSrv.Token())
	// 操作日志接线：远端 API 操作 + TCP 桥客户端增减
	a.apiSrv.SetOpNotifier(func(entry string) { a.oplog.Log("%s", entry) })
	a.startSubscribers()
	go a.oplogLoop()
	go a.statusLoop()
	a.startWatchdog()
	if a.httpChk.Checked() {
		a.applyNetwork() // 默认开箱即用：自动启动 HTTP 服务
	}

	a.termWrite(fmt.Sprintf("rkttl v%s 就绪。选择串口后点击“打开串口”，随后可直接在终端中输入命令（Enter 发送）；远端可通过下方地址接入。\n", version.Version), true)
	if a.logInitErr != "" {
		a.appendSys("会话日志不可用: " + a.logInitErr + "（串口功能不受影响）\n")
	} else if dir, _ := logDirDefault(); dir != "" {
		a.appendSys("已启用会话日志：打开串口后原始字节流将记录到 " + dir + " 目录（菜单“日志”可导出/清空）。\n")
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
					if ev.Type == "opened" {
						a.oplog.Log("串口已打开: %s", ev.Info)
					} else {
						a.oplog.Log("串口断开: %s", ev.Info)
					}
					if a.uiAlive.Load() {
						a.mw.Synchronize(a.updateSerialUI)
						a.mw.Synchronize(func() {
							a.appendSys(fmt.Sprintf("串口%s: %s\n",
								map[bool]string{true: "已打开", false: "已断开"}[ev.Type == "opened"], ev.Info))
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
	// 全部输出同步喂给 VT 屏幕（保持状态连续），再判断呈现模式
	a.vtScreen.FeedBytes(data)
	fs := a.vtScreen.FullScreenActive() && !vtDisableSnapshot.Load()
	clearReq := a.vtScreen.ConsumeClearRequest()
	a.mw.Synchronize(func() {
		if fs {
			a.renderSnapshot()
			return
		}
		if a.snapMode {
			// 刚退出全屏应用：把 VT 主屏内容接回流式终端
			a.exitSnapshot()
		}
		if clearReq {
			// clear/reset 命令：清空流式终端（保留 EDIT，重置锚点）
			a.logView.SetText("")
			a.logLen = 0
			a.inputAnchor16 = 0
			a.pendingClean16 = 0
			a.pendingLine = ""
		}
		text := string(data)
		if a.ansiChk.Checked() {
			text = ansi.Strip(text)
		}
		a.termWrite(normalizeEOL(text), false)
	})
}

// renderSnapshot 快照渲染：htop/top 等全屏程序用 VT 网格的纯文本快照
// 整体刷新终端区（EDIT 控件无法网格渲染；快照保证可读不乱码），节流 300ms。
// 进入时暂存流式内容并记录 VT 主屏基线，退出时恢复并只接回增量。
func (a *app) renderSnapshot() {
	if !a.snapMode {
		a.snapMode = true
		a.pendingClean16 = 0
		a.pendingLine = ""
		// 暂存流式内容（含本地输入行）与锚点；记录 VT 主屏进入基线
		a.snapSaved = a.logView.Text()
		a.snapAnchor16 = a.inputAnchor16
		a.snapEnterDmp = a.vtScreen.Dump()
	}
	if time.Since(a.lastSnap) < 300*time.Millisecond {
		return
	}
	a.lastSnap = time.Now()
	dump := normalizeEOL(a.vtScreen.Dump())
	a.logView.SetText(dump)
	a.logLen = len(dump)
	// 光标移到末尾（EDIT 全量替换后）
	l16 := a.logView.TextLength()
	a.logView.SetTextSelection(l16, l16)
	a.scrollBottom()
}

// exitSnapshot 退出快照模式：恢复暂存的流式内容与输入锚点，
// 把 VT 期间主屏新增的内容增量接回（不重复、不丢历史）。
func (a *app) exitSnapshot() {
	a.snapMode = false
	// 恢复进入时的流式内容
	a.logView.SetText(a.snapSaved)
	a.logLen = len(a.snapSaved)
	// 清掉暂存中遗留的旧本地输入行（进入快照时未提交的部分）：
	// 它从锚点起延伸到文本末尾，恢复前先删除，避免锚点指向错乱内容
	if l16 := a.logView.TextLength(); a.snapAnchor16 >= 0 && a.snapAnchor16 < l16 {
		a.logView.SetTextSelection(a.snapAnchor16, l16)
		a.logView.ReplaceSelectedText("", false)
		if a.logLen -= l16 - a.snapAnchor16; a.logLen < 0 {
			a.logLen = 0
		}
	}
	// 增量接回 VT 期间主屏新增内容
	nowDmp := a.vtScreen.Dump()
	switch {
	case len(nowDmp) > len(a.snapEnterDmp) && strings.HasPrefix(nowDmp, a.snapEnterDmp):
		a.appendRaw(normalizeEOL(nowDmp[len(a.snapEnterDmp):]))
	case nowDmp != a.snapEnterDmp:
		a.appendRaw(normalizeEOL(nowDmp)) // 主屏被改写（罕见）：全量接回
	}
	// 恢复输入锚点（旧输入行已删除，锚点即文本末尾）
	a.inputAnchor16 = a.logView.TextLength()
	l16 := a.logView.TextLength()
	a.logView.SetTextSelection(l16, l16)
	a.appendSys("已退出全屏快照模式，恢复流式终端\n")
}

// ---- xshell 式终端 ----
//
// 终端区是一个可编辑的 TextEdit：用户直接在末尾输入（IME/粘贴/退格由
// EDIT 原生处理）。串口输出到达时，把"未提交的本地输入行"临时撤下、
// 追加输出、再把输入行放回末尾 —— 与 xshell 的滚动行为一致。
// Enter 提交：发送 line+CR 后删除本地行，显示交给设备回显。

// utf16Len 返回字符串的 UTF-16 code unit 数（EDIT 控件选区索引单位）。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16ToByteOffset 把 UTF-16 索引转换为 Go 字符串字节偏移。
func utf16ToByteOffset(s string, off16 int) int {
	if off16 <= 0 {
		return 0
	}
	n := 0
	for i, r := range s {
		if n >= off16 {
			return i
		}
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return len(s)
}

// inputLine 返回当前本地输入行（锚点之后的文本，即用户敲入、
// 未提交的内容——不含设备输出的提示符）。
func (a *app) inputLine() string {
	l16 := a.logView.TextLength()
	anchor := a.inputAnchor16
	if anchor < 0 || anchor > l16 {
		anchor = l16 // 用户把锚点之前的内容删掉了：输入行为空
	}
	full := a.logView.Text()
	return full[utf16ToByteOffset(full, anchor):]
}

// removeTail 按 UTF-16 单元删除终端末尾 n16 个字符（用于撤下本地输入行）。
func (a *app) removeTail(n16 int) {
	if n16 <= 0 {
		return
	}
	l16 := a.logView.TextLength()
	if n16 > l16 {
		n16 = l16
	}
	a.logView.SetTextSelection(l16-n16, l16)
	a.logView.ReplaceSelectedText("", false)
}

// appendRaw 追加文本（不做输入行管理），并处理上限裁剪与滚动。
func (a *app) appendRaw(text string) {
	const capBytes = 512 * 1024
	a.logLen += len(text)
	a.logView.AppendText(text)
	if a.logLen > capBytes {
		s := a.logView.Text()
		cut := s[len(s)-capBytes/2:]
		if i := strings.IndexByte(cut, '\n'); i >= 0 {
			cut = cut[i+1:]
		}
		a.logView.SetText(cut)
		a.logLen = len(cut)
		a.logView.AppendText("[sys] 终端区已达上限，已裁剪最旧内容\r\n")
		a.logLen += len("[sys] 终端区已达上限，已裁剪最旧内容\r\n")
	}
	a.scrollBottom()
}

// scrollBottom 滚动到底部；空文本时跳过（防御：部分 EDIT 版本对
// 空内容处理 WM_VSCROLL 行为异常）。
func (a *app) scrollBottom() {
	if a.logView.TextLength() > 0 {
		a.logView.SendMessage(win.WM_VSCROLL, win.SB_BOTTOM, 0)
	}
}

// termWrite 把一段内容写入终端：先撤下本地输入行，追加内容，
// 再把输入行放回末尾并重置锚点，光标移到输入行尾。
// 锚点（inputAnchor16）标记"串口输出与本地输入的分界"：
// 其后是用户敲入未提交的字符，提交时只发送这部分 —— 修复把
// 设备提示符（如 "=> "）一并当作输入发送的乱码问题。
func (a *app) termWrite(text string, isSys bool) {
	if a.logView == nil || text == "" {
		return
	}
	// 先清掉未完成的提交残留（本处在消息循环闭包栈，安全）
	a.doPendingClean()
	line := a.inputLine()
	line16 := utf16Len(line)
	a.removeTail(line16)
	if isSys {
		a.appendRaw("[sys] " + text)
	} else {
		a.appendRaw(text)
	}
	if line != "" {
		a.appendRaw(normalizeEOL(line))
	}
	// 重置锚点：输入行重新放回了末尾
	a.inputAnchor16 = a.logView.TextLength() - utf16Len(line)
	// 光标/选区移到末尾（输入行末尾）
	l16 := a.logView.TextLength()
	a.logView.SetTextSelection(l16, l16)
}

// appendConsole 是 termWrite 的别名（历史调用点）。
func (a *app) appendConsole(text string, isSys bool) {
	a.termWrite(text, isSys)
}

func (a *app) appendSys(s string) {
	a.termWrite(s, true)
}

// termSubmit 处理 Enter：发送锚点之后的本地输入行 + CR（空行发送裸 CR，
// 与真实终端一致）；串口写异步执行。本地行的显示清理采用惰性机制：
// 只记录 pendingLine/pendingClean16，绝不在 EN_CHANGE 通知栈内修改
// EDIT 文本（Win32 EDIT 在该栈内再改文本会内部状态损坏/死循环），
// 清理延后到安全栈（下次按键、输出渲染或兜底定时）执行。
func (a *app) termSubmit() {
	// 先清掉上一笔提交的残留（本处在 WM_KEYDOWN 栈，安全）
	a.doPendingClean()
	// 光标移到末尾，确保 EDIT 默认的 \r\n 插在输入行之后
	l16 := a.logView.TextLength()
	a.logView.SetTextSelection(l16, l16)
	line := a.inputLine()
	a.writeAsync([]byte(line+"\r"), line)
	// 记录待清理：line + EDIT 即将插入的 \r\n
	a.pendingLine = line
	a.pendingClean16 = utf16Len(line) + 2
	// 兜底：若之后既无输出也无按键，也把残留清掉（消息循环栈执行）
	go func() {
		time.Sleep(30 * time.Millisecond)
		if a.uiAlive.Load() {
			a.mw.Synchronize(a.doPendingClean)
		}
	}()
}

// doPendingClean 清理最近一次提交的本地行（含 EDIT 插入的换行）。
// 只在安全栈调用（WM_KEYDOWN / 消息循环闭包），且删除前校验末尾内容
// 与提交时一致 —— 期间文本被改写则放弃，永不误删。
func (a *app) doPendingClean() {
	n16 := a.pendingClean16
	if n16 <= 0 {
		return
	}
	a.pendingClean16 = 0
	line := a.pendingLine
	a.pendingLine = ""
	l16 := a.logView.TextLength()
	if n16 > l16 {
		return
	}
	full := a.logView.Text()
	tail := full[utf16ToByteOffset(full, l16-n16):]
	if tail != line+"\r\n" {
		return // 状态不符（期间被改写）：放弃清理
	}
	a.removeTail(n16)
	a.inputAnchor16 = a.logView.TextLength()
	l16 = a.logView.TextLength()
	a.logView.SetTextSelection(l16, l16)
	a.scrollBottom()
}

// termChanged 是 EDIT 的 EN_CHANGE 回调：这里【只】夹紧锚点，
// 绝不修改 EDIT 文本 —— 在 EN_CHANGE 通知栈内发送 EM_* 修改消息
// 会重入 EDIT 内部（Win32 已知禁区，导致卡死/状态损坏）。
func (a *app) termChanged() {
	if l16 := a.logView.TextLength(); a.inputAnchor16 > l16 {
		a.inputAnchor16 = l16
	}
}

// writeAsync 在后台 goroutine 写串口（串口 Write/Drain 可能阻塞，
// 必须移出 UI 线程，否则快速按键会导致窗口假死）。
func (a *app) writeAsync(payload []byte, lineForMsg string) {
	go func() {
		if _, err := a.hub.Write(payload); err != nil {
			if a.uiAlive.Load() {
				a.mw.Synchronize(func() {
					a.appendSys("发送失败: " + err.Error() + "\n")
				})
			}
		}
	}()
}

// termKeyDown 处理终端键盘。快照模式（全屏程序）下按键直接映射为
// 终端序列发送（方向键/翻页/F 键/字符/Enter）；流式模式下：Enter 提交
// 本地输入行、Tab/Esc 透传、Ctrl+C 一律发 0x03、Ctrl+D/U 透传。
func (a *app) termKeyDown(key walk.Key) {
	sendByte := func(b byte) { a.writeAsync([]byte{b}, "") }
	sendStr := func(s string) { a.writeAsync([]byte(s), "") }

	// —— 快照模式（htop/top 等全屏程序）：按键即终端序列 ——
	if a.snapMode {
		ctrl := walk.ModifiersDown()&walk.ModControl != 0
		switch key {
		case walk.KeyReturn:
			sendByte(0x0d)
		case walk.KeyUp:
			sendStr("\x1b[A")
		case walk.KeyDown:
			sendStr("\x1b[B")
		case walk.KeyRight:
			sendStr("\x1b[C")
		case walk.KeyLeft:
			sendStr("\x1b[D")
		case walk.KeyPrior: // PageUp
			sendStr("\x1b[5~")
		case walk.KeyNext: // PageDown
			sendStr("\x1b[6~")
		case walk.KeyHome:
			sendStr("\x1b[H")
		case walk.KeyEnd:
			sendStr("\x1b[F")
		case walk.KeyDelete:
			sendStr("\x1b[3~")
		case walk.KeyInsert:
			if !ctrl {
				sendStr("\x1b[2~")
			}
		case walk.KeyF1:
			sendStr("\x1bOP")
		case walk.KeyF2:
			sendStr("\x1bOQ")
		case walk.KeyF3:
			sendStr("\x1bOR")
		case walk.KeyF4:
			sendStr("\x1bOS")
		case walk.KeyF5:
			sendStr("\x1b[15~")
		case walk.KeyF6:
			sendStr("\x1b[17~")
		case walk.KeyF7:
			sendStr("\x1b[18~")
		case walk.KeyF8:
			sendStr("\x1b[19~")
		case walk.KeyF9:
			sendStr("\x1b[20~")
		case walk.KeyF10:
			sendStr("\x1b[21~")
		default:
			if ctrl {
				switch key {
				case walk.KeyC:
					sendByte(0x03)
				case walk.KeyD:
					sendByte(0x04)
				case walk.KeyU:
					sendByte(0x15)
				}
				return
			}
			// 字母/数字等可打印键：虚拟键即 ASCII 大写，按 Shift 还原小写
			if c := vkToASCII(key, walk.ModifiersDown()&walk.ModShift != 0); c != 0 {
				sendByte(c)
			}
		}
		return
	}

	// —— 流式模式（本地输入行 + 透传控制键）——
	switch key {
	case walk.KeyReturn:
		a.termSubmit()
	case walk.KeyTab:
		sendByte(0x09)
		// EDIT 默认会把焦点切走；异步抢回，保证继续输入
		a.mw.Synchronize(func() { _ = a.logView.SetFocus() })
	case walk.KeyEscape:
		sendByte(0x1b)
	default:
		if walk.ModifiersDown()&walk.ModControl == 0 {
			return
		}
		switch key {
		case walk.KeyC:
			// 终端聚焦时 Ctrl+C 一律映射为 TTL 中断信号
			// （有选区时 EDIT 会同时复制到剪贴板，不影响串口）
			sendByte(0x03)
		case walk.KeyD:
			sendByte(0x04)
		case walk.KeyU:
			sendByte(0x15)
		}
	}
}

// vkToASCII 把字母/数字虚拟键转为 ASCII 字节（考虑 Shift 大小写）。
func vkToASCII(key walk.Key, shift bool) byte {
	switch {
	case key >= '0' && key <= '9':
		return byte(key)
	case key >= 'A' && key <= 'Z':
		if shift {
			return byte(key)
		}
		return byte(key) + 32 // 小写
	}
	return 0 // 符号/中文等交由 EDIT 默认行为（快照刷新会覆盖显示）
}

// sendHexLine 把当前未提交的输入行按十六进制解析并发送（工具菜单）。
func (a *app) sendHexLine() {
	line := a.inputLine()
	if line == "" {
		a.appendSys("HEX 发送：当前输入行为空（先在终端里输入十六进制字节，如 0d0a41）\n")
		return
	}
	b, err := decodeHexLoose(line)
	if err != nil {
		a.appendSys("HEX 解析失败: " + err.Error() + "\n")
		return
	}
	a.writeAsync(b, line)
	a.removeTail(utf16Len(line))
	a.inputAnchor16 = a.logView.TextLength()
	l16 := a.logView.TextLength()
	a.logView.SetTextSelection(l16, l16)
	a.appendSys(fmt.Sprintf("已按 HEX 发送 %d 字节（本地输入行已清除）\n", len(b)))
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
					heartbeat.Add(1) // UI 心跳（看门狗据此判断线程存活）
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
	// 分段 1：连接状态（XShell 风格 ● 标记）
	if st.Open {
		_ = a.stSerial.SetText(fmt.Sprintf("● %s @ %d", st.Port, st.Baud))
	} else if st.LastError != "" {
		_ = a.stSerial.SetText("● 未连接: " + st.LastError)
	} else {
		_ = a.stSerial.SetText("● 未连接")
	}
	_ = a.stRx.SetText("RX " + human(st.Received))
	_ = a.stTx.SetText("TX " + human(st.Sent))
	_ = a.stSub.SetText(fmt.Sprintf("共享端 %d", st.Subscribers))
	net := "HTTP ✗"
	if a.apiRunning {
		net = "HTTP ✓"
	}
	if a.bridgeRun {
		net += fmt.Sprintf(" TCP ✓(%d)", a.bridge.ClientCount())
	} else {
		net += " TCP ✗"
	}
	_ = a.stNet.SetText(net)
}

func (a *app) updateSerialUI() {
	st := a.hub.Status()
	text := "打开串口"
	if st.Open {
		text = "关闭串口"
	}
	if a.btnOpen != nil {
		a.btnOpen.SetText(text)
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

// portOf 提取 "host:port" 中的端口。
func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return addr
}

// ---- 操作 ----

func (a *app) refreshPorts() {
	ports, err := api.ListPorts()
	if err != nil {
		a.appendSys("枚举串口失败: " + err.Error() + "\n")
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
		a.appendSys("未发现串口：请插入 USB 转 TTL 适配器并确认已安装驱动（CH340 驱动见仓库 DriverAssitant）。\n")
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

// ---- 操作日志（可折叠） ----

// toggleOpLog 展开或折叠操作日志面板。
func (a *app) toggleOpLog() {
	if a.opView != nil {
		a.opView.SetVisible(a.opChk.Checked())
		if a.opChk.Checked() {
			a.opView.SetText(a.oplog.Text())
		}
	}
}

// oplogLoop 把操作日志变更刷新到 UI。
func (a *app) oplogLoop() {
	for {
		ch := a.oplog.Changed()
		select {
		case <-a.doneCh:
			return
		case <-ch:
		}
		if a.uiAlive.Load() {
			a.mw.Synchronize(func() {
				if a.opView != nil && a.opView.Visible() {
					a.opView.SetText(a.oplog.Text())
				}
			})
		}
	}
}

// ---- TCP 桥帮助 ----

// showTcpHelp 弹出"远端如何连接 TCP 桥"帮助（nc / socat 命令）。
func (a *app) showTcpHelp() {
	port := "9999"
	if a.tcpAddr != nil {
		if _, p, err := net.SplitHostPort(strings.TrimSpace(a.tcpAddr.Text())); err == nil && p != "" {
			port = p
		}
	}
	ip := "<rkttl主机IP>"
	if n, ok := api.PrimaryNetIf(); ok {
		ip = n.IP
	}
	urls := api.ConsoleURLs("0.0.0.0:7373", "")
	content := fmt.Sprintf(
		"Linux / 远端连接 TCP 桥（端口 %s，主机 %s）\r\n"+
			"================================================================\r\n\r\n"+
			"1) nc 交互（最简单）：\r\n"+
			"    nc %s %s\r\n\r\n"+
			"2) socat 终端模式（回显更接近真实终端）：\r\n"+
			"    socat -,raw,echo=0 TCP:%s:%s\r\n\r\n"+
			"3) 伪装成本地串口设备（供只认 /dev/tty* 的工具使用）：\r\n"+
			"    socat pty,link=/tmp/ttyrkttl,raw TCP:%s:%s\r\n"+
			"    screen /tmp/ttyrkttl 1500000\r\n\r\n"+
			"说明：\r\n"+
			"· TCP 桥支持多客户端同时收发，与网页控制台共享同一路串口\r\n"+
			"  （tmux 式 attach）；连接后先收到一行 rkttl 状态横幅\r\n"+
			"· 首次开桥请在 Windows 防火墙弹窗中允许该端口\r\n"+
			"· 程序化访问建议用 HTTP API（curl），见项目 README\r\n"+
			"· 网页控制台地址：\r\n", port, ip, ip, port, ip, port, ip, port)
	for _, u := range urls {
		content += "    " + u.URL + "  [" + u.Name + "]\r\n"
	}

	var dlg *walk.Dialog
	var txt *walk.TextEdit
	_, err := (Dialog{
		AssignTo: &dlg,
		Title:    "远端如何连接 TCP 桥",
		MinSize:  Size{Width: 640, Height: 420},
		Layout:   VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}},
		Children: []Widget{
			TextEdit{
				AssignTo: &txt, ReadOnly: true, VScroll: true,
				Text: content, StretchFactor: 1,
				Font: Font{Family: "Consolas", PointSize: 9},
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					PushButton{Text: "复制 nc 命令", OnClicked: func() {
						_ = walk.Clipboard().SetText("nc " + ip + " " + port)
					}},
					PushButton{Text: "复制 socat 命令", OnClicked: func() {
						_ = walk.Clipboard().SetText("socat -,raw,echo=0 TCP:" + ip + ":" + port)
					}},
					PushButton{Text: "复制全部说明", OnClicked: func() {
						_ = walk.Clipboard().SetText(strings.ReplaceAll(content, "\r\n", "\n"))
					}},
					PushButton{Text: "关闭", OnClicked: func() { dlg.Accept() }},
				},
			},
		},
	}).Run(a.mw)
	if err != nil {
		a.msgBoxError("打开帮助失败: " + err.Error())
	}
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
				a.oplog.Log("HTTP 服务启动失败(%s): %v", addr, err)
			} else {
				a.apiRunning, a.apiAddr = true, addr
				a.httpState.SetText("运行中: " + addr)
				a.oplog.Log("HTTP 服务已启动: %s", addr)
			}
		}
	} else if a.apiRunning {
		a.apiSrv.Close()
		a.apiRunning = false
		a.httpState.SetText("未启动")
		a.oplog.Log("HTTP 服务已停止")
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
				a.bridge.SetClientNotifier(func(n int, joined bool) {
					if joined {
						a.oplog.Log("TCP 桥客户端接入（当前 %d 台）", n)
					} else {
						a.oplog.Log("TCP 桥客户端断开（剩余 %d 台）", n)
					}
				})
			}
			if err := a.bridge.Start(addr); err != nil {
				a.tcpState.SetText("启动失败: " + err.Error())
				a.msgBoxError("TCP 桥启动失败: " + err.Error())
				a.oplog.Log("TCP 桥启动失败(%s): %v", addr, err)
			} else {
				a.bridgeRun, a.bridgeAddr = true, addr
				a.tcpState.SetText("运行中: " + addr)
				a.oplog.Log("TCP 桥已启动: %s（Linux 端: nc <本机IP> %s）", addr, portOf(addr))
			}
		}
	} else if a.bridgeRun && a.bridge != nil {
		a.bridge.Close()
		a.bridgeRun = false
		a.tcpState.SetText("未启动")
		a.oplog.Log("TCP 桥已停止")
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
	a.oplog.Log("访问令牌已重新生成")
	a.appendSys("访问令牌已更换，远端需要使用新令牌。\n")
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
	a.appendSys("已复制: " + a.consoleURL + "\n")
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
	a.oplog.Log("已导出会话日志: %s（%s）", dlg.FilePath, human(uint64(len(data))))
	a.appendSys(fmt.Sprintf("日志已导出: %s（%s）\n", dlg.FilePath, human(uint64(len(data)))))
}

// exportScreen 导出当前终端屏幕的显示内容（纯文本，含快照模式下的
// htop/top 屏幕快照）。
func (a *app) exportScreen() {
	text := strings.ReplaceAll(a.logView.Text(), "\r\n", "\n")
	if strings.TrimSpace(text) == "" {
		a.msgBoxError("当前屏幕没有内容")
		return
	}
	dlg := new(walk.FileDialog)
	dlg.Title = "导出屏幕内容"
	dlg.FilePath = "rkttl-screen-" + time.Now().Format("20060102-150405") + ".txt"
	accepted, err := dlg.ShowSave(a.mw)
	if err != nil {
		a.msgBoxError("打开保存对话框失败: " + err.Error())
		return
	}
	if !accepted {
		return
	}
	if err := os.WriteFile(dlg.FilePath, []byte(text), 0o644); err != nil {
		a.msgBoxError("写入文件失败: " + err.Error())
		return
	}
	a.oplog.Log("已导出屏幕内容: %s", dlg.FilePath)
	a.appendSys(fmt.Sprintf("屏幕内容已导出: %s\n", dlg.FilePath))
}

// clearLog 清空接收缓冲并截断会话日志文件（文件句柄保持，继续记录新数据）。
func (a *app) clearLog() {
	a.hub.FlushBuffer()
	if err := a.hub.TruncateLog(); err != nil {
		a.appendSys("接收缓冲已清空（日志文件: " + err.Error() + "）\n")
		return
	}
	a.oplog.Log("已清空接收缓冲与会话日志")
	a.appendSys("接收缓冲与日志文件已清空\n")
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

// toggleNoSnap 切换"禁用全屏快照模式"诊断开关（持久化）。
func (a *app) toggleNoSnap() {
	v := a.actNoSnap.Checked()
	vtDisableSnapshot.Store(v)
	saveGUIConfig(a.curTheme, v)
	if v {
		a.appendSys("已禁用全屏快照模式（诊断模式）：htop/top 将以流式显示，若 reset 不再卡死请反馈开发者\n")
		if a.snapMode {
			a.exitSnapshot()
		}
	} else {
		a.appendSys("已恢复全屏快照模式\n")
	}
}

func (a *app) msgBoxError(msg string) {
	a.appendSys("" + msg + "\n")
	_ = walk.MsgBox(a.mw, "rkttl", msg, walk.MsgBoxIconWarning)
}
