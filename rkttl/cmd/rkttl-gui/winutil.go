//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/lxn/walk"
)

// setupConsole 把控制台输出代码页设为 UTF-8（携带 -H windowsgui 时无控制台，
// 该调用也无害），保证日志/panic 中的中文不乱码。
func setupConsole() {
	proc, err := syscall.MustLoadDLL("kernel32.dll").FindProc("SetConsoleOutputCP")
	if err == nil {
		_, _, _ = proc.Call(65001)
	}

}

// openBrowser 用默认浏览器打开 URL。
func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// fatalBox 以消息框展示致命错误（无控制台时仍可见）。
func fatalBox(msg string) {
	_ = walk.MsgBox(nil, "rkttl", msg, walk.MsgBoxIconError)
	fmt.Fprintln(os.Stderr, msg)
}
