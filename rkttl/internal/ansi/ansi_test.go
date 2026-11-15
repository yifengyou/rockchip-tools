package ansi

import "testing"

func TestStrip(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain text\r\n", "plain text\r\n"},
		{"\x1b[31mRED\x1b[0m plain", "RED plain"},
		{"\x1b[2J\x1b[Hclear", "clear"},                 // CSI J/H
		{"\x1b]0;title\x07after", "after"},              // OSC + BEL
		{"\x1b]0;title\x1b\\after", "after"},            // OSC + ST
		{"a\bb", "b"},                                   // 退格吃掉 a
		{"中文测试\x1b[1m加粗\x1b[0m", "中文测试加粗"},              // UTF-8 保留
		{"\x00\x07\x1fctrl", "ctrl"},                    // 控制字符移除
		{"incomplete \x1b", "incomplete "},              // 截断的 ESC
		{"\x1b[38;5;196mtruecolor\x1b[0m", "truecolor"}, // 256 色
		{"boot\x1b[2K\r\nlog", "boot\r\nlog"},           // \x1b[2K 移除，\r\n 保留
	}
	for _, c := range cases {
		if got := Strip(c.in); got != c.want {
			t.Errorf("Strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
