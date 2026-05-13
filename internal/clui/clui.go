// Package clui 是 dsipper 的 CLI 视觉层 — 阿屠宰风(awang style):
//
//   钉蓝 #1677FF  主色 / 标题 / box 框线
//   苹果绿 #34C759 success / accent / 进度条
//   主红 #C00000  fail / error
//   slate / 灰    次要文本 / 暗淡
//
// 全 ANSI truecolor;输出非 tty(管道 / 重定向)时自动降级为无色纯文本。
// 不引入外部依赖(termenv / lipgloss 都不用),保持 dsipper binary self-contained。
package clui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ====== ANSI 色 ======

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
	dim   = "\x1b[2m"
)

// 阿屠宰色板(truecolor RGB)
const (
	cBlue    = "\x1b[38;2;22;119;255m"  // #1677FF 钉蓝
	cBlueD   = "\x1b[38;2;15;79;184m"   // #0F4FB8 钉蓝深
	cGreen   = "\x1b[38;2;52;199;89m"   // #34C759 苹果绿
	cRed     = "\x1b[38;2;192;0;0m"     // #C00000 失败
	cYellow  = "\x1b[38;2;255;203;102m" // #FFCB66 警告
	cSlate   = "\x1b[38;2;136;136;136m" // #888 次要
	cSlateD  = "\x1b[38;2;90;90;90m"    // #5a5a5a 暗
	cWhite   = "\x1b[38;2;240;240;240m"
)

// enabled 决定 ANSI 是否生效。包初始化时按 stderr 是否 TTY 判断。
// 强制开:DSIPPER_FORCE_COLOR=1;强制关:NO_COLOR / DSIPPER_NO_COLOR。
var enabled = func() bool {
	if v := os.Getenv("DSIPPER_FORCE_COLOR"); v != "" {
		return true
	}
	if v := os.Getenv("NO_COLOR"); v != "" {
		return false
	}
	if v := os.Getenv("DSIPPER_NO_COLOR"); v != "" {
		return false
	}
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}()

// SetEnabled 测试 / 外部强制开关。
func SetEnabled(b bool) { enabled = b }

// wrap 给文本套色。non-TTY 直接返原文本。
func wrap(c, s string) string {
	if !enabled {
		return s
	}
	return c + s + reset
}

// Blue / Green / Red / ... 着色 helpers
func Blue(s string) string   { return wrap(cBlue, s) }
func BlueD(s string) string  { return wrap(cBlueD, s) }
func Green(s string) string  { return wrap(cGreen, s) }
func Red(s string) string    { return wrap(cRed, s) }
func Yellow(s string) string { return wrap(cYellow, s) }
func Slate(s string) string  { return wrap(cSlate, s) }
func SlateD(s string) string { return wrap(cSlateD, s) }
func Bold(s string) string {
	if !enabled {
		return s
	}
	return bold + s + reset
}
func Dim(s string) string {
	if !enabled {
		return s
	}
	return dim + s + reset
}

// ====== logo ======

// logo 5 行 ASCII art,横向两色渐变(钉蓝 → 苹果绿),底下一条色带分隔。
// 仅在 banner 顶部打印一次。
var logoLines = []string{
	`     _      _                       `,
	`  __| |___(_)_ __  _ __   ___ _ __  `,
	` / _' / __| | '_ \| '_ \ / _ \ '__| `,
	`| (_| \__ \ | |_) | |_) |  __/ |    `,
	` \__,_|___/_| .__/| .__/ \___|_|    `,
	`            |_|   |_|               `,
}

// Logo 返回着色后的 logo + 当前 version + 副标(如 "invite / listen / ...")。
func Logo(version, subcmd string) string {
	if !enabled {
		var b strings.Builder
		for _, l := range logoLines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, " dsipper %s — %s\n", version, subcmd)
		return b.String()
	}
	var b strings.Builder
	// 1-2 行钉蓝深,3-4 行钉蓝,5-6 行钉蓝→苹果绿过渡(整行同色但渐浅)
	colors := []string{cBlueD, cBlueD, cBlue, cBlue, cGreen, cGreen}
	for i, l := range logoLines {
		b.WriteString(colors[i] + l + reset + "\n")
	}
	// 横线
	b.WriteString(cGreen + "  ▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇▇" + reset + "\n")
	fmt.Fprintf(&b, "  %s %s %s %s\n",
		Bold(Blue("dsipper")),
		Slate(version),
		Slate("·"),
		Green(subcmd))
	return b.String()
}

// ====== banner box ======

// Banner 用阿屠宰风的圆角 unicode box 渲染一组 key/value 配置摘要。
// keys 列表保持 caller 给的顺序;k 左对齐到统一宽度。
type KV struct{ K, V string }

func BannerBox(title string, kvs []KV) string {
	// 计算宽度
	kw := 0
	for _, kv := range kvs {
		if l := len(kv.K); l > kw {
			kw = l
		}
	}
	rows := make([]string, 0, len(kvs)+2)
	for _, kv := range kvs {
		rows = append(rows, fmt.Sprintf("%s  %s",
			Slate(padRight(kv.K, kw)),
			kv.V))
	}
	// 内容最宽多少
	maxW := visibleLen(title) + 4
	for _, r := range rows {
		if l := visibleLen(r); l+2 > maxW {
			maxW = l + 2
		}
	}
	if maxW < 50 {
		maxW = 50
	}
	titleBar := fmt.Sprintf("─ %s ", Bold(Blue(title)))
	titleBarW := visibleLen(titleBar)
	pad := maxW - titleBarW
	if pad < 0 {
		pad = 0
	}
	var b strings.Builder
	// top
	b.WriteString(Blue("╭"))
	b.WriteString(Blue("─"))
	b.WriteString(titleBar)
	b.WriteString(Blue(strings.Repeat("─", pad)))
	b.WriteString(Blue("╮"))
	b.WriteString("\n")
	// rows
	for _, r := range rows {
		filler := maxW - visibleLen(r) - 1
		if filler < 0 {
			filler = 0
		}
		b.WriteString(Blue("│ "))
		b.WriteString(r)
		b.WriteString(strings.Repeat(" ", filler))
		b.WriteString(Blue("│"))
		b.WriteString("\n")
	}
	// bottom
	b.WriteString(Blue("╰"))
	b.WriteString(Blue(strings.Repeat("─", maxW+1)))
	b.WriteString(Blue("╯"))
	b.WriteString("\n")
	return b.String()
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// visibleLen 计算去掉 ANSI escape 后的可见字符数(粗略,按 byte;ASCII 场景准)。
func visibleLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		// 中文 / 框线字符按宽 2 算
		if r > 0x7f {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// ====== status print ======

// Step 是带状态符号的单行打印(给短启动消息用,如 "✓ TLS connected")。
// kind: "ok" / "fail" / "info" / "warn"
func Step(kind, msg string) string {
	switch kind {
	case "ok":
		return Green("✓ ") + msg
	case "fail":
		return Red("✗ ") + msg
	case "warn":
		return Yellow("! ") + msg
	default:
		return Blue("● ") + msg
	}
}

// ====== LiveStats:listen 模式实时统计行 ======

// Snapshot 是给 LiveStats 渲染的当前数值快照。caller 自己负责并发安全采样。
type Snapshot struct {
	Total   int64
	OK      int64
	Fail    int64
	Pending int64 // 当前活跃通(尚未拿到 final)
	CPS     float64 // 最近 1s 新增 INVITE / s
}

// LiveStats 在 stderr 同一行循环重写,给 listen 长跑时一眼看到吞吐。
// 启动:Start();停止:Stop()。
// caller 每秒提供 snapshot(回调式)。
type LiveStats struct {
	getSnap func() Snapshot
	stop    chan struct{}
	w       io.Writer
	running atomic.Bool
}

// NewLiveStats 创建 stats 渲染器;w 一般传 os.Stderr。
func NewLiveStats(w io.Writer, snap func() Snapshot) *LiveStats {
	return &LiveStats{w: w, getSnap: snap, stop: make(chan struct{})}
}

// Start 后台 goroutine 每秒重绘一行。
func (s *LiveStats) Start() {
	if !enabled {
		// 非 tty 不刷新(避免 \r 乱码进 log 文件)
		return
	}
	if !s.running.CompareAndSwap(false, true) {
		return
	}
	go s.loop()
}

// Stop 停止刷新并清行。
func (s *LiveStats) Stop() {
	if !s.running.CompareAndSwap(true, false) {
		return
	}
	close(s.stop)
	// 清当前行并下移光标,避免最后一帧停在跑出的下一条 log 上面
	fmt.Fprint(s.w, "\r\x1b[2K")
}

func (s *LiveStats) loop() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	startT := time.Now()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			snap := s.getSnap()
			uptime := time.Since(startT).Round(time.Second)
			line := renderStatsLine(snap, uptime)
			fmt.Fprint(s.w, "\r\x1b[2K"+line)
		}
	}
}

// ====== LivePanel — 多行实时面板,支持 in-place ANSI 重绘 ======

// LivePanel 在 stderr 上画一个 BannerBox 风格的多行卡片,每 interval 重绘一次。
// caller 提供 getRows 回调,返回当帧要画的 KV 行。stop 时清空回到上一行末尾,
// 主进程后续 println 不会跟面板撞。
//
// 非 tty(管道 / 重定向)时 Start() 直接 return,不污染 log 文件。
//
// 工程注意:caller 别在 panel 运行期间往 stderr 直接打 log(会撞行);
// 用 slog 文件 sink 或 stdout 输出都安全。
type LivePanel struct {
	w        io.Writer
	title    string
	getRows  func() []KV
	interval time.Duration

	stop      chan struct{}
	running   atomic.Bool
	lastLines int
}

// NewLivePanel 创建面板。w 一般 os.Stderr;title 顶部标题;getRows 帧回调;interval 刷新周期。
func NewLivePanel(w io.Writer, title string, interval time.Duration, getRows func() []KV) *LivePanel {
	return &LivePanel{
		w:        w,
		title:    title,
		getRows:  getRows,
		interval: interval,
		stop:     make(chan struct{}),
	}
}

// Start 启动后台刷新 goroutine。非 tty 时立刻 return。
func (p *LivePanel) Start() {
	if !enabled {
		return
	}
	if !p.running.CompareAndSwap(false, true) {
		return
	}
	fmt.Fprint(p.w, "\x1b[?25l") // hide cursor
	go p.loop()
}

// Stop 停止刷新,清空已画区域,恢复 cursor。可重复调用。
func (p *LivePanel) Stop() {
	if !p.running.CompareAndSwap(true, false) {
		return
	}
	close(p.stop)
	// 清掉已画的行(回到顶部 + 清屏到末尾)+ 恢复 cursor
	if p.lastLines > 0 {
		fmt.Fprintf(p.w, "\x1b[%dF\x1b[J", p.lastLines)
	}
	fmt.Fprint(p.w, "\x1b[?25h")
}

func (p *LivePanel) loop() {
	// 立刻画一帧避免 1s 空白
	p.render()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.render()
		}
	}
}

func (p *LivePanel) render() {
	rows := p.getRows()
	box := BannerBox(p.title, rows)
	// 计算行数(BannerBox 每行结尾有 \n)
	lines := strings.Count(box, "\n")
	// 上移到面板顶,清屏到末尾
	if p.lastLines > 0 {
		fmt.Fprintf(p.w, "\x1b[%dF\x1b[J", p.lastLines)
	}
	fmt.Fprint(p.w, box)
	p.lastLines = lines
}

// ====== ProgressBar ======

// ProgressBar 渲染一条 unicode 进度条。total<=0 时画全 dim 占位条。
// cur 自动 clamp 到 [0, total]。返回带色字符串,无尾换行。
func ProgressBar(cur, total, width int) string {
	if width <= 0 {
		width = 20
	}
	if total <= 0 {
		return Slate(strings.Repeat("░", width))
	}
	if cur < 0 {
		cur = 0
	}
	if cur > total {
		cur = total
	}
	filled := cur * width / total
	if filled > width {
		filled = width
	}
	return Green(strings.Repeat("█", filled)) + Slate(strings.Repeat("░", width-filled))
}

func renderStatsLine(snap Snapshot, uptime time.Duration) string {
	cps := ""
	if snap.CPS > 0 {
		cps = fmt.Sprintf("  cps %s", Bold(Yellow(fmt.Sprintf("%.0f", snap.CPS))))
	}
	return fmt.Sprintf("  %s  total %s  ok %s %s  fail %s %s  active %s %s%s  uptime %s",
		Green("●"),
		Bold(Blue(fmt.Sprintf("%d", snap.Total))),
		Bold(Green(fmt.Sprintf("%d", snap.OK))), Green("✓"),
		Bold(Red(fmt.Sprintf("%d", snap.Fail))), Red("✗"),
		Bold(Yellow(fmt.Sprintf("%d", snap.Pending))), Yellow("⟳"),
		cps,
		Slate(uptime.String()),
	)
}
