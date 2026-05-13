// Package report 收集 dsipper 一次跑里所有呼叫的 SIP 信令事件 + RTP 统计,
// 退出时落一份单文件 HTML report,每条呼叫可展开看 SVG 信令时序图。
//
// 设计取舍:
//   - Recorder 线程安全;UAC.logSIP / listen.go 主 loop / sendReply / sendUASBye 都喂同一个实例
//   - Call-ID 作为去重 key;首次见到自动建 Call,From/To/Transport/Local/Remote 字段就地填一次
//   - HTML 单文件:内嵌 CSS + server-side SVG,无外部依赖,scp 出去打开即可看
//   - <details>/<summary> 走原生 HTML 折叠,不依赖 JS
package report

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dsipper/internal/sipua"
)

// Event 一条 SIP 信令事件(TX / RX 一行)。
type Event struct {
	T        time.Time
	Dir      string // "TX" 本端发出 / "RX" 本端收到
	IsReq    bool
	Method   string
	Status   int
	Reason   string
	CSeq     string
	FromTag  string
	ToTag    string
	Peer     string // 对端 host:port 字符串,可为空
	First    string // 头部首行 "INVITE sip:..." 或 "200 OK"
}

// Call 一通呼叫的元数据 + 事件流。
type Call struct {
	CallID      string
	From        string
	To          string
	Transport   string
	Local       string
	Remote      string
	Events      []Event
	Start       time.Time
	End         time.Time
	FinalStatus int
	FinalReason string
	RTPTx       uint64
	RTPRx       uint64
	RTPTxBytes  uint64
	RTPRxBytes  uint64
	Note        string

	mu sync.Mutex
}

// Recorder 进程级单例(本工具一次跑只一个),收集所有 Call。
//
// 内存模型:活动通(尚未拿到 final)+ 失败通(>=300 / pending)全量留事件;
// 成功通(2xx final)收到 final 那一刻 drop events,只 inc 计数。这样
// long-running 跑几千万通成功呼叫内存仍是 O(并发活动通) ~ MB 量级。
type Recorder struct {
	mu      sync.Mutex
	calls   map[string]*Call
	order   []string // 按首次出现顺序(只含目前还在表里的)

	// KeepOnlyFailed=true 时,收到 INVITE 事务 2xx final 立即 drop events + 不留 Call。
	// 默认 true(成功通不展示)。
	KeepOnlyFailed bool

	// MaxFailedKept 失败通详情保留上限。达上限后新失败只 inc 计数,不再保留信令图。
	// 默认 50。<=0 表示不限。汇总区(顶部)始终用全量计数,跟此上限无关。
	MaxFailedKept int

	// 全量累计统计:按 final status code 分桶。0 桶 = INVITE 事务没拿到 final(pending)。
	statusCount   map[int]int64
	successByCode map[int]int64 // 仅 2xx,跟 statusCount 重复,单独留方便汇总
	failedKept    int           // 当前在 calls 表里的"失败已确认"通数(用于 MaxFailedKept 判断)
	failedDropped int64         // 因 MaxFailedKept 上限被 drop 的失败通数
	totalCalls    int64         // 见过的所有 Call-ID 总数(含已 drop 的成功 / 已 drop 的失败)

	// wall durations 用于 HTML report 直方图;cap=MaxWallSamples 防压测下无界增长
	// (达上限后丢弃新样本,直方图仍代表前 N 通的分布)。
	wallDurs       []time.Duration
	MaxWallSamples int

	// LogCtrl 可选钩子 — 收到 INVITE final 时通知日志层 flush(失败通)/ drop(成功通)缓存。
	// 配合 logsink.BufHandler 的 --log-only-failed 模式;为 nil 时全 noop。
	LogCtrl LogController

	Title   string
	Subcmd  string
	Started time.Time
}

// LogController 是 recorder ↔ 日志缓存层的通知接口。
// 实现见 internal/logsink.BufHandler。
type LogController interface {
	FlushCall(callID string)
	DropCall(callID string)
}

// Snapshot 是 recorder 当前 stats 的瞬时快照(给 CLI live stats 面板用)。
type Snapshot struct {
	Total   int64
	OK      int64
	Fail    int64
	Pending int64           // 见过但没拿到 final 的 callid 数(活跃通)
	Status  map[int]int64   // status code → count(完整分布快照,caller 可读)
}

// AddWallDuration 记录一次完整呼叫的 wall duration(INVITE 起到 BYE 完)。
// 用于 HTML report 直方图。达 MaxWallSamples 上限后新样本静默丢弃,直方图仍代表
// 前 N 通的分布(压测下 N=100K 足够代表)。
func (r *Recorder) AddWallDuration(d time.Duration) {
	if d <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.MaxWallSamples > 0 && len(r.wallDurs) >= r.MaxWallSamples {
		return
	}
	r.wallDurs = append(r.wallDurs, d)
}

// Snapshot 取当前统计,线程安全。Status map 是 defensive copy,caller 修改不回写。
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ok, fail int64
	status := make(map[int]int64, len(r.statusCount))
	for code, cnt := range r.statusCount {
		status[code] = cnt
		if code >= 200 && code < 300 {
			ok += cnt
		} else if code >= 300 {
			fail += cnt
		}
	}
	return Snapshot{
		Total:   r.totalCalls,
		OK:      ok,
		Fail:    fail,
		Pending: r.totalCalls - ok - fail,
		Status:  status,
	}
}

// New 创建 recorder。title 用于 HTML <title> 与顶部 banner。
func New(subcmd, title string) *Recorder {
	return &Recorder{
		calls:          map[string]*Call{},
		statusCount:    map[int]int64{},
		successByCode:  map[int]int64{},
		KeepOnlyFailed: true,
		MaxWallSamples: 100_000,
		MaxFailedKept:  50,
		Title:          title,
		Subcmd:         subcmd,
		Started:        time.Now(),
	}
}

// getOrCreate 返回 callID 对应的 Call;不存在则建。带 isNew 指示是否新建,用于 total 计数。
// 已经 drop 过的成功通(从 calls 移除)再次见到会被当成新 call 重新建。dsipper 场景里
// 同一 Call-ID drop 后基本不会再来事件,影响可忽略。
func (r *Recorder) getOrCreate(callID string) (*Call, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.calls[callID]; ok {
		return c, false
	}
	c := &Call{CallID: callID}
	r.calls[callID] = c
	r.order = append(r.order, callID)
	r.totalCalls++
	return c, true
}

// dropCall 把指定 Call-ID 从 calls map + order slice 移除,释放 events 内存。
func (r *Recorder) dropCall(callID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.calls, callID)
	for i, id := range r.order {
		if id == callID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// cseqMethod 抽 CSeq header 的方法部分,例如 "1 INVITE" → "INVITE"。
func cseqMethod(cseq string) string {
	for i := len(cseq) - 1; i >= 0; i-- {
		if cseq[i] == ' ' {
			return strings.TrimSpace(cseq[i+1:])
		}
	}
	return ""
}

// Record 喂入一条 SIP message 事件。dir = "TX" / "RX"。peer 可空(只是展示用)。
// 实现 sipua.Recorder 接口,可直接挂到 UAC.Recorder。
//
// 大规模场景关键路径:
//   - 收到 INVITE 事务的 2xx final + KeepOnlyFailed=true → 立刻 drop 整条 Call,
//     只在 successByCode + statusCount 里加 1。十万通成功呼叫内存增长 0。
//   - 失败 final(>=300)继续保留,后续 HTML 全量展示信令图。
//   - CANCEL / BYE 事务的 2xx 不影响 INVITE 事务的 final 判断。
func (r *Recorder) Record(dir string, m *sipua.Message, peer string) {
	if m == nil {
		return
	}
	callID := m.Headers.Get("Call-ID")
	if callID == "" {
		return
	}
	// 只有 INVITE 请求是"建仓"事件。其他事件(100/180/200/ACK/BYE/CANCEL...)
	// 若 callid 已经不在 calls 表里,说明对应 INVITE 已被 drop(成功通 / 失败溢出),
	// 后续 dialog 尾巴(ACK / BYE / 等)直接 noop,避免空白 call 重建 → totalCalls 多算。
	isInviteReq := m.IsRequest && m.Method == "INVITE"
	if !isInviteReq {
		r.mu.Lock()
		_, exists := r.calls[callID]
		r.mu.Unlock()
		if !exists {
			return
		}
	}
	c, _ := r.getOrCreate(callID)
	c.mu.Lock()

	ev := Event{
		T:       time.Now(),
		Dir:     dir,
		CSeq:    m.Headers.Get("CSeq"),
		FromTag: m.FromTag(),
		ToTag:   m.ToTag(),
		Peer:    peer,
	}
	if m.IsRequest {
		ev.IsReq = true
		ev.Method = m.Method
		ev.First = m.Method + " " + shortRURI(m.RURI)
		if c.From == "" {
			c.From = trimURI(m.Headers.Get("From"))
		}
		if c.To == "" {
			c.To = trimURI(m.Headers.Get("To"))
		}
	} else {
		ev.Status = m.StatusCode
		ev.Reason = m.ReasonPhrase
		ev.First = fmt.Sprintf("%d %s", m.StatusCode, m.ReasonPhrase)
	}
	c.Events = append(c.Events, ev)
	if c.Start.IsZero() {
		c.Start = ev.T
	}
	c.End = ev.T

	// 只把 INVITE 事务的 final response 当作"通话级 final"。
	// CANCEL / BYE / OPTIONS 等独立事务的 200 不算。
	isInviteFinal := !m.IsRequest && m.StatusCode >= 200 && cseqMethod(ev.CSeq) == "INVITE"
	if isInviteFinal {
		c.FinalStatus = m.StatusCode
		c.FinalReason = m.ReasonPhrase
	}
	c.mu.Unlock()

	// 拿到 INVITE final 的 3 种处理路径:
	//   2xx:汇总区 inc + 立刻 drop 详情(KeepOnlyFailed)
	//   ≥300 且 failedKept < 上限:汇总区 inc + failedKept++,保留信令图
	//   ≥300 且已到上限:汇总区 inc + failedDropped++ + drop 详情(只看前 N 个失败样本)
	if !isInviteFinal {
		return
	}
	switch {
	case m.StatusCode < 300:
		r.mu.Lock()
		r.statusCount[m.StatusCode]++
		r.successByCode[m.StatusCode]++
		r.mu.Unlock()
		if r.KeepOnlyFailed {
			r.dropCall(callID)
		}
		// 成功通:让日志层也丢弃缓存
		if r.LogCtrl != nil {
			r.LogCtrl.DropCall(callID)
		}
	default: // ≥300 失败
		r.mu.Lock()
		r.statusCount[m.StatusCode]++
		overLimit := r.MaxFailedKept > 0 && r.failedKept >= r.MaxFailedKept
		if overLimit {
			r.failedDropped++
		} else {
			r.failedKept++
		}
		r.mu.Unlock()
		if overLimit {
			r.dropCall(callID)
		}
		// 失败通:日志要真正落盘(只在保留详情的通才 flush;溢出 drop 的通日志也丢,
		// 跟 HTML 详情区一致 — 顶部汇总数字仍含它)
		if r.LogCtrl != nil {
			if overLimit {
				r.LogCtrl.DropCall(callID)
			} else {
				r.LogCtrl.FlushCall(callID)
			}
		}
	}
}

// existing 取已在 calls 表里的 Call;不存在返 nil(不会建)。
// 用于 SetMeta / SetRTP / Note 这些"更新元数据"动作 —— 已被 drop 的成功通不需要再建出来。
func (r *Recorder) existing(callID string) *Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[callID]
}

// SetMeta 设元数据(transport / local / remote),通常在 dial 后调一次。
// 空字符串字段不覆盖已有值。callid 不在表 → noop。
func (r *Recorder) SetMeta(callID, transport, local, remote string) {
	c := r.existing(callID)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if transport != "" {
		c.Transport = transport
	}
	if local != "" {
		c.Local = local
	}
	if remote != "" {
		c.Remote = remote
	}
}

// SetRTP 设 RTP 统计,通常呼叫结束 rtp.Stats() 后调用。
// callid 不在表(成功通已被 drop) → noop,统计只在失败 / 活动通上有用。
func (r *Recorder) SetRTP(callID string, tx, rx, txB, rxB uint64) {
	c := r.existing(callID)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RTPTx, c.RTPRx, c.RTPTxBytes, c.RTPRxBytes = tx, rx, txB, rxB
}

// Note 给一通呼叫追加一行备注(将渲染在元数据下)。callid 不在表 → noop。
func (r *Recorder) Note(callID, msg string) {
	c := r.existing(callID)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Note != "" {
		c.Note += "\n"
	}
	c.Note += msg
}

// SaveHTML 把当前所有 Call 落成一份 HTML report。
// path 可以是目录(自动拼 dsipper-report-<时间戳>.html)或具体文件名。
func (r *Recorder) SaveHTML(path string) (string, error) {
	r.mu.Lock()
	calls := make([]*Call, 0, len(r.order))
	for _, id := range r.order {
		calls = append(calls, r.calls[id])
	}
	r.mu.Unlock()

	// 排序:按 Start 时间升序,稳定顺序
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Start.Before(calls[j].Start) })

	// 路径处理:.html 结尾视作具体文件,其他都按目录(自动 mkdir + 时间戳文件名)。
	target := path
	asFile := strings.HasSuffix(strings.ToLower(target), ".html") ||
		strings.HasSuffix(strings.ToLower(target), ".htm")
	if !asFile {
		_ = os.MkdirAll(target, 0755)
		ts := time.Now().Format("20060102-150405")
		target = filepath.Join(target, fmt.Sprintf("dsipper-%s-%s.html", r.Subcmd, ts))
	} else if dir := filepath.Dir(target); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}

	view := buildView(r, calls)
	f, err := os.Create(target)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := htmlTmpl.Execute(f, view); err != nil {
		return "", err
	}
	return target, nil
}

// ===== view 模型 =====

type callView struct {
	Idx       int
	CallID    string
	CallIDS   string // 截断展示
	From      string
	To        string
	Transport string
	Local     string
	Remote    string
	StartStr  string
	Duration  string
	Status    string
	StatusCls string // ok / fail / pending
	RTPLine   string
	Note      string
	Ladder    template.HTML
	Events    []eventView
}

type eventView struct {
	TStr    string
	DeltaMs string
	Dir     string
	First   string
	CSeq    string
	FromTag string
	ToTag   string
}

type statRow struct {
	Code  int
	Reason string
	Count int64
	Cls   string // ok / fail / pending
}

type reportView struct {
	Title          string
	Subcmd         string
	Started        string

	// 顶部汇总 = 全量累计(跟 MaxFailedKept 无关)
	Total          int64
	OKCount        int64
	FailCount      int64
	PendingCount   int64
	StatRows       []statRow

	// 图表数据(SVG inline render 用)
	PieSVG    template.HTML // 状态码饼图
	HistSVG   template.HTML // wall time 直方图
	HistN     int           // 直方图样本数
	HistP50   string
	HistP95   string
	HistP99   string

	// 详情控制
	KeepOnlyFailed bool
	MaxFailedKept  int
	FailedDropped  int64
	Calls          []callView
}

func buildView(r *Recorder, calls []*Call) reportView {
	v := reportView{
		Title:          r.Title,
		Subcmd:         r.Subcmd,
		Started:        r.Started.Format("2006-01-02 15:04:05"),
		KeepOnlyFailed: r.KeepOnlyFailed,
		MaxFailedKept:  r.MaxFailedKept,
	}

	// 全量汇总:从 statusCount 出。totalCalls 含已 drop 的成功/失败。
	r.mu.Lock()
	v.Total = r.totalCalls
	v.FailedDropped = r.failedDropped
	codes := make([]int, 0, len(r.statusCount))
	for code := range r.statusCount {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	var finalSeen int64
	for _, code := range codes {
		cnt := r.statusCount[code]
		finalSeen += cnt
		cls := "fail"
		if code >= 200 && code < 300 {
			cls = "ok"
			v.OKCount += cnt
		} else if code >= 300 {
			v.FailCount += cnt
		}
		v.StatRows = append(v.StatRows, statRow{
			Code: code, Reason: reasonFor(code), Count: cnt, Cls: cls,
		})
	}
	r.mu.Unlock()
	// pending = 见过的 Call 数 - 已 final 的;包含进程退出时仍活跃的通
	if v.Total > finalSeen {
		v.PendingCount = v.Total - finalSeen
		v.StatRows = append(v.StatRows, statRow{
			Code: 0, Reason: "no final (timeout / aborted)", Count: v.PendingCount, Cls: "pending",
		})
	}

	// 图表渲染:饼图(status 分布)+ 直方图(wall durations)
	v.PieSVG = renderStatusPie(v.StatRows)
	r.mu.Lock()
	wallCopy := make([]time.Duration, len(r.wallDurs))
	copy(wallCopy, r.wallDurs)
	r.mu.Unlock()
	hSVG, hN, hP50, hP95, hP99 := renderWallHist(wallCopy)
	v.HistSVG = hSVG
	v.HistN = hN
	v.HistP50 = hP50
	v.HistP95 = hP95
	v.HistP99 = hP99

	// 详情区:已经在 r.calls 里的(失败保留前 MaxFailedKept 条 + 所有 pending)
	for i, c := range calls {
		cv := callView{
			Idx:       i + 1,
			CallID:    c.CallID,
			CallIDS:   shortCallID(c.CallID),
			From:      c.From,
			To:        c.To,
			Transport: c.Transport,
			Local:     c.Local,
			Remote:    c.Remote,
			Note:      c.Note,
		}
		if !c.Start.IsZero() {
			cv.StartStr = c.Start.Format("15:04:05.000")
		}
		if !c.End.IsZero() && !c.Start.IsZero() {
			cv.Duration = c.End.Sub(c.Start).Round(time.Millisecond).String()
		}
		switch {
		case c.FinalStatus == 0:
			cv.Status = "pending"
			cv.StatusCls = "pending"
		case c.FinalStatus >= 200 && c.FinalStatus < 300:
			cv.Status = fmt.Sprintf("%d %s", c.FinalStatus, c.FinalReason)
			cv.StatusCls = "ok"
		default:
			cv.Status = fmt.Sprintf("%d %s", c.FinalStatus, c.FinalReason)
			cv.StatusCls = "fail"
		}
		if c.RTPTx > 0 || c.RTPRx > 0 {
			cv.RTPLine = fmt.Sprintf("tx=%d pkts/%d B  rx=%d pkts/%d B",
				c.RTPTx, c.RTPTxBytes, c.RTPRx, c.RTPRxBytes)
		}
		for _, e := range c.Events {
			ev := eventView{
				TStr:    e.T.Format("15:04:05.000"),
				Dir:     e.Dir,
				First:   e.First,
				CSeq:    e.CSeq,
				FromTag: e.FromTag,
				ToTag:   e.ToTag,
			}
			if !c.Start.IsZero() {
				ev.DeltaMs = fmt.Sprintf("+%d ms", e.T.Sub(c.Start).Milliseconds())
			}
			cv.Events = append(cv.Events, ev)
		}
		cv.Ladder = template.HTML(renderLadder(c))
		v.Calls = append(v.Calls, cv)
	}
	return v
}

// renderStatusPie 把 status code 分布渲染成 SVG 饼图(全量累计,与 StatRows 同源)。
// 200x200 viewBox,中心 (100,100),半径 80。颜色按 2xx 绿 / ≥300 红 / 0 灰梯度。
// 计算 cumulative angle,每片用一个 path d="M cx cy L x1 y1 A r r 0 large 1 x2 y2 Z"。
func renderStatusPie(rows []statRow) template.HTML {
	if len(rows) == 0 {
		return ""
	}
	var total int64
	for _, r := range rows {
		total += r.Count
	}
	if total <= 0 {
		return ""
	}
	// 2xx 用渐变绿,≥300 用渐变红,pending(0)用灰
	greens := []string{"#34C759", "#2EB04E", "#27993E", "#1F7E32"}
	reds := []string{"#C00000", "#9C0000", "#7A0000", "#5A0000", "#3D0000"}
	gi, ri := 0, 0
	type slice struct {
		Label string
		Count int64
		Color string
	}
	slices := make([]slice, 0, len(rows))
	for _, row := range rows {
		s := slice{Count: row.Count}
		switch row.Cls {
		case "ok":
			s.Color = greens[gi%len(greens)]
			gi++
			s.Label = fmt.Sprintf("%d ✓", row.Code)
		case "fail":
			s.Color = reds[ri%len(reds)]
			ri++
			s.Label = fmt.Sprintf("%d", row.Code)
		case "pending":
			s.Color = "#888"
			s.Label = "pending"
		default:
			s.Color = "#bbb"
			s.Label = "?"
		}
		slices = append(slices, s)
	}

	const cx, cy, r = 100.0, 100.0, 80.0
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 360 200" class="pie">`)
	// 饼图本体在 0..200,右侧 200..360 是 legend
	cumAngle := -math.Pi / 2 // 从顶部 12 点起
	for _, s := range slices {
		frac := float64(s.Count) / float64(total)
		angle := 2 * math.Pi * frac
		x1 := cx + r*math.Cos(cumAngle)
		y1 := cy + r*math.Sin(cumAngle)
		cumAngle += angle
		x2 := cx + r*math.Cos(cumAngle)
		y2 := cy + r*math.Sin(cumAngle)
		large := 0
		if angle > math.Pi {
			large = 1
		}
		// 整圆特殊处理(单一类别 100%)— path 退化,改画完整 circle
		if frac >= 0.999 {
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s"/>`, cx, cy, r, s.Color)
			break
		}
		fmt.Fprintf(&b, `<path d="M %.1f %.1f L %.1f %.1f A %.1f %.1f 0 %d 1 %.1f %.1f Z" fill="%s"/>`,
			cx, cy, x1, y1, r, r, large, x2, y2, s.Color)
	}
	// legend
	ly := 30
	for _, s := range slices {
		frac := float64(s.Count) * 100 / float64(total)
		fmt.Fprintf(&b, `<rect x="210" y="%d" width="12" height="12" fill="%s"/>`, ly, s.Color)
		fmt.Fprintf(&b, `<text x="230" y="%d" class="lg-lbl">%s</text>`, ly+10, htmlEscape(s.Label))
		fmt.Fprintf(&b, `<text x="290" y="%d" class="lg-cnt">%d  (%.0f%%)</text>`, ly+10, s.Count, frac)
		ly += 22
		if ly > 180 {
			break
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// renderWallHist 把 wallDurs 渲染成直方图(20 bins,linear scale)+ p50/p95 标注。
// 返回 (svg, n, p50, p95, p99)。n=0 时返回空 svg。
func renderWallHist(durs []time.Duration) (template.HTML, int, string, string, string) {
	n := len(durs)
	if n == 0 {
		return "", 0, "—", "—", "—"
	}
	sorted := make([]time.Duration, n)
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 := sorted[n*50/100]
	p95 := sorted[n*95/100]
	p99 := sorted[n*99/100]
	if p99 == 0 && n > 0 {
		p99 = sorted[n-1]
	}
	// bin 边界:取 min ~ p99 范围(去掉极端长尾,看主分布)
	lo := sorted[0]
	hi := p99
	if hi <= lo {
		hi = sorted[n-1]
	}
	if hi <= lo {
		hi = lo + time.Millisecond
	}
	const bins = 20
	span := hi - lo
	binW := span / bins
	if binW <= 0 {
		binW = time.Millisecond
	}
	counts := make([]int, bins)
	for _, d := range sorted {
		idx := int((d - lo) / binW)
		if idx < 0 {
			idx = 0
		}
		if idx >= bins {
			idx = bins - 1
		}
		counts[idx]++
	}
	maxC := 0
	for _, c := range counts {
		if c > maxC {
			maxC = c
		}
	}
	if maxC == 0 {
		maxC = 1
	}
	// SVG 600x180 (含 axis 留白)
	const W, H = 600.0, 180.0
	const padL, padR, padT, padB = 40.0, 20.0, 12.0, 30.0
	plotW := W - padL - padR
	plotH := H - padT - padB
	barW := plotW / bins
	var b strings.Builder
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 600 180" class="hist">`)
	// y axis ticks (0, max)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="ax-lbl">%d</text>`, 4.0, padT+10, maxC)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="ax-lbl">0</text>`, 4.0, padT+plotH+4)
	// bars
	for i, c := range counts {
		bh := float64(c) / float64(maxC) * plotH
		x := padL + float64(i)*barW
		y := padT + plotH - bh
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="bar"/>`, x, y, barW-1, bh)
	}
	// p50 / p95 lines
	p50x := padL + float64(p50-lo)/float64(binW*bins)*plotW
	p95x := padL + float64(p95-lo)/float64(binW*bins)*plotW
	if p50x < padL {
		p50x = padL
	}
	if p50x > padL+plotW {
		p50x = padL + plotW
	}
	if p95x < padL {
		p95x = padL
	}
	if p95x > padL+plotW {
		p95x = padL + plotW
	}
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="pline p50"/>`, p50x, padT, p50x, padT+plotH)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="pmark">p50 %s</text>`, p50x+3, padT+10, p50.Round(time.Millisecond).String())
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="pline p95"/>`, p95x, padT, p95x, padT+plotH)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="pmark">p95 %s</text>`, p95x+3, padT+24, p95.Round(time.Millisecond).String())
	// x axis labels (lo / hi)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="ax-lbl">%s</text>`, padL, H-padB+18, lo.Round(time.Millisecond).String())
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="ax-lbl" text-anchor="end">%s</text>`, padL+plotW, H-padB+18, hi.Round(time.Millisecond).String())
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), n,
		p50.Round(time.Millisecond).String(),
		p95.Round(time.Millisecond).String(),
		p99.Round(time.Millisecond).String()
}

func htmlEscape(s string) string {
	return template.HTMLEscapeString(s)
}

// reasonFor 把常见 SIP status code 映射成简短理由字符串(只覆盖最常见的)。
func reasonFor(code int) string {
	switch code {
	case 0:
		return "pending"
	case 200:
		return "OK"
	case 100:
		return "Trying"
	case 180:
		return "Ringing"
	case 183:
		return "Session Progress"
	case 302:
		return "Moved Temporarily"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 407:
		return "Proxy Auth Required"
	case 408:
		return "Request Timeout"
	case 480:
		return "Temporarily Unavailable"
	case 481:
		return "Call/Transaction Does Not Exist"
	case 486:
		return "Busy Here"
	case 487:
		return "Request Terminated"
	case 488:
		return "Not Acceptable Here"
	case 500:
		return "Server Internal Error"
	case 503:
		return "Service Unavailable"
	case 504:
		return "Server Time-out"
	case 600:
		return "Busy Everywhere"
	case 603:
		return "Decline"
	}
	return ""
}

// renderLadder 出 SVG 时序图(server-side 渲染,纯字符串)。
//
// 布局:左 lifeline = 本端(local),右 lifeline = 对端(SBC/remote)。
// 每条 event 一行 40px。
//   - Dir="TX"(本端发) → 左 → 右 箭头
//   - Dir="RX"(本端收) → 右 → 左 箭头
// 失败 / 错误响应箭头红色,2xx 绿色,1xx 灰色,请求蓝色。
func renderLadder(c *Call) string {
	const (
		leftX   = 130
		rightX  = 570
		rowH    = 42
		topPad  = 60
		botPad  = 30
		width   = 720
	)
	height := topPad + botPad + rowH*len(c.Events)
	if height < 200 {
		height = 200
	}

	leftLabel := "UAC (local)"
	rightLabel := "SBC / peer"
	if c.Local != "" {
		leftLabel = "local " + c.Local
	}
	if c.Remote != "" {
		rightLabel = "peer " + c.Remote
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" class="ladder">`, width, height)
	// lifeline rects + labels
	fmt.Fprintf(&b, `<rect x="%d" y="20" width="160" height="28" rx="4" class="actor"/>`, leftX-80)
	fmt.Fprintf(&b, `<text x="%d" y="38" class="actor-label">%s</text>`, leftX, html.EscapeString(leftLabel))
	fmt.Fprintf(&b, `<rect x="%d" y="20" width="160" height="28" rx="4" class="actor"/>`, rightX-80)
	fmt.Fprintf(&b, `<text x="%d" y="38" class="actor-label">%s</text>`, rightX, html.EscapeString(rightLabel))
	// lifelines
	fmt.Fprintf(&b, `<line x1="%d" y1="48" x2="%d" y2="%d" class="lifeline"/>`, leftX, leftX, height-10)
	fmt.Fprintf(&b, `<line x1="%d" y1="48" x2="%d" y2="%d" class="lifeline"/>`, rightX, rightX, height-10)

	defs := `<defs>
<marker id="arr-req" viewBox="0 0 10 10" refX="10" refY="5" markerWidth="8" markerHeight="8" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="#1677FF"/></marker>
<marker id="arr-ok" viewBox="0 0 10 10" refX="10" refY="5" markerWidth="8" markerHeight="8" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="#34C759"/></marker>
<marker id="arr-prov" viewBox="0 0 10 10" refX="10" refY="5" markerWidth="8" markerHeight="8" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="#999"/></marker>
<marker id="arr-fail" viewBox="0 0 10 10" refX="10" refY="5" markerWidth="8" markerHeight="8" orient="auto"><path d="M0,0 L10,5 L0,10 z" fill="#C00000"/></marker>
</defs>`
	b.WriteString(defs)

	for i, ev := range c.Events {
		y := topPad + rowH*i + rowH/2
		var x1, x2 int
		if ev.Dir == "TX" {
			x1, x2 = leftX, rightX
		} else {
			x1, x2 = rightX, leftX
		}
		cls, marker := classifyArrow(ev)
		fmt.Fprintf(&b,
			`<line x1="%d" y1="%d" x2="%d" y2="%d" class="arrow %s" marker-end="url(#%s)"/>`,
			x1, y, x2, y, cls, marker)

		// 标签:箭头上方居中,first line + cseq + Δt
		labelX := (leftX + rightX) / 2
		delta := ""
		if !c.Start.IsZero() {
			delta = fmt.Sprintf("  Δ%dms", ev.T.Sub(c.Start).Milliseconds())
		}
		label := html.EscapeString(fmt.Sprintf("%s   [%s]%s", ev.First, ev.CSeq, delta))
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="ev-label %s">%s</text>`,
			labelX, y-6, cls, label)
		// 时间戳放最左
		fmt.Fprintf(&b, `<text x="6" y="%d" class="ev-time">%s</text>`,
			y+4, ev.T.Format("15:04:05.000"))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func classifyArrow(ev Event) (cls, marker string) {
	if ev.IsReq {
		return "req", "arr-req"
	}
	switch {
	case ev.Status >= 200 && ev.Status < 300:
		return "ok", "arr-ok"
	case ev.Status >= 100 && ev.Status < 200:
		return "prov", "arr-prov"
	case ev.Status >= 300:
		return "fail", "arr-fail"
	}
	return "prov", "arr-prov"
}

// ===== 小工具 =====

// trimURI 把 "<sip:user@host>;tag=xxx" 简化为 "sip:user@host"。
func trimURI(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return s[i+1 : i+j]
		}
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		return s[:i]
	}
	return s
}

// shortRURI 截断过长 RURI 方便列表显示。
func shortRURI(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:57] + "..."
}

func shortCallID(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:10] + "…" + s[len(s)-10:]
}

// ===== HTML 模板 =====

var htmlTmpl = template.Must(template.New("rep").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>{{.Title}}</title>
<style>
:root { --ok:#34C759; --fail:#C00000; --pending:#888; --primary:#1677FF; --bg:#fff; --muted:#666; --hair:#e3e3e3; --row-hover:#f6faff; }
body { font-family: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; margin:0; background:#fafafa; color:#222; }
header { background:linear-gradient(90deg,#0F4FB8,#1677FF); color:white; padding:16px 24px; }
header h1 { margin:0; font-size:20px; font-weight:600; }
header .meta { font-size:12px; opacity:.85; margin-top:4px; }
main { max-width:1200px; margin:0 auto; padding:16px 24px 48px; }
.summary { display:flex; gap:16px; margin:12px 0 20px; }
.summary .card { flex:1; background:white; padding:12px 16px; border-radius:8px; box-shadow:0 1px 2px rgba(0,0,0,.06); }
.summary .num { font-size:24px; font-weight:600; }
.summary .lbl { font-size:12px; color:var(--muted); margin-top:2px; }
.summary .ok .num { color:var(--ok); }
.summary .fail .num { color:var(--fail); }
.summary .total .num { color:var(--primary); }
.summary .pending .num { color:var(--pending); }
.stat-table { margin:12px 0 20px; background:white; padding:12px 16px; border-radius:8px; box-shadow:0 1px 2px rgba(0,0,0,.06); }
.stat-table h3 { font-size:13px; font-weight:600; margin:0 0 8px; color:#444; }
.stat-table table { width:auto; border-collapse:collapse; font-size:12px; font-family:Menlo,Consolas,monospace; }
.stat-table th, .stat-table td { padding:4px 16px 4px 0; text-align:left; }
.stat-table th { font-family:-apple-system,"PingFang SC",sans-serif; font-size:11px; color:var(--muted); font-weight:600; }
.detail-banner { background:#fff6e0; border-left:3px solid #FFCB66; padding:8px 14px; font-size:13px; margin-bottom:12px; border-radius:4px; }
.detail-banner .warn { color:var(--fail); font-size:12px; }
details.call { background:white; border-radius:8px; box-shadow:0 1px 2px rgba(0,0,0,.06); margin-bottom:8px; overflow:hidden; }
details.call > summary { list-style:none; cursor:pointer; padding:10px 16px; display:grid; grid-template-columns:30px 90px 1fr 1fr 80px 100px 130px 1fr; gap:8px; align-items:center; font-size:13px; }
details.call > summary::-webkit-details-marker { display:none; }
details.call > summary:hover { background:var(--row-hover); }
details.call[open] > summary { background:var(--row-hover); border-bottom:1px solid var(--hair); }
.col-idx { color:var(--muted); }
.col-t { color:var(--muted); font-family:Menlo,Consolas,monospace; font-size:12px; }
.col-uri { white-space:nowrap; overflow:hidden; text-overflow:ellipsis; font-family:Menlo,Consolas,monospace; font-size:12px; }
.col-trans { font-size:11px; padding:2px 6px; border-radius:4px; background:#eef2f7; color:#374151; text-transform:uppercase; text-align:center; }
.col-status { font-weight:600; }
.col-status.ok { color:var(--ok); }
.col-status.fail { color:var(--fail); }
.col-status.pending { color:var(--pending); }
.col-dur { color:var(--muted); font-family:Menlo,Consolas,monospace; font-size:12px; }
.col-rtp { color:var(--muted); font-size:11px; font-family:Menlo,Consolas,monospace; }
.body { padding:16px; }
.meta-grid { display:grid; grid-template-columns:120px 1fr; gap:4px 16px; font-size:12px; font-family:Menlo,Consolas,monospace; color:#444; margin-bottom:16px; }
.meta-grid .k { color:var(--muted); }
.note { background:#fff6e0; border-left:3px solid #FFCB66; padding:6px 10px; font-size:12px; white-space:pre-wrap; margin-bottom:12px; }
table.events { width:100%; border-collapse:collapse; font-size:12px; font-family:Menlo,Consolas,monospace; margin-top:12px; }
table.events th, table.events td { padding:4px 8px; border-bottom:1px solid var(--hair); text-align:left; vertical-align:top; }
table.events th { font-weight:600; color:var(--muted); font-family:-apple-system,"PingFang SC",sans-serif; font-size:11px; }
.dir-TX { color:var(--primary); font-weight:600; }
.dir-RX { color:#9333ea; font-weight:600; }
.ladder { width:100%; height:auto; background:#fbfbfd; border:1px solid var(--hair); border-radius:4px; }
.ladder .actor { fill:#eaf2ff; stroke:var(--primary); stroke-width:1; }
.ladder .actor-label { font-family:-apple-system,"PingFang SC",sans-serif; font-size:12px; fill:#0F4FB8; text-anchor:middle; font-weight:600; dominant-baseline:middle; }
.ladder .lifeline { stroke:#bbb; stroke-width:1; stroke-dasharray:4 4; }
.ladder .arrow { stroke-width:1.8; fill:none; }
.ladder .arrow.req { stroke:var(--primary); }
.ladder .arrow.ok { stroke:var(--ok); }
.ladder .arrow.prov { stroke:#999; stroke-dasharray:5 3; }
.ladder .arrow.fail { stroke:var(--fail); }
.ladder .ev-label { font-family:Menlo,Consolas,monospace; font-size:11px; text-anchor:middle; }
.ladder .ev-label.req { fill:var(--primary); }
.ladder .ev-label.ok { fill:#1b8a3f; }
.ladder .ev-label.prov { fill:#666; }
.ladder .ev-label.fail { fill:var(--fail); }
.ladder .ev-time { font-family:Menlo,Consolas,monospace; font-size:10px; fill:#888; }
.empty { padding:32px; text-align:center; color:var(--muted); }
.charts { display:grid; grid-template-columns:380px 1fr; gap:16px; margin:12px 0 20px; }
.chart-card { background:white; padding:12px 16px; border-radius:8px; box-shadow:0 1px 2px rgba(0,0,0,.06); }
.chart-card h3 { font-size:13px; font-weight:600; margin:0 0 8px; color:#444; }
.chart-card .sub { font-size:11px; color:var(--muted); font-family:Menlo,Consolas,monospace; margin-bottom:6px; }
.pie { width:100%; height:auto; max-height:200px; }
.pie .lg-lbl { font-family:Menlo,Consolas,monospace; font-size:11px; fill:#222; dominant-baseline:middle; }
.pie .lg-cnt { font-family:Menlo,Consolas,monospace; font-size:11px; fill:#666; dominant-baseline:middle; }
.hist { width:100%; height:auto; max-height:200px; }
.hist .bar { fill:var(--primary); fill-opacity:.72; }
.hist .pline { stroke-width:1; stroke-dasharray:3 3; }
.hist .pline.p50 { stroke:var(--ok); }
.hist .pline.p95 { stroke:#FFCB66; }
.hist .pmark { font-family:Menlo,Consolas,monospace; font-size:10px; fill:#222; }
.hist .ax-lbl { font-family:Menlo,Consolas,monospace; font-size:10px; fill:var(--muted); }
@media (max-width: 900px) { .charts { grid-template-columns: 1fr; } }
</style>
</head><body>
<header>
<h1>{{.Title}}</h1>
<div class="meta">subcmd: {{.Subcmd}} · started: {{.Started}}</div>
</header>
<main>
<section class="summary">
<div class="card total"><div class="num">{{.Total}}</div><div class="lbl">总呼叫 (全量)</div></div>
<div class="card ok"><div class="num">{{.OKCount}}</div><div class="lbl">成功 (2xx)</div></div>
<div class="card fail"><div class="num">{{.FailCount}}</div><div class="lbl">失败 (≥300)</div></div>
<div class="card pending"><div class="num">{{.PendingCount}}</div><div class="lbl">无 final</div></div>
</section>

{{if or .PieSVG .HistSVG}}
<section class="charts">
{{if .PieSVG}}
<div class="chart-card">
<h3>Status code distribution</h3>
<div class="sub">slice = code · legend shows count and percent</div>
{{.PieSVG}}
</div>
{{end}}
{{if .HistSVG}}
<div class="chart-card">
<h3>Wall time distribution</h3>
<div class="sub">n = {{.HistN}}  ·  p50 {{.HistP50}}  ·  p95 {{.HistP95}}  ·  p99 {{.HistP99}}  ·  range trimmed at p99</div>
{{.HistSVG}}
</div>
{{end}}
</section>
{{end}}

{{if .StatRows}}
<section class="stat-table">
<h3>Status code table</h3>
<table><thead><tr><th>code</th><th>reason</th><th>count</th></tr></thead><tbody>
{{range .StatRows}}<tr><td class="col-status {{.Cls}}">{{if eq .Code 0}}—{{else}}{{.Code}}{{end}}</td><td>{{.Reason}}</td><td>{{.Count}}</td></tr>{{end}}
</tbody></table>
</section>
{{end}}

<section class="detail-banner">
<strong>失败信令详情</strong>
— 保留最多 {{.MaxFailedKept}} 条失败 + 所有 pending;成功通不展示
{{if gt .FailedDropped 0}}<br><span class="warn">⚠ 已超失败上限,另外 {{.FailedDropped}} 通失败详情已丢弃(只计入汇总)</span>{{end}}
</section>
{{if .Calls}}
<div class="header-row" style="font-size:11px;color:#666;display:grid;grid-template-columns:30px 90px 1fr 1fr 80px 100px 130px 1fr;gap:8px;padding:6px 16px;text-transform:uppercase;letter-spacing:.4px;">
<div>#</div><div>start</div><div>from</div><div>to</div><div>trans</div><div>duration</div><div>status</div><div>rtp</div>
</div>
{{range .Calls}}
<details class="call">
<summary>
<span class="col-idx">{{.Idx}}</span>
<span class="col-t">{{.StartStr}}</span>
<span class="col-uri" title="{{.From}}">{{.From}}</span>
<span class="col-uri" title="{{.To}}">{{.To}}</span>
<span class="col-trans">{{.Transport}}</span>
<span class="col-dur">{{.Duration}}</span>
<span class="col-status {{.StatusCls}}">{{.Status}}</span>
<span class="col-rtp">{{.RTPLine}}</span>
</summary>
<div class="body">
<div class="meta-grid">
<span class="k">Call-ID</span><span>{{.CallID}}</span>
<span class="k">From</span><span>{{.From}}</span>
<span class="k">To</span><span>{{.To}}</span>
<span class="k">Transport</span><span>{{.Transport}}</span>
<span class="k">Local</span><span>{{.Local}}</span>
<span class="k">Remote</span><span>{{.Remote}}</span>
<span class="k">Duration</span><span>{{.Duration}}</span>
<span class="k">Final</span><span class="col-status {{.StatusCls}}">{{.Status}}</span>
{{if .RTPLine}}<span class="k">RTP</span><span>{{.RTPLine}}</span>{{end}}
</div>
{{if .Note}}<div class="note">{{.Note}}</div>{{end}}
{{.Ladder}}
<table class="events"><thead><tr><th>time</th><th>Δ</th><th>dir</th><th>first line</th><th>CSeq</th><th>from-tag</th><th>to-tag</th></tr></thead><tbody>
{{range .Events}}<tr>
<td>{{.TStr}}</td><td>{{.DeltaMs}}</td><td class="dir-{{.Dir}}">{{.Dir}}</td><td>{{.First}}</td><td>{{.CSeq}}</td><td>{{.FromTag}}</td><td>{{.ToTag}}</td>
</tr>{{end}}
</tbody></table>
</div>
</details>
{{end}}
{{else}}
<div class="empty">本次未捕获任何呼叫信令。</div>
{{end}}
</main>
</body></html>
`))
