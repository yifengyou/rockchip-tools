// Package vt 实现 VT100/xterm 的一个子集终端模拟器（屏幕网格 + 光标
// 寻址），用于正确呈现 htop/top 这类全屏光标寻址程序的输出。
//
// 支持的能力（覆盖 busybox top / htop / 常见 shell 交互）：
//   - 控制字符：\n（换行+滚动）\r（回车）\b（光标左移）\t（制表）
//   - CSI：CUP/HVP 光标定位、CUU/CUD/CUF/CUB/CNL/CPL/CHA/VPA 光标移动、
//     ED/EL 清屏清行、IL/DL 行插入删除、SU/SD 滚动、DECSTBM 滚动区域、
//     SGR 颜色（解析但本实现不存储属性）、光标显隐、
//     alt screen 切换（?1049h/l、?47h/l、?1047h/l）、光标保存/恢复
//   - ESC：7/8（DECSC/DECRC）、D/E、M（RI）、c
//   - OSC/DCS：忽略
//
// 不完整序列跨 Feed 分块缓冲，保证任意分块输入与整块输入状态一致。
package vt

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Screen 是终端屏幕模拟器。主屏（primary）行数可增长并保留历史，
// 备用屏（alternate）为固定网格；htop 等通过 ?1049h 切换到备用屏。
type Screen struct {
	Cols, Rows int

	lines [][]rune // 主屏（含历史行）
	alt   [][]rune // 备用屏（固定 Rows 行）

	altActive bool
	cx, cy    int    // 光标列/行（主屏 cy 为绝对行索引）
	acx, acy  int    // 备用屏光标
	saved     cursor // DECSC 保存的光标
	top, bot  int    // 滚动区域（1 基；0 表示未设置=全屏）
	wrap      bool   // DECAWM 自动折行（默认开）
	autowrap  pendingWrap

	// 全屏应用检测：记录最近的光标寻址/清屏序列时间戳，
	// 密集出现（2.5s 内 ≥4 次；全屏程序每帧至少 2 次）才判定为
	// 全屏 —— 普通 shell 的行编辑、单次 clear 命令不会误触发。
	marks []time.Time

	lastOut time.Time

	// 解析器状态
	esc     string // 缓冲中的已解码转义序列 rune（跨 Feed）
	partial []byte // 不完整的 UTF-8 多字节尾部（跨 Feed）
	inOSC   bool
}

type cursor struct{ x, y int }

// NewScreen 创建 cols×rows 的屏幕（典型串口终端为 80×24）。
func NewScreen(cols, rows int) *Screen {
	if cols < 20 {
		cols = 80
	}
	if rows < 4 {
		rows = 24
	}
	s := &Screen{Cols: cols, Rows: rows, wrap: true}
	s.lines = [][]rune{{}}
	s.alt = make([][]rune, rows)
	for i := range s.alt {
		s.alt[i] = []rune{}
	}
	return s
}

// Feed 输入一段文本（假定合法 UTF-8；便利入口）。
func (s *Screen) Feed(text string) {
	s.FeedBytes([]byte(text))
}

// FeedBytes 输入原始字节流：数据块可为任意字节边界（如网络分包），
// 不完整的 UTF-8 多字节序列会被缓冲到下一块，保证任意分块与
// 整块输入结果一致。
func (s *Screen) FeedBytes(p []byte) {
	buf := s.partial
	buf = append(buf, p...)
	s.partial = nil

	var text []rune
	for len(buf) > 0 {
		r, size := utf8.DecodeRune(buf)
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(buf) {
				// 不完整的多字节序列尾部：缓冲等待下一块
				s.partial = buf
				break
			}
			text = append(text, 0xFFFD) // 真无效字节
			buf = buf[1:]
		} else {
			text = append(text, r)
			buf = buf[size:]
		}
	}
	s.feedRunes(text)
}

// feedRunes 处理已解码的一批 rune。
func (s *Screen) feedRunes(rs []rune) {
	s.lastOut = time.Now()
	if s.esc != "" {
		rs = append([]rune(s.esc), rs...)
		s.esc = ""
	}
	i := 0
	for i < len(rs) {
		// 未决 OSC
		if s.inOSC {
			j := i
			for j < len(rs) {
				if rs[j] == 0x07 {
					i = j + 1
					s.inOSC = false
					break
				}
				if rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\' {
					i = j + 2
					s.inOSC = false
					break
				}
				j++
			}
			if s.inOSC { // 未终结：全部缓冲
				s.esc = string(rs[i:])
				return
			}
			continue
		}
		r := rs[i]
		if r == 0x1b {
			n, consumed := s.parseEscape(rs[i:])
			if consumed {
				i += n
			} else { // 不完整：缓冲等待下一块
				s.esc = string(rs[i:])
				return
			}
			continue
		}
		s.putRune(r)
		i++
	}
}

// putRune 写入一个可见字符（覆盖式），处理自动折行。
func (s *Screen) putRune(r rune) {
	if r < 0x20 || r == 0x7f {
		s.putCtrl(byte(r))
		return
	}
	if s.autowrap.pending && s.wrap {
		s.autowrap.pending = false
		s.newlineSoft()
	}
	line := s.curLine()
	for len(line) <= s.col() {
		line = append(line, ' ')
	}
	line[s.col()] = r
	nx := s.col() + 1
	if nx >= s.Cols {
		// DECAWM：光标停在行尾，写下一个字符时才折行（延迟换行）
		s.autowrap.pending = true
		s.setCol(s.Cols - 1)
	} else {
		s.setCol(nx)
	}
}

// putCtrl 处理到达 putRune 的控制字符（一般不会，防御）。
func (s *Screen) putCtrl(b byte) {
	switch b {
	case '\n':
		s.lineFeed()
	case '\r':
		s.setCol(0)
	case '\b':
		if s.col() > 0 {
			s.setCol(s.col() - 1)
		}
	case '\t':
		nx := (s.col()/8 + 1) * 8
		if nx >= s.Cols {
			nx = s.Cols - 1
		}
		s.setCol(nx)
	}
}

// curLine 返回光标所在行（按需扩展），基于当前屏（主/备）。
func (s *Screen) curLine() []rune {
	if s.altActive {
		for len(s.alt[s.acy]) <= s.acx {
			s.alt[s.acy] = append(s.alt[s.acy], ' ')
		}
		return s.alt[s.acy]
	}
	for s.cy >= len(s.lines) {
		s.lines = append(s.lines, []rune{})
	}
	for len(s.lines[s.cy]) <= s.cx {
		s.lines[s.cy] = append(s.lines[s.cy], ' ')
	}
	return s.lines[s.cy]
}

func (s *Screen) col() int {
	if s.altActive {
		return s.acx
	}
	return s.cx
}

func (s *Screen) setCol(x int) {
	if x < 0 {
		x = 0
	}
	if x >= s.Cols {
		x = s.Cols - 1
	}
	if s.altActive {
		s.acx = x
	} else {
		s.cx = x
	}
}

func (s *Screen) setRow(y int) {
	if s.altActive {
		if y < 0 {
			y = 0
		}
		if y >= s.Rows {
			y = s.Rows - 1
		}
		s.acy = y
		return
	}
	// 主屏：y 为可视区相对行（CUP 语义），换算为绝对行
	top := s.viewTop()
	abs := top + y
	for abs >= len(s.lines) {
		s.lines = append(s.lines, []rune{})
	}
	if abs < 0 {
		abs = 0
	}
	s.cy = abs
}

// viewTop 返回主屏可视区首行的绝对索引。
func (s *Screen) viewTop() int {
	t := len(s.lines) - s.Rows
	if t < 0 {
		t = 0
	}
	return t
}

// newlineSoft 折行/换行：DECAWM 折行 = LF+CR，光标到下一行行首。
func (s *Screen) newlineSoft() {
	s.lineFeedRaw()
	s.setCol(0)
}

// lineFeed 处理 LF：滚区内下移；出底滚动。
func (s *Screen) lineFeed() {
	s.lineFeedRaw()
}

func (s *Screen) lineFeedRaw() {
	if s.altActive {
		if s.acy < s.scrollBottom() {
			s.acy++
		} else {
			s.scrollUp(1)
		}
		return
	}
	// 主屏：绝对行下移；行不足则追加（历史保留）
	s.cy++
	for s.cy >= len(s.lines) {
		s.lines = append(s.lines, []rune{})
	}
}

// scrollTop/scrollBottom 返回滚动区域边界（0 基行号）。
func (s *Screen) scrollTop() int {
	if s.top > 0 {
		return s.top - 1
	}
	return 0
}

func (s *Screen) scrollBottom() int {
	if s.bot > 0 {
		return s.bot - 1
	}
	return s.Rows - 1
}

// scrollUp 滚动区域内容上移 n 行（顶部行丢弃，底部补空行）。
func (s *Screen) scrollUp(n int) {
	if s.altActive {
		t, b := s.scrollTop(), s.scrollBottom()
		for i := 0; i < n; i++ {
			copy(s.alt[t:], s.alt[t+1:b+1])
			s.alt[b] = []rune{}
		}
		return
	}
	// 主屏滚区滚动：视口内上移（等价丢弃滚区顶行内容）
	t := s.viewTop() + s.scrollTop()
	b := s.viewTop() + s.scrollBottom()
	if t < 0 || b >= len(s.lines) {
		return
	}
	for i := 0; i < n; i++ {
		copy(s.lines[t:], s.lines[t+1:b+1])
		s.lines[b] = []rune{}
	}
}

// scrollDown 滚动区域内容下移 n 行。
func (s *Screen) scrollDown(n int) {
	if s.altActive {
		t, b := s.scrollTop(), s.scrollBottom()
		for i := 0; i < n; i++ {
			copy(s.alt[t+1:b+1], s.alt[t:b])
			s.alt[t] = []rune{}
		}
		return
	}
	t := s.viewTop() + s.scrollTop()
	b := s.viewTop() + s.scrollBottom()
	if t < 0 || b >= len(s.lines) {
		return
	}
	for i := 0; i < n; i++ {
		copy(s.lines[t+1:b+1], s.lines[t:b])
		s.lines[t] = []rune{}
	}
}

// eraseLine 按模式清除当前行：0=光标到行尾，1=行首到光标，2=整行。
func (s *Screen) eraseLine(mode int) {
	line := s.curLineRaw()
	c := s.col()
	switch mode {
	case 0:
		for i := c; i < len(line); i++ {
			line[i] = ' '
		}
	case 1:
		for i := 0; i <= c && i < len(line); i++ {
			line[i] = ' '
		}
	case 2:
		s.setCurLine([]rune{})
	}
}

// eraseDisplay 按模式清屏：0=光标到屏尾，1=屏首到光标，2=全屏。
func (s *Screen) eraseDisplay(mode int) {
	if s.altActive {
		switch mode {
		case 0:
			s.eraseLine(0)
			for y := s.acy + 1; y < s.Rows; y++ {
				s.alt[y] = []rune{}
			}
		case 1:
			for y := 0; y < s.acy; y++ {
				s.alt[y] = []rune{}
			}
			s.eraseLine(1)
		case 2, 3:
			for y := range s.alt {
				s.alt[y] = []rune{}
			}
		}
		return
	}
	top := s.viewTop()
	switch mode {
	case 0:
		s.eraseLine(0)
		for y := s.cy + 1; y < len(s.lines); y++ {
			s.lines[y] = []rune{}
		}
	case 1:
		for y := top; y < s.cy && y < len(s.lines); y++ {
			s.lines[y] = []rune{}
		}
		s.eraseLine(1)
	case 2, 3:
		for y := top; y < len(s.lines); y++ {
			s.lines[y] = []rune{}
		}
	}
}

// curLineRaw 返回当前行引用（不扩展长度），用于 erase。
func (s *Screen) curLineRaw() []rune {
	if s.altActive {
		return s.alt[s.acy]
	}
	if s.cy < len(s.lines) {
		return s.lines[s.cy]
	}
	return nil
}

func (s *Screen) setCurLine(l []rune) {
	if s.altActive {
		s.alt[s.acy] = l
	} else if s.cy < len(s.lines) {
		s.lines[s.cy] = l
	}
}

// ---- 转义序列解析 ----

// parseEscape 解析从 ESC 开始的序列。
// 返回 (消耗的 rune 数, 是否完整)；不完整时 consumed=false。
func (s *Screen) parseEscape(rs []rune) (int, bool) {
	if len(rs) < 2 {
		return 0, false
	}
	switch rs[1] {
	case '[': // CSI
		j := 2
		priv := ""
		if j < len(rs) && (rs[j] == '?' || rs[j] == '>' || rs[j] == '<' || rs[j] == '=') {
			priv = string(rs[j])
			j++
		}
		start := j
		for j < len(rs) {
			r := rs[j]
			if r >= 0x40 && r <= 0x7e {
				s.execCSI(priv, string(rs[start:j]), r)
				return j + 1, true
			}
			j++
		}
		return 0, false // 终止字节未到
	case ']': // OSC
		j := 2
		for j < len(rs) {
			if rs[j] == 0x07 {
				return j + 1, true
			}
			if rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 2, true
			}
			j++
		}
		s.inOSC = true // 交回 Feed 缓冲
		return 2, true
	case '7': // DECSC
		s.saveCursor()
		return 2, true
	case '8': // DECRC
		s.restoreCursor()
		return 2, true
	case 'D': // IND：下移（出底滚动）
		s.lineFeedRaw()
		return 2, true
	case 'E': // NEL
		s.lineFeedRaw()
		s.setCol(0)
		return 2, true
	case 'M': // RI：上移（出顶滚动）
		if s.altActive {
			if s.acy > s.scrollTop() {
				s.acy--
			} else {
				s.scrollDown(1)
			}
		}
		return 2, true
	case 'c': // RIS 全复位
		s.resetSoft()
		return 2, true
	default:
		return 2, true // 其他两字节转义：忽略
	}
}

func (s *Screen) saveCursor() {
	if s.altActive {
		s.saved = cursor{s.acx, s.acy}
	} else {
		s.saved = cursor{s.cx, s.cy}
	}
}

func (s *Screen) restoreCursor() {
	if s.altActive {
		s.acx, s.acy = s.saved.x, s.saved.y
	} else {
		s.cx, s.cy = s.saved.x, s.saved.y
	}
}

func (s *Screen) resetSoft() {
	for i := range s.alt {
		s.alt[i] = []rune{}
	}
	s.altActive = false
	s.cx, s.cy = 0, 0
	s.acx, s.acy = 0, 0
	s.top, s.bot = 0, 0
}

// execCSI 执行一条 CSI 序列。params 为分号前的原始参数串。
func (s *Screen) execCSI(priv, params string, final rune) {
	ps := splitParams(params)
	n1 := func(def int) int {
		if len(ps) > 0 && ps[0] != "" {
			return atoiSafe(ps[0], def)
		}
		return def
	}
	n2 := func(def int) int {
		if len(ps) > 1 && ps[1] != "" {
			return atoiSafe(ps[1], def)
		}
		return def
	}

	switch final {
	case 'H', 'f': // CUP/HVP
		s.markFullScreen()
		s.setCol(n2(1) - 1)
		s.setRow(n1(1) - 1)
	case 'A': // CUU
		if s.altActive {
			s.acy -= n1(1)
		} else {
			s.cy -= n1(1)
		}
		s.clampCursor()
	case 'B': // CUD
		if s.altActive {
			s.acy += n1(1)
		} else {
			s.cy += n1(1)
		}
		s.clampCursor()
	case 'C': // CUF
		s.setCol(s.col() + n1(1))
	case 'D': // CUB
		s.setCol(s.col() - n1(1))
	case 'E': // CNL
		if s.altActive {
			s.acy += n1(1)
		}
		s.setCol(0)
		s.clampCursor()
	case 'F': // CPL
		if s.altActive {
			s.acy -= n1(1)
		}
		s.setCol(0)
		s.clampCursor()
	case 'G', '`': // CHA/HPA
		s.setCol(n1(1) - 1)
	case 'd': // VPA
		if s.altActive {
			s.acy = n1(1) - 1
			s.clampCursor()
		}
	case 'J': // ED
		s.markFullScreen()
		s.eraseDisplay(n1(0))
	case 'K': // EL
		s.eraseLine(n1(0))
	case 'L': // IL
		s.markFullScreen()
		if s.altActive {
			s.scrollDownAt(n1(1))
		}
	case 'M': // DL
		s.markFullScreen()
		if s.altActive {
			s.scrollUpAt(n1(1))
		}
	case 'S': // SU
		s.markFullScreen()
		s.scrollUp(n1(1))
	case 'T': // SD
		s.markFullScreen()
		s.scrollDown(n1(1))
	case 'r': // DECSTBM
		if priv == "" {
			s.markFullScreen()
			s.top, s.bot = n1(1), n2(s.Rows)
			if s.altActive {
				s.acx, s.acy = 0, s.scrollTop()
			}
		}
	case 'm': // SGR（解析忽略：本实现不存属性）
		_ = ps
	case 'h', 'l':
		if priv == "?" {
			switch n1(0) {
			case 25: // 光标显隐：忽略
			case 47, 1047, 1049: // alt screen
				if final == 'h' && !s.altActive {
					s.markFullScreen()
					for i := range s.alt {
						s.alt[i] = []rune{}
					}
					s.altActive = true
					s.acx, s.acy = 0, 0
				} else if final == 'l' && s.altActive {
					s.markFullScreen()
					s.altActive = false
				}
			case 7: // DECAWM
				s.wrap = final == 'h'
			}
		}
	case 's': // 保存光标
		s.saveCursor()
	case 'u': // 恢复光标
		s.restoreCursor()
	}
}

// scrollUpAt/scrollDownAt：从光标行开始的插入/删除（IL/DL，仅备用屏实现）。
func (s *Screen) scrollUpAt(n int) {
	t, b := s.acy, s.scrollBottom()
	for i := 0; i < n; i++ {
		copy(s.alt[t:], s.alt[t+1:b+1])
		s.alt[b] = []rune{}
	}
}

func (s *Screen) scrollDownAt(n int) {
	t, b := s.acy, s.scrollBottom()
	for i := 0; i < n; i++ {
		copy(s.alt[t+1:b+1], s.alt[t:b])
		s.alt[t] = []rune{}
	}
}

func (s *Screen) clampCursor() {
	if s.altActive {
		if s.acy < 0 {
			s.acy = 0
		}
		if s.acy >= s.Rows {
			s.acy = s.Rows - 1
		}
	} else {
		if s.cy < 0 {
			s.cy = 0
		}
		for s.cy >= len(s.lines) {
			s.lines = append(s.lines, []rune{})
		}
	}
}

// markFullScreen 记录一次全屏程序特征序列（CUP/ED/滚区等），
// 保留最近 2.5 秒内的记录。
func (s *Screen) markFullScreen() {
	now := time.Now()
	s.marks = append(s.marks, now)
	cut := now.Add(-2500 * time.Millisecond)
	keep := s.marks[:0]
	for _, t := range s.marks {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	s.marks = keep
	if len(s.marks) > 16 {
		s.marks = s.marks[len(s.marks)-16:]
	}
}

// FullScreenActive 返回当前是否应按"全屏应用"呈现：
// 备用屏激活，或 2.5 秒内出现 ≥4 次特征序列（持续帧重绘）。
func (s *Screen) FullScreenActive() bool {
	if s.altActive {
		return true
	}
	cut := time.Now().Add(-2500 * time.Millisecond)
	n := 0
	for i := len(s.marks) - 1; i >= 0; i-- {
		if s.marks[i].Before(cut) {
			break
		}
		n++
	}
	return n >= 4
}

// AltActive 返回备用屏是否激活。
func (s *Screen) AltActive() bool { return s.altActive }

// Dump 返回当前呈现的文本（备用屏优先；主屏为可视区）。
// 行尾空白被裁剪，行以 \n 连接。
func (s *Screen) Dump() string {
	if s.altActive {
		var b strings.Builder
		for y, line := range s.alt {
			b.WriteString(strings.TrimRight(string(line), " "))
			if y != len(s.alt)-1 {
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	var b strings.Builder
	top := s.viewTop()
	end := len(s.lines)
	// 裁掉连续的尾部空行（光标占位行），保留至少一行
	for end > top+1 && len(strings.TrimRight(string(s.lines[end-1]), " \t")) == 0 {
		end--
	}
	for y := top; y < end; y++ {
		b.WriteString(strings.TrimRight(string(s.lines[y]), " "))
		if y != end-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ---- 参数辅助 ----

func splitParams(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ";")
}

func atoiSafe(s string, def int) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

type pendingWrap struct{ pending bool }
