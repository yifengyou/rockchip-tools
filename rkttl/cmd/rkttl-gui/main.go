//go:build windows

// rkttl-gui 是 rkttl 的 Windows 图形界面版本：
// 在本地图形窗口中直连串口（输入/收发），同时把同一串口通过
// 网页控制台 / HTTP API / TCP 桥共享给远端 —— 本地与远端
// 同时收发，类似 tmux 的多端 attach。
package main

import (
	"fmt"
	"log"
	"os"
	"runtime"

	"rkttl/internal/version"
)

func main() {
	runtime.LockOSThread() // Win32 消息循环要求固定在主线程
	setupConsole()

	defer func() {
		if r := recover(); r != nil {
			fatalBox(fmt.Sprintf("rkttl-gui v%s 发生内部错误: %v", version.Version, r))
			os.Exit(1)
		}
	}()

	app, err := newApp()
	if err != nil {
		fatalBox("初始化失败: " + err.Error())
		os.Exit(1)
	}
	defer app.shutdown()

	// -H windowsgui 构建下没有控制台，把标准日志也导向 GUI 终端区。
	log.SetOutput(app.logWriter())
	log.SetFlags(log.LstdFlags)

	if err := app.run(); err != nil {
		fatalBox("运行失败: " + err.Error())
		os.Exit(1)
	}
}
