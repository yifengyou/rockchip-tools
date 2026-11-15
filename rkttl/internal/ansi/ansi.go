// Package ansi 提供终端 ANSI/VT100 转义序列的清理，
// 用于在不支持转义序列的显示控件（如 Windows EDIT 控件）中呈现串口输出。
package ansi

import "strings"

// Strip 移除字符串中的 ANSI 转义序列与不可打印控制字符，保留 \r \n \t。
// 退格(\b)按"删除前一个字符"的语义近似处理。
func Strip(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == 0x1b: // ESC
			i += escLen(s[i:])
		case c == '\r', c == '\n', c == '\t':
			b.WriteByte(c)
			i++
		case c == '\b':
			// 退格：从已输出内容中删掉最后一个字符（简化处理）
			str := b.String()
			if str != "" {
				// 按 rune 删除，避免把多字节字符截断
				r := []rune(str)
				b.Reset()
				b.WriteString(string(r[:len(r)-1]))
			}
			i++
		case c < 0x20 || c == 0x7f:
			i++ // 其他 C0 控制字符与 DEL
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// escLen 返回以 ESC 开头的转义序列的长度；无法识别时按 2 处理（ESC + 下一字节）。
func escLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[': // CSI: ESC [ 参数... 终止字节(0x40-0x7E)
		j := 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		if j < len(s) {
			j++ // 包含终止字节
		}
		return j
	case ']': // OSC: ESC ] ... (BEL | ESC \)
		j := 2
		for j < len(s) {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return j
	default: // 其他两字节转义（ESC c、ESC M 等）
		return 2
	}
}
