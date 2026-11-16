//go:build windows

package main

import (
	"github.com/lxn/walk"

	. "github.com/lxn/walk/declarative"
)

// ---- 终端主题（GUI 应用） ----
//
// 主题作用于终端区与操作日志区（背景 + 文字色）；窗口框架保持
// 系统原生风格。所选主题持久化到 exe 同目录 rkttl-gui.json。

// themeMenuItems 生成"视图"菜单的主题项（可勾选，单选语义）。
func themeMenuItems(a *app) []MenuItem {
	a.themeActions = make([]*walk.Action, len(themes))
	items := make([]MenuItem, 0, len(themes))
	for i := range themes {
		name := themes[i].Name
		items = append(items, Action{
			Text:      name,
			Checkable: true,
			AssignTo:  &a.themeActions[i],
			OnTriggered: func() {
				a.applyTheme(name)
			},
		})
	}
	return items
}

// applyTheme 切换终端/操作日志区配色并持久化。
// 启动时的首次应用（curTheme 为空）不打印提示。
func (a *app) applyTheme(name string) {
	t, ok := lookupTheme(name)
	if !ok {
		t, name = themes[0], themes[0].Name
	}
	prev := a.curTheme
	a.curTheme = name

	// 菜单打勾（单选语义）
	for i, act := range a.themeActions {
		if act != nil {
			_ = act.SetChecked(themes[i].Name == name)
		}
	}

	// 终端区与操作日志区共用同一把背景刷
	if a.logView == nil {
		return
	}
	brush, err := walk.NewSolidColorBrush(walk.Color(t.Bg))
	if err != nil {
		return
	}
	a.logView.SetBackground(brush)
	a.logView.SetTextColor(walk.Color(t.Fg))
	a.logView.Invalidate()
	if a.opView != nil {
		a.opView.SetBackground(brush)
		a.opView.SetTextColor(walk.Color(t.Fg))
		a.opView.Invalidate()
	}
	// 新刷已生效，释放上一把（不再被任何控件引用）
	if a.themeBrush != nil {
		a.themeBrush.Dispose()
	}
	a.themeBrush = brush

	saveThemeConfig(name)
	if a.stTheme != nil {
		_ = a.stTheme.SetText("主题: " + name)
	}
	if prev != "" && prev != name {
		a.appendSys("终端主题已切换: " + name + "\n")
	}
}
