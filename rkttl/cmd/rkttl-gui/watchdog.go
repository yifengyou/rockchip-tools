//go:build windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lxn/walk"
)

// ---- UI 看门狗 ----
//
// UI 线程通过 statusLoop 每秒递增心跳；看门狗 goroutine 监测心跳，
// 连续 6 秒无心跳（UI 假死）时：
//  1. 把全部 goroutine 堆栈 + 最近操作日志 + 状态快照转储到
//     exe 同目录 rkttl-gui-stack.log；
//  2. 从后台线程弹窗告知用户（UI 线程已死，无法在窗口内提示），
//     用户确认后进程安全退出，便于立即重启。
//
// 堆栈文件即是卡死现场，发回开发者即可精确定位根因。

var heartbeat atomic.Int64

// startWatchdog 启动 UI 心跳看门狗。
func (a *app) startWatchdog() {
	go func() {
		const interval = 2 * time.Second
		const threshold = 3 // 3 × 2s = 6s 无心跳判定 UI 假死
		last := int64(-1)
		stalled := 0
		reported := false
		for {
			time.Sleep(interval)
			select {
			case <-a.doneCh:
				return
			default:
			}
			if !a.uiAlive.Load() {
				return
			}
			cur := heartbeat.Load()
			if cur == last {
				stalled++
				if stalled >= threshold && !reported {
					reported = true
					path := a.dumpAllStacks("UI 线程无响应 6 秒（reset/回车等操作后假死）")
					// UI 线程已死：从本 goroutine 弹系统消息框（独立于 UI 消息循环）
					msg := "rkttl 界面已无响应（可能已假死）。\n\n" +
						"堆栈现场已保存到：\n" + path +
						"\n\n请把该文件发给开发者以定位根因。\n点击确定后程序将退出，可重新启动。"
					_ = walk.MsgBox(nil, "rkttl 界面无响应", msg, walk.MsgBoxIconError)
					os.Exit(1)
				}
			} else {
				stalled = 0
				last = cur
			}
		}
	}()
}

// dumpAllStacks 把全部 goroutine 堆栈 + 诊断信息写入 rkttl-gui-stack.log，
// 返回文件路径。
func (a *app) dumpAllStacks(reason string) string {
	p, err := configPathFn()
	if err != nil {
		return "(未知目录)"
	}
	path := filepath.Join(filepath.Dir(p), "rkttl-gui-stack.log")

	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)

	header := "=== rkttl-gui 卡死现场转储 ===\n原因: " + reason +
		"\n时间: " + time.Now().Format("2006-01-02 15:04:05") +
		"\n诊断开关: 禁用全屏快照=" + boolMarkStr(vtDisableSnapshot.Load()) +
		"\n\n--- 串口/网络状态 ---\n"
	st := a.hub.Status()
	header += "串口打开=" + boolMarkStr(st.Open) + " 端口=" + st.Port +
		" RX=" + itoaU64(st.Received) + " TX=" + itoaU64(st.Sent) + "\n" +
		"快照模式=" + boolMarkStr(a.snapMode) +
		" 锚点16=" + itoaU64(uint64(a.inputAnchor16)) +
		" pendingClean16=" + itoaU64(uint64(a.pendingClean16)) + "\n\n" +
		"--- 最近操作日志 ---\n" + a.oplog.Text() + "\n\n" +
		"--- 全部 goroutine 堆栈 ---\n"
	_ = os.WriteFile(path, append([]byte(header), buf[:n]...), 0o644)
	return path
}

func boolMarkStr(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func itoaU64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

var _ = syscall.UTF16PtrFromString // 保留引用（Windows 构建提示一致性）
