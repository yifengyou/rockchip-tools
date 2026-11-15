package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestThemesAtLeastTen(t *testing.T) {
	if len(themes) < 10 {
		t.Fatalf("内置主题应至少 10 个，当前 %d 个", len(themes))
	}
	seen := map[string]bool{}
	for _, th := range themes {
		if th.Name == "" {
			t.Fatal("存在空主题名")
		}
		if seen[th.Name] {
			t.Fatalf("主题名重复: %s", th.Name)
		}
		seen[th.Name] = true
		if th.Bg == th.Fg {
			t.Fatalf("主题 %s 前景与背景相同（不可读）", th.Name)
		}
	}
}

func TestThemesColorLayout(t *testing.T) {
	// COLORREF 布局：低字节 R，中字节 G，高字节 B
	if got := rgb(0x11, 0x22, 0x33); got != 0x332211 {
		t.Fatalf("rgb 布局错误: %#x", got)
	}
	dracula, ok := lookupTheme("Dracula")
	if !ok {
		t.Fatal("缺少 Dracula 主题")
	}
	if dracula.Bg != rgb(0x28, 0x2A, 0x36) || dracula.Fg != rgb(0xF8, 0xF8, 0xF2) {
		t.Fatalf("Dracula 配色错误: Bg=%#x Fg=%#x", dracula.Bg, dracula.Fg)
	}
	// 亮色主题 sanity：纸白应为纯白底纯黑字
	paper, _ := lookupTheme("纸白")
	if paper.Bg != 0xFFFFFF || paper.Fg != 0x000000 {
		t.Fatalf("纸白配色错误: %#x / %#x", paper.Bg, paper.Fg)
	}
}

func TestLookupThemeUnknown(t *testing.T) {
	if _, ok := lookupTheme("不存在的主题"); ok {
		t.Fatal("未知主题不应命中")
	}
	if _, ok := lookupTheme(""); ok {
		t.Fatal("空名不应命中")
	}
}

func TestThemeConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configPathFn = func() (string, error) { return filepath.Join(dir, "rkttl-gui.json"), nil }
	defer func() {
		// 还原默认实现（避免影响其他测试）
		configPathFn = func() (string, error) {
			exe, err := os.Executable()
			if err != nil {
				return "", err
			}
			return filepath.Join(filepath.Dir(exe), "rkttl-gui.json"), nil
		}
	}()

	// 无配置文件 → 默认主题
	if got := loadThemeConfig(); got != defaultThemeName() {
		t.Fatalf("无配置应返回默认主题，得到 %q", got)
	}
	// 保存后加载 → 往返一致
	saveThemeConfig("Tokyo Night")
	if got := loadThemeConfig(); got != "Tokyo Night" {
		t.Fatalf("往返不一致: %q", got)
	}
	// 非法主题名 → 回退默认
	saveThemeConfig("hacker theme 1337")
	if got := loadThemeConfig(); got != defaultThemeName() {
		t.Fatalf("非法主题应回退默认，得到 %q", got)
	}
	// 损坏 JSON → 回退默认
	if err := os.WriteFile(filepath.Join(dir, "rkttl-gui.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadThemeConfig(); got != defaultThemeName() {
		t.Fatalf("损坏配置应回退默认，得到 %q", got)
	}
}
