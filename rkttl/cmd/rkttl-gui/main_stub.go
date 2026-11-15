//go:build !windows

// 非 Windows 平台的占位入口：rkttl-gui 仅支持 Windows，
// 完整功能请使用 rkttl 命令行版。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("rkttl-gui 图形界面仅支持 Windows；请使用 rkttl 命令行版（serve/bridge/monitor）。")
	os.Exit(1)
}
