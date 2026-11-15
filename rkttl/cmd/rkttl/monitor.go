package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"rkttl/internal/hub"
)

// cmdMonitor 在本地终端直连串口：stdin 逐行写入（默认补 \r\n），
// 串口输出原样写到 stdout（终端本身即可渲染 ANSI 颜色）。
// 它与其他订阅者（网页控制台 / TCP 桥）共享同一路串口输出。
func cmdMonitor(args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ExitOnError)
	var sf serialFlags
	var eol string
	fs.StringVar(&eol, "eol", "crlf", "发送时追加的行尾: crlf/lf/none")
	addSerialFlags(fs, &sf, true)
	_ = fs.Parse(args)
	if sf.port == "" {
		return fmt.Errorf("monitor 模式必须用 -port 指定串口（先用 rkttl ports 查看）")
	}

	logger := newLogger(sf.verbose)
	h := hub.New(sf.bufSize, sf.reopen, logger, sf.verbose)
	cfg := sf.config()
	h.SetDefault(cfg)
	if err := h.Open(cfg); err != nil {
		return err
	}

	var eolBytes []byte
	switch strings.ToLower(eol) {
	case "crlf":
		eolBytes = []byte("\r\n")
	case "lf":
		eolBytes = []byte("\n")
	case "none", "":
		eolBytes = nil
	default:
		return fmt.Errorf("无效 -eol %q（支持 crlf/lf/none）", eol)
	}

	done := make(chan struct{})
	go func() {
		<-signalCh()
		log.Printf("收到退出信号，正在关闭...")
		h.Stop()
		close(done)
	}()
	go func() {
		rd := bufio.NewReader(os.Stdin)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				if eolBytes != nil {
					// ReadBytes 保留了结尾的 \n；替换为所选行尾
					line = append(trimEOL(line), eolBytes...)
				}
				if _, werr := h.Write(line); werr != nil {
					log.Printf("写入串口失败: %v", werr)
				}
			}
			if err != nil {
				log.Printf("stdin 已结束（%v），仅保留接收", err)
				return
			}
		}
	}()

	fmt.Printf("rkttl monitor: 已连接 %s @ %d（Ctrl+C 退出）\r\n", cfg.Port, cfg.Baud)
	ch := h.Subscribe()
	defer h.Unsubscribe(ch)
	for {
		select {
		case <-done:
			return nil
		case ev := <-ch:
			switch ev.Type {
			case "data":
				os.Stdout.Write(ev.Data)
			case "closed":
				fmt.Printf("\r\n[rkttl] 串口断开: %s\r\n", ev.Info)
			case "opened":
				fmt.Printf("\r\n[rkttl] 串口已重新打开: %s\r\n", ev.Info)
			}
		}
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
