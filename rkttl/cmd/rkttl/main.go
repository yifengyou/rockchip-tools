// rkttl 是 Rockchip TTL 串口共享工具的命令行版本（Linux / Windows 通用）。
//
// 它把一块 USB 转 TTL 串口适配器（如 CH340）通过 网页控制台 / HTTP API /
// TCP 桥共享给远端服务器；本地与远端可以同时收发同一串口，
// 效果类似 tmux 的多端 attach —— 远端服务器上的 AI 由此可以间接控制 TTL。
//
// 子命令：
//
//	rkttl serve    启动服务：HTTP API + 网页控制台（可选同时启动 TCP 桥）
//	rkttl bridge   仅启动裸 TCP 桥（nc/socat 直连）
//	rkttl monitor  本地终端直连串口（stdin/stdout）
//	rkttl ports    列出本机串口
//	rkttl version  显示版本
//
// Windows 图形界面版本见 rkttl-gui。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"rkttl/internal/hub"
	"rkttl/internal/tcpbridge"
	"rkttl/internal/version"
	"rkttl/internal/ws"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "bridge":
		err = cmdBridge(os.Args[2:])
	case "monitor":
		err = cmdMonitor(os.Args[2:])
	case "ports":
		err = cmdPorts(os.Args[2:])
	case "version":
		fmt.Println("rkttl " + version.Version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("错误: %v", err)
	}
}

func usage() {
	fmt.Print(`rkttl ` + version.Version + ` — Rockchip TTL 串口多端共享工具

把本机串口（如 CH340 USB 转 TTL）通过 网页控制台 / HTTP API / TCP 桥
共享给远端服务器，本地与远端可同时收发（tmux 式多端 attach），
远端 AI 可通过 HTTP API 间接控制 TTL。

用法:
  rkttl serve    [选项]   启动服务（HTTP API + 网页控制台，可选 TCP 桥）
  rkttl bridge   [选项]   仅启动裸 TCP 桥（nc/socat 直连串口）
  rkttl monitor  [选项]   本地终端直连串口（stdin/stdout）
  rkttl ports    [选项]   列出本机串口
  rkttl version           显示版本

serve 选项:
  -listen  地址   HTTP 监听地址（默认 0.0.0.0:7373）
  -port    名字   串口名（如 COM3 或 /dev/ttyUSB0）；指定后启动时自动打开
  -baud    数值   波特率（默认 1500000，Rockchip 调试串口标准速率）
  -tcp     地址   同时启动裸 TCP 桥（如 0.0.0.0:9999）
  -token   令牌   API 访问令牌（默认自动生成并打印）
  -no-auth        关闭鉴权（仅建议在隔离网络使用）
  -buf     字节   接收环形缓冲大小（默认 1048576）
  -reopen         串口异常断开后自动重连（如 USB 重新插拔）
  -quiet           TCP 桥纯透传（不发送横幅与状态行）
  -v               详细日志

bridge 选项: -port（必填）-baud -listen（默认 0.0.0.0:9999）-buf -reopen -quiet -v

monitor 选项: -port（必填）-baud -eol crlf|lf|none（默认 crlf）-reopen -v

ports 选项: -json  以 JSON 输出

示例:
  rkttl serve -port COM3 -tcp 0.0.0.0:9999
  rkttl serve -port /dev/ttyUSB0 -listen 0.0.0.0:7373
  rkttl bridge -port /dev/ttyUSB0 -listen 0.0.0.0:9999
  rkttl monitor -port COM3 -baud 1500000
`)
}

// serialFlags 汇总串口相关的公共选项。
type serialFlags struct {
	port    string
	baud    int
	dataBit int
	stopBit int
	parity  string
	bufSize int
	reopen  bool
	quiet   bool
	verbose bool
}

func addSerialFlags(fs *flag.FlagSet, sf *serialFlags, withPort bool) {
	if withPort {
		fs.StringVar(&sf.port, "port", "", "串口名（如 COM3 或 /dev/ttyUSB0）")
	}
	fs.IntVar(&sf.baud, "baud", 1500000, "波特率")
	fs.IntVar(&sf.dataBit, "data-bits", 8, "数据位")
	fs.IntVar(&sf.stopBit, "stop-bits", 1, "停止位")
	fs.StringVar(&sf.parity, "parity", "none", "校验位 none/odd/even/mark/space")
	fs.IntVar(&sf.bufSize, "buf", 1<<20, "接收环形缓冲大小（字节）")
	fs.BoolVar(&sf.reopen, "reopen", false, "串口异常断开后自动重连")
	fs.BoolVar(&sf.quiet, "quiet", false, "TCP 桥纯透传（不发送横幅与状态行）")
	fs.BoolVar(&sf.verbose, "v", false, "详细日志")
}

func (sf *serialFlags) config() hub.SerialConfig {
	return hub.SerialConfig{
		Port:     sf.port,
		Baud:     sf.baud,
		DataBits: sf.dataBit,
		StopBits: sf.stopBit,
		Parity:   sf.parity,
	}
}

func newLogger(verbose bool) *log.Logger {
	return log.New(os.Stderr, "", log.LstdFlags)
}

// genToken 生成随机访问令牌。
func genToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "rkttl-default-token"
	}
	return hex.EncodeToString(b)
}

// waitSignal 阻塞等待退出信号（Ctrl+C 等），返回后由调用方清理。
func waitSignal() {
	<-signalCh()
}

// signalCh 返回退出信号通道（os.Interrupt / SIGTERM）。
func signalCh() <-chan os.Signal {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	return sig
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var sf serialFlags
	var listen, tcpAddr, token, logDir string
	var noAuth bool
	fs.StringVar(&listen, "listen", "0.0.0.0:7373", "HTTP 监听地址")
	fs.StringVar(&tcpAddr, "tcp", "", "同时启动裸 TCP 桥的监听地址")
	fs.StringVar(&token, "token", "", "API 访问令牌（默认自动生成）")
	fs.StringVar(&logDir, "logdir", "", "会话日志目录（串口打开时记录原始字节流；默认不记录）")
	fs.BoolVar(&noAuth, "no-auth", false, "关闭鉴权（仅建议隔离网络）")
	addSerialFlags(fs, &sf, true)
	_ = fs.Parse(args)

	logger := newLogger(sf.verbose)
	h := hub.New(sf.bufSize, sf.reopen, logger, sf.verbose)
	cfg := sf.config()
	h.SetDefault(cfg)

	if logDir != "" {
		if err := h.EnableFileLogging(logDir); err != nil {
			logger.Printf("警告: 启用会话日志失败: %v", err)
		}
	}

	if sf.port != "" {
		if err := h.Open(cfg); err != nil {
			// 打不开不致命：服务继续运行，远端可通过 /api/ports 选择其他口
			logger.Printf("警告: 启动时打开串口 %s 失败: %v（可稍后通过 API/控制台打开）", sf.port, err)
		}
	}

	if noAuth {
		token = ""
	} else if token == "" {
		token = genToken()
	}
	apiSrv := ws.New(h, token, cfg, logger)

	var bridge *tcpbridge.Bridge
	if tcpAddr != "" {
		bridge = tcpbridge.New(h, sf.quiet, sf.port != "", logger)
	}

	errCh := make(chan error, 2)
	go func() { errCh <- apiSrv.ListenAndServe(listen) }()
	if bridge != nil {
		go func() { errCh <- bridge.ListenAndServe(tcpAddr) }()
	}

	printServeBanner(listen, tcpAddr, token, h)

	go func() {
		waitSignal()
		apiSrv.Close()
		if bridge != nil {
			bridge.Close()
		}
		h.Stop()
		errCh <- nil
	}()

	err := <-errCh
	apiSrv.Close()
	if bridge != nil {
		bridge.Close()
	}
	h.Stop()
	return err
}

func printServeBanner(listen, tcpAddr, token string, h *hub.SerialHub) {
	st := h.Status()
	fmt.Println()
	fmt.Println("========================================================")
	fmt.Println(" rkttl " + version.Version + " — TTL 串口多端共享服务已启动")
	fmt.Println("--------------------------------------------------------")
	urls := ws.ConsoleURLs(listen, token)
	if len(urls) == 0 {
		fmt.Println("   网页控制台 : （未找到可用网卡，请检查网络）")
	}
	for _, u := range urls {
		line := "   网页控制台 : " + u.URL
		if u.Name != "" {
			line += "   [" + u.Name + "]"
			if u.IsVirtual {
				line += "（虚拟网卡，远端通常不可达）"
			}
		}
		fmt.Println(line)
	}
	if len(urls) > 1 {
		fmt.Println("               （上面多个地址中，选择远端可达的那个）")
	}
	if tcpAddr != "" {
		fmt.Printf("   TCP 桥     : nc <远端可达的本机IP> %s\n", portOf(tcpAddr))
	}
	if token != "" {
		fmt.Println("   访问令牌   : " + token)
	}
	if st.DefaultPort != "" {
		fmt.Printf("   默认串口   : %s @ %d %s\n", st.DefaultPort, st.DefaultBaud, boolMark(st.Open, "已打开", "未打开"))
	} else {
		fmt.Println("   串口       : 未指定（在控制台或通过 /api/open 打开）")
	}
	if st.Log.Enabled {
		fmt.Printf("   会话日志   : %s\n", st.Log.Path)
		fmt.Println("                （网页端可回放历史/导出：GET /api/export）")
	}
	fmt.Println("   API 基址   : http://<远端可达的本机IP>:" + portOf(listen) + "/api")
	fmt.Println("--------------------------------------------------------")
	fmt.Println(" Ctrl+C 退出；远端 AI 用法见 README（/api/read、/api/write）")
	fmt.Println("========================================================")
	fmt.Println()
}

func portOf(addr string) string {
	const defaultPort = "7373"
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return defaultPort
}

func boolMark(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

func cmdBridge(args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ExitOnError)
	var sf serialFlags
	var listen string
	fs.StringVar(&listen, "listen", "0.0.0.0:9999", "TCP 监听地址")
	addSerialFlags(fs, &sf, true)
	_ = fs.Parse(args)
	if sf.port == "" {
		return fmt.Errorf("bridge 模式必须用 -port 指定串口（先用 rkttl ports 查看）")
	}

	logger := newLogger(sf.verbose)
	h := hub.New(sf.bufSize, sf.reopen, logger, sf.verbose)
	cfg := sf.config()
	h.SetDefault(cfg)
	if err := h.Open(cfg); err != nil {
		return err
	}
	bridge := tcpbridge.New(h, sf.quiet, true, logger)

	go func() {
		waitSignal()
		bridge.Close()
		h.Stop()
	}()
	fmt.Printf("rkttl %s TCP 桥: 串口 %s @ %d -> tcp://%s （Ctrl+C 退出）\n", version.Version, sf.port, sf.baud, listen)
	if err := bridge.ListenAndServe(listen); err != nil {
		bridge.Close()
		h.Stop()
		return err
	}
	h.Stop()
	return nil
}
