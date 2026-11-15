package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 白盒测试会话日志：直接驱动内部文件生命周期（真实串口路径由 e2e 覆盖）。
func TestFileLoggingLifecycle(t *testing.T) {
	dir := t.TempDir()
	h := New(4096, false, nil, false)
	if err := h.EnableFileLogging(dir); err != nil {
		t.Fatal(err)
	}

	// 未打开串口时：手动开启会话文件并写入
	h.logMu.Lock()
	h.openLogFileLocked()
	h.logMu.Unlock()

	st := h.Status().Log
	if !st.Enabled || st.Path == "" {
		t.Fatalf("应已创建日志文件: %+v", st)
	}
	if filepath.Dir(st.Path) != dir {
		t.Fatalf("日志文件位置 %s 应在 %s 下", st.Path, dir)
	}

	h.writeLog([]byte("boot line 1\r\n"))
	h.writeLog([]byte("boot line 2\r\n"))

	// ReadLog 返回文件内容（不受环形缓冲限制）
	data, err := h.ReadLog()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "boot line 1\r\nboot line 2\r\n" {
		t.Fatalf("日志内容 = %q", data)
	}
	if st2 := h.Status().Log; st2.Written != 26 {
		t.Fatalf("written = %d, want 26", st2.Written)
	}

	// TruncateLog 清空后继续写
	if err := h.TruncateLog(); err != nil {
		t.Fatal(err)
	}
	h.writeLog([]byte("after clear\n"))
	data, _ = h.ReadLog()
	if string(data) != "after clear\n" {
		t.Fatalf("清空后日志 = %q", data)
	}

	// 关闭会话文件（手动关闭串口的语义），再开启 → 新文件
	firstPath := h.Status().Log.Path
	h.logMu.Lock()
	h.closeLogFileLocked()
	h.openLogFileLocked()
	h.logMu.Unlock()
	secondPath := h.Status().Log.Path
	if secondPath == firstPath {
		t.Fatal("重新打开会话应创建新文件")
	}
	data, _ = h.ReadLog()
	if len(data) != 0 {
		t.Fatalf("新文件应为空, got %q", data)
	}

	// DisableFileLogging 彻底停用
	h.DisableFileLogging()
	if st := h.Status().Log; st.Enabled {
		t.Fatal("Disable 后不应再启用")
	}
	h.writeLog([]byte("ignored"))
	if st := h.Status().Log; st.Written != 0 {
		t.Fatalf("停用后不应记录, written=%d", st.Written)
	}
}

// 未启用文件记录时 ReadLog 应回退到环形缓冲。
func TestReadLogFallsBackToBuffer(t *testing.T) {
	h := New(4096, false, nil, false)
	h.ring.Write([]byte("buffered bytes"))
	data, err := h.ReadLog()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "buffered bytes" {
		t.Fatalf("回退内容 = %q", data)
	}
}

// 日志文件名格式。
func TestLogFileName(t *testing.T) {
	h := New(4096, false, nil, false)
	name := h.LogFileName()
	if !strings.HasPrefix(name, "rkttl-serial-") || !strings.HasSuffix(name, ".log") {
		t.Fatalf("默认文件名格式错误: %q", name)
	}
	dir := t.TempDir()
	_ = h.EnableFileLogging(dir)
	h.logMu.Lock()
	h.openLogFileLocked()
	h.logMu.Unlock()
	got := h.LogFileName()
	if filepath.Base(h.Status().Log.Path) != got {
		t.Fatalf("应返回当前文件名: %q vs %q", got, h.Status().Log.Path)
	}
	// 文件确实在磁盘上
	if _, err := os.Stat(h.Status().Log.Path); err != nil {
		t.Fatal(err)
	}
}
