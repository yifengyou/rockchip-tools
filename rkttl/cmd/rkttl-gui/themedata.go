// themeData 定义终端主题与配置持久化（无平台依赖，可跨平台单测）。
//
// 颜色采用 Win32 COLORREF 布局 0x00BBGGRR（与 walk.Color/walk.RGB 一致），
// 供 GUI 侧直接转换使用。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// rgb 按 0x00BBGGRR（COLORREF）布局合成颜色，参数为 R/G/B 的 0-255 值。
func rgb(r, g, b uint32) uint32 { return r | g<<8 | b<<16 }

// theme 是一个终端配色。
type theme struct {
	Name string
	Bg   uint32 // 终端背景（COLORREF）
	Fg   uint32 // 终端文字（COLORREF）
}

// themes 内置主题（暗色为主，含两个亮色）。
var themes = []theme{
	{Name: "默认深色", Bg: rgb(0x1D, 0x1F, 0x21), Fg: rgb(0xC5, 0xC8, 0xC6)},
	{Name: "Dracula", Bg: rgb(0x28, 0x2A, 0x36), Fg: rgb(0xF8, 0xF8, 0xF2)},
	{Name: "One Dark", Bg: rgb(0x28, 0x2C, 0x34), Fg: rgb(0xAB, 0xB2, 0xBF)},
	{Name: "Nord", Bg: rgb(0x2E, 0x34, 0x40), Fg: rgb(0xD8, 0xDE, 0xE9)},
	{Name: "Monokai", Bg: rgb(0x27, 0x28, 0x22), Fg: rgb(0xF8, 0xF8, 0xF2)},
	{Name: "Gruvbox Dark", Bg: rgb(0x28, 0x28, 0x28), Fg: rgb(0xEB, 0xDB, 0xB2)},
	{Name: "Solarized Dark", Bg: rgb(0x00, 0x2B, 0x36), Fg: rgb(0x83, 0x94, 0x96)},
	{Name: "Tokyo Night", Bg: rgb(0x1A, 0x1B, 0x26), Fg: rgb(0xA9, 0xB1, 0xD6)},
	{Name: "Ubuntu 紫", Bg: rgb(0x30, 0x0A, 0x24), Fg: rgb(0xFF, 0xFF, 0xFF)},
	{Name: "Windows 终端", Bg: rgb(0x0C, 0x0C, 0x0C), Fg: rgb(0xCC, 0xCC, 0xCC)},
	{Name: "Solarized Light", Bg: rgb(0xFD, 0xF6, 0xE3), Fg: rgb(0x65, 0x7B, 0x83)},
	{Name: "纸白", Bg: rgb(0xFF, 0xFF, 0xFF), Fg: rgb(0x00, 0x00, 0x00)},
}

// defaultThemeName 返回默认主题名。
func defaultThemeName() string { return themes[0].Name }

func lookupTheme(name string) (theme, bool) {
	for _, t := range themes {
		if t.Name == name {
			return t, true
		}
	}
	return theme{}, false
}

// ---- 配置持久化（exe 同目录 rkttl-gui.json） ----

type guiConfig struct {
	Theme string `json:"theme"`
}

// configPathFn 可在测试中替换，默认指向 exe 同目录。
var configPathFn = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "rkttl-gui.json"), nil
}

func loadThemeConfig() string {
	p, err := configPathFn()
	if err != nil {
		return defaultThemeName()
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return defaultThemeName()
	}
	var cfg guiConfig
	if json.Unmarshal(data, &cfg) != nil || cfg.Theme == "" {
		return defaultThemeName()
	}
	if _, ok := lookupTheme(cfg.Theme); !ok {
		return defaultThemeName()
	}
	return cfg.Theme
}

func saveThemeConfig(name string) {
	p, err := configPathFn()
	if err != nil {
		return
	}
	data, _ := json.MarshalIndent(guiConfig{Theme: name}, "", "  ")
	_ = os.WriteFile(p, data, 0o644)
}
