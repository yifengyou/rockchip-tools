package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"rkttl/internal/api"
)

// cmdPorts 列出本机串口（文本表格或 JSON）。
func cmdPorts(args []string) error {
	fs := flag.NewFlagSet("ports", flag.ExitOnError)
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "以 JSON 输出")
	_ = fs.Parse(args)

	ports, err := api.ListPorts()
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"ports": ports, "count": len(ports)})
	}
	if len(ports) == 0 {
		fmt.Println("未发现串口。")
		if isWindows() {
			fmt.Println("提示: Windows 下 USB 转 TTL（如 CH340）需先安装驱动（仓库内有 USB转串口驱动-CH340.zip / DriverAssitant）。")
		}
		return nil
	}
	fmt.Printf("%-16s %-6s %-12s %s\n", "PORT", "USB", "VID:PID", "PRODUCT")
	for _, p := range ports {
		usb := "-"
		vidpid := "-"
		if p.IsUSB {
			usb = "是"
			vidpid = p.VID + ":" + p.PID
		}
		fmt.Printf("%-16s %-6s %-12s %s\n", p.Name, usb, vidpid, p.Product)
	}
	fmt.Println()
	fmt.Println("提示: 用 rkttl serve -port <PORT> 启动共享，或 rkttl monitor -port <PORT> 本地直连。")
	return nil
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}
