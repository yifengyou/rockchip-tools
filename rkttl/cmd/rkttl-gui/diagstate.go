// diagstate 保存跨线程的诊断开关状态（无平台依赖）。
package main

import "sync/atomic"

// vtDisableSnapshot 是"禁用全屏快照模式"诊断开关：
// 用于排查 reset 等场景的卡死问题 —— 禁用后 htop/top 等全屏程序
// 退化为流式显示（内容会乱但不会卡），若因此不再卡死，
// 即可把根因锁定在全屏快照路径。
var vtDisableSnapshot atomic.Bool
