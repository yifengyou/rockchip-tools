// Package oplog 提供带时间戳的操作日志（记录"谁做了什么"），
// 供 GUI 的操作日志面板与各组件共享：线程安全、容量受限、变更可订阅。
package oplog

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Logger 是操作日志容器。
type Logger struct {
	mu      sync.Mutex
	entries []string
	max     int
	notify  chan struct{} // 每次 Log 后 close 并重建（广播）
}

// New 创建容量为 max 条的操作日志（超出后丢弃最旧）。
func New(max int) *Logger {
	if max < 50 {
		max = 50
	}
	return &Logger{max: max, notify: make(chan struct{})}
}

// Log 追加一条操作记录（自动加时间戳）。
func (l *Logger) Log(format string, args ...any) {
	entry := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
	n := l.notify
	l.notify = make(chan struct{})
	l.mu.Unlock()
	close(n)
}

// Text 返回全部日志（按行连接，不含结尾换行）。
func (l *Logger) Text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.entries, "\n")
}

// Count 返回当前条数。
func (l *Logger) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Changed 返回变更信号通道：每次 Log 后旧通道被关闭。
// 订阅者持有旧通道即可被唤醒，随后应重新调用 Changed 获取新通道。
func (l *Logger) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.notify
}
