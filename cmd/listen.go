package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"dsipper/internal/clui"
	"dsipper/internal/logsink"
	"dsipper/internal/media"
	"dsipper/internal/report"
	"dsipper/internal/sdp"
	"dsipper/internal/sipua"
)

// dialogTable 跟踪在跑的呼叫,供 BYE / SIGTERM 联动取消。
type dialogTable struct {
	mu      sync.Mutex
	cancels map[string]context.CancelFunc // call-id -> cancel
}

func newDialogTable() *dialogTable {
	return &dialogTable{cancels: map[string]context.CancelFunc{}}
}
func (d *dialogTable) put(callID string, c context.CancelFunc) {
	d.mu.Lock()
	d.cancels[callID] = c
	d.mu.Unlock()
}
func (d *dialogTable) cancel(callID string) {
	d.mu.Lock()
	c, ok := d.cancels[callID]
	delete(d.cancels, callID)
	d.mu.Unlock()
	if ok {
		c()
	}
}
func (d *dialogTable) cancelAll() {
	d.mu.Lock()
	for _, c := range d.cancels {
		c()
	}
	d.cancels = map[string]context.CancelFunc{}
	d.mu.Unlock()
}

// Listen 子命令:UAS 模式,监听端口接收 INVITE / OPTIONS,自动 200 接听并回 RTP。
func Listen(args []string) {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	transport := fs.String("transport", "udp", "udp / tls")
	bind := fs.String("bind", "0.0.0.0:5060", "监听 host:port")
	cert := fs.String("cert", "", "TLS server 证书 (transport=tls 必填)")
	key := fs.String("key", "", "TLS 私钥 (transport=tls 必填)")
	codecName := fs.String("codec", "PCMA", "回送 codec PCMA / PCMU")
	tone := fs.Float64("tone", 880, "回送正弦波频率 Hz (与主叫不同方便区分)")
	saveRecv := fs.String("save-recv", "", "把每通呼叫收到的 RTP 落 WAV (前缀,如 'rx',会写 rx-N.wav)")
	byeAfter := fs.Duration("bye-after", 0, "UAS 在答 200 OK 后 N 秒主动发 BYE (0=不主动 BYE,只回响应)")
	noRTP := fs.Bool("no-rtp", false, "信令 only 模式:跳过 RTP socket / SDP answer / 媒体协程,只回纯 200 OK + 等 BYE(用于 cps 压测)")
	enableUI := fs.Bool("ui", false, "stderr 显示实时统计面板(total / ok / fail / active / cps)")
	verbose := fs.Int("v", 0, "verbose 0/1")
	logFile := fs.String("log", "", "日志落盘路径;空=cwd 下 dsipper-listen-<时间戳>.log;'-'=只打 stderr")
	logMaxMB := fs.Int("log-max-mb", 100, "单日志文件 size 上限 MB,达上限 rename 到 .log.old 重开(0=不滚动)")
	logOnlyFailed := fs.Bool("log-only-failed", false, "只落失败通日志:含 call-id 的日志先 buffer,2xx 时丢弃,>=300 / 退出 pending 时才 flush")
	tlsKeepalive := fs.Duration("tls-keepalive", 0, "TLS 长连接每隔 N 发 \\r\\n\\r\\n 心跳(RFC 5626);0=关")
	reportPath := fs.String("report", "", "退出时把所有接到的呼叫信令落一份 HTML report 到该路径(目录或具体 .html);空=不生成")
	reportMaxFailed := fs.Int("report-max-failed", 50, "HTML 详情区保留多少条失败通的信令图(成功不展示);超出只计入顶部汇总")
	pcapOpts := AttachPcap(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	log, bufH := co(*verbose, *logFile, "listen", *logMaxMB, *logOnlyFailed, *enableUI)

	stopPcap, err := pcapOpts.Start(log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: pcap: %v\n", err)
		os.Exit(1)
	}
	defer stopPcap()

	var t sipua.Transport
	switch *transport {
	case "udp":
		t, err = sipua.NewUDPClient(*bind)
	case "tls":
		if *cert == "" || *key == "" {
			fmt.Fprintln(os.Stderr, "ERR: TLS 模式需要 --cert / --key")
			os.Exit(2)
		}
		t, err = sipua.NewTLSServer(*bind, sipua.TLSOptions{CertFile: *cert, KeyFile: *key})
	default:
		fmt.Fprintln(os.Stderr, "ERR: --transport udp/tls")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	if *tlsKeepalive > 0 {
		if ka, ok := t.(interface{ EnableKeepalive(time.Duration) }); ok {
			ka.EnableKeepalive(*tlsKeepalive)
		}
	}
	defer t.Close()

	Banner("listen", []clui.KV{
		{K: "bind", V: clui.Bold(clui.Blue(t.LocalAddr().String())) + clui.Slate(" ("+*transport+")")},
		{K: "codec", V: clui.Green(*codecName)},
		{K: "tone", V: fmt.Sprintf("%.0f Hz", *tone)},
		{K: "no-rtp", V: defaultStr(boolStr(*noRTP), clui.Dim("off"))},
		{K: "bye-after", V: defaultStr(durStrIfPos(*byeAfter), clui.Dim("off"))},
		{K: "report", V: defaultStr(*reportPath, clui.Dim("off"))},
		{K: "log-only-failed", V: defaultStr(boolStr(*logOnlyFailed), clui.Dim("off"))},
	})

	// recorder 启用条件:--report / --log-only-failed / --ui 三选一启动一个 recorder。
	// 后两个不写 HTML,只用 recorder 做 final 信号源驱动 BufHandler 或 LiveStats。
	var rep *report.Recorder
	wantReport := *reportPath != ""
	if wantReport || bufH != nil || *enableUI {
		rep = report.New("listen", fmt.Sprintf("dsipper listen report — %s://%s", *transport, t.LocalAddr()))
		rep.MaxFailedKept = *reportMaxFailed
		if bufH != nil {
			rep.LogCtrl = bufH
		}
		defer func() {
			if wantReport {
				if p, err := rep.SaveHTML(*reportPath); err != nil {
					fmt.Fprintf(os.Stderr, "WARN: save report: %v\n", err)
				} else {
					fmt.Printf("report HTML: %s\n", p)
				}
			}
			if bufH != nil {
				bufH.FlushAll()
			}
		}()
	}

	// 实时面板(--ui 启动多行 LivePanel,1Hz 刷新;非 tty 静默 noop)
	var panel *clui.LivePanel
	if *enableUI && rep != nil {
		panelStart := time.Now()
		var lastTotal int64
		var lastT = panelStart
		panel = clui.NewLivePanel(os.Stderr, "listen", time.Second, func() []clui.KV {
			s := rep.Snapshot()
			now := time.Now()
			dt := now.Sub(lastT).Seconds()
			cps := 0.0
			if dt > 0 {
				cps = float64(s.Total-lastTotal) / dt
			}
			lastTotal = s.Total
			lastT = now

			rows := []clui.KV{
				{K: "total", V: clui.Bold(clui.Blue(fmt.Sprintf("%d", s.Total))) +
					clui.Dim("  active ") + clui.Bold(clui.Yellow(fmt.Sprintf("%d", s.Pending)))},
				{K: "ok / fail", V: clui.Bold(clui.Green(fmt.Sprintf("%d ✓", s.OK))) +
					"   " + clui.Bold(clui.Red(fmt.Sprintf("%d ✗", s.Fail)))},
				{K: "cps now", V: clui.Yellow(fmt.Sprintf("%.1f", cps)) +
					clui.Dim(fmt.Sprintf("   avg %.2f", float64(s.Total)/time.Since(panelStart).Seconds()))},
			}
			if line := statusDistLine(s.Status); line != "" {
				rows = append(rows, clui.KV{K: "status", V: line})
			}
			rows = append(rows, clui.KV{K: "uptime", V: time.Since(panelStart).Round(time.Second).String()})
			return rows
		})
		panel.Start()
		defer panel.Stop()
	}

	ctx, cancel := signalContext()
	defer cancel()

	codecObj := sdp.PCMA
	if strings.EqualFold(*codecName, "PCMU") {
		codecObj = sdp.PCMU
	}

	dt := newDialogTable()
	var wg sync.WaitGroup

	defer func() {
		dt.cancelAll() // 主退出时联动取消所有 in-flight dialog
		wg.Wait()      // 等 handler 走完 dump
	}()

	callIdx := 0
	for {
		select {
		case <-ctx.Done():
			return
		case in, ok := <-t.Recv():
			if !ok {
				return
			}
			m, err := sipua.Parse(in.Data)
			if err != nil {
				log.Warn("parse", "err", err)
				continue
			}
			if !m.IsRequest {
				continue
			}
			peer := ""
			if in.From != nil {
				peer = in.From.String()
			}
			if rep != nil {
				rep.Record("RX", m, peer)
			}
			switch m.Method {
			case "OPTIONS":
				replyStatus(t, in, m, 200, "OK", rep)
				log.Info("OPTIONS RX", "from", in.From)
			case "INVITE":
				callIdx++
				if rep != nil {
					localAddr := ""
					if la := t.LocalAddr(); la != nil {
						localAddr = la.String()
					}
					rep.SetMeta(m.Headers.Get("Call-ID"), *transport, localAddr, peer)
				}
				if *noRTP {
					handleIncomingCallSignalingOnly(t, in, m, log, dt, rep)
				} else {
					wg.Add(1)
					go func(idx int, inb sipua.Inbound, msg *sipua.Message) {
						defer wg.Done()
						handleIncomingCall(ctx, t, inb, msg, codecObj, *tone, *saveRecv, *byeAfter, idx, log, dt, rep)
					}(callIdx, in, m)
				}
			case "ACK":
				log.Debug("ACK rx (in-dialog)")
			case "BYE":
				replyStatus(t, in, m, 200, "OK", rep)
				log.Info("BYE RX → 200", "call-id", m.Headers.Get("Call-ID"))
				dt.cancel(m.Headers.Get("Call-ID"))
			case "CANCEL":
				replyStatus(t, in, m, 200, "OK", rep)
				log.Info("CANCEL RX → 200")
				dt.cancel(m.Headers.Get("Call-ID"))
			default:
				replyStatus(t, in, m, 405, "Method Not Allowed", rep)
				log.Info("method not allowed", "method", m.Method)
			}
		}
	}
}

// handleIncomingCallSignalingOnly 纯信令应答(不开 RTP),用于 cps 压测。
// 直接在 listen 主 loop 里同步回 200 OK,几百 μs 完成,不 spawn goroutine,不建任何 UDP socket。
// dialog 注册 to-tag 到 dialogTable,后续收到 BYE 主 loop 那一支会 cancel。
func handleIncomingCallSignalingOnly(t sipua.Transport, in sipua.Inbound, invite *sipua.Message,
	log *slog.Logger, dt *dialogTable, rep *report.Recorder) {
	resp := buildResponse(invite, 200, "OK")
	// 加 to-tag,让 dialog 合法
	toHdr := resp.Headers.Get("To")
	if !strings.Contains(toHdr, "tag=") {
		resp.Headers.Set("To", toHdr+";tag="+sipua.Branch()[len("z9hG4bK-"):])
	}
	// 不带 Contact / SDP — sipp 内置 uac scenario 不强校验,空 200 OK 够走完 ACK/BYE
	sendReply(t, in, resp, rep)
	// 注册 dialog 表占位(收到 BYE 时由主 loop dt.cancel 移除)
	dt.put(invite.Headers.Get("Call-ID"), func() {})
}

func handleIncomingCall(ctx context.Context, t sipua.Transport, in sipua.Inbound, invite *sipua.Message,
	codecObj sdp.Codec, toneHz float64, saveRecvPrefix string, byeAfter time.Duration, idx int, log *slog.Logger, dt *dialogTable, rep *report.Recorder) {

	// 100 Trying
	replyStatus(t, in, invite, 100, "Trying", rep)
	// 180 Ringing 一下,模拟真实
	replyStatus(t, in, invite, 180, "Ringing", rep)
	time.Sleep(200 * time.Millisecond)

	// 解 SDP offer
	offer, err := sdp.Parse(string(invite.Body))
	if err != nil {
		log.Warn("SDP offer parse", "err", err)
		replyStatus(t, in, invite, 488, "Not Acceptable Here", rep)
		return
	}

	localIP := localIPForRemote(invite.Headers.Get("Via"))
	rtp, err := media.NewRTPSession(localIP, uint8(codecObj.PT), codecObj.Name)
	if err != nil {
		log.Warn("rtp init", "err", err)
		replyStatus(t, in, invite, 500, "Server Error", rep)
		return
	}
	defer rtp.Close()
	if err := rtp.SetRemote(offer.ConnIP, offer.AudioPort); err != nil {
		log.Warn("rtp remote", "err", err)
		replyStatus(t, in, invite, 488, "Not Acceptable Here", rep)
		return
	}

	// 拼 200 OK + answer SDP
	answer := sdp.Offer{
		SessionID:  uint64(rand.Uint32()),
		SessionVer: uint64(rand.Uint32()),
		Username:   "dsipper-uas",
		Origin:     localIP,
		ConnIP:     localIP,
		AudioPort:  rtp.LocalPort(),
		Codecs:     []sdp.Codec{codecObj},
	}
	uasContact := fmt.Sprintf("<sip:uas@%s;transport=%s>", localIP, t.Proto())
	resp := buildResponse(invite, 200, "OK")
	resp.Headers.Set("Contact", uasContact)
	resp.Headers.Set("Content-Type", "application/sdp")
	// 给 To 加 to-tag(后续 UAS-initiated BYE 也要用这个 to-tag 作为 From-tag)
	toHdr := resp.Headers.Get("To")
	if !strings.Contains(toHdr, "tag=") {
		toHdr = toHdr + ";tag=" + sipua.Branch()[len("z9hG4bK-"):]
		resp.Headers.Set("To", toHdr)
	}
	localToHeader := toHdr // 即 UAS 角度的 From,after-200
	resp.Body = []byte(answer.Build())
	sendReply(t, in, resp, rep)
	log.Info("INVITE → 200 OK", "call", idx, "remote-rtp", fmt.Sprintf("%s:%d", offer.ConnIP, offer.AudioPort))

	// RTP 双向:边收边发 880Hz 正弦波,直到 BYE / SIGTERM / 上限到。
	rtpCtx, rtpCancel := context.WithCancel(ctx)
	defer rtpCancel()
	dt.put(invite.Headers.Get("Call-ID"), rtpCancel)

	// 调度 UAS 主动 BYE(--bye-after > 0 才启用)
	if byeAfter > 0 {
		go func() {
			t0 := time.Now()
			select {
			case <-rtpCtx.Done():
				return // 对端先 BYE 了,无需主动发
			case <-time.After(byeAfter):
			}
			sendUASBye(t, in, invite, localToHeader, uasContact, localIP, log, rep)
			log.Info("UAS BYE sent",
				"call", idx,
				"call-id", invite.Headers.Get("Call-ID"),
				"after", time.Since(t0).Round(time.Millisecond),
			)
			rtpCancel() // BYE 发出 → 自家 RTP 停
		}()
	}

	// PCM 时长:cover 默认 30s 与 byeAfter+grace 的较大者,避免提前静音
	pcmDur := 30.0
	if byeAfter > 0 {
		if d := byeAfter.Seconds() + 2; d > pcmDur {
			pcmDur = d
		}
	}
	pcm := media.SineTone(toneHz, pcmDur, 8000, 0.3)

	go rtp.Recv(rtpCtx, log)
	go func() {
		_ = rtp.Send(rtpCtx, pcm)
	}()

	upperBound := 30 * time.Second
	if d := byeAfter + 5*time.Second; d > upperBound {
		upperBound = d
	}
	select {
	case <-rtpCtx.Done():
	case <-time.After(upperBound):
	}

	tx, rx, txB, rxB := rtp.Stats()
	if rep != nil {
		rep.SetRTP(invite.Headers.Get("Call-ID"), tx, rx, txB, rxB)
	}
	dtmfRX := rtp.RxDTMF()
	if dtmfRX != "" {
		log.Info("call done", "call", idx, "tx", tx, "rx", rx, "txB", txB, "rxB", rxB, "dtmf", dtmfRX)
	} else {
		log.Info("call done", "call", idx, "tx", tx, "rx", rx, "txB", txB, "rxB", rxB)
	}
	if saveRecvPrefix != "" {
		path := fmt.Sprintf("%s-%d.wav", saveRecvPrefix, idx)
		if err := rtp.DumpWAV(path); err != nil {
			log.Warn("dump wav", "err", err)
		} else {
			log.Info("recv WAV", "path", path)
		}
	}
}

// sendUASBye 以 UAS 角度发一条 BYE。
//
// 拼装规则(RFC 3261 §15.1.1):
//   - RURI       = 对端 Contact URI(从 INVITE 抠);抠不到则 fallback INVITE.RURI
//   - From       = 200 OK 时填入的 local-To(含我们加的 to-tag)
//   - To         = INVITE 的 From(含对端 from-tag)
//   - Call-ID    = 沿用
//   - CSeq       = 1 BYE(UAS 端独立计数,从 1 起)
//   - Via        = 我们这一跳新生成,branch 全新
//   - Max-Forwards = 70
//   - Route 集合 = INVITE 的 Record-Route 头按原顺序(UAS 视角不反转)
//   - Contact    = 我们 200 OK 时回的 Contact
//
// 发送通道:TLS 用 in.Conn 原连接写回;UDP 走 transport.Send 到 in.From。
func sendUASBye(t sipua.Transport, in sipua.Inbound, invite *sipua.Message,
	localToHeader, uasContact, localIP string, log *slog.Logger, rep *report.Recorder) {

	bye := &sipua.Message{
		IsRequest: true,
		Method:    "BYE",
		RURI:      extractContactURI(invite.Headers.Get("Contact"), invite.RURI),
		Headers:   sipua.NewHeaders(),
	}

	// Via 走我们自己 transport 的协议,sent-by 用 localIP + transport 本地端口
	var localPort int
	switch a := t.LocalAddr().(type) {
	case *net.UDPAddr:
		localPort = a.Port
	case *net.TCPAddr:
		localPort = a.Port
	}
	viaSentBy := localIP
	if localPort > 0 {
		viaSentBy = fmt.Sprintf("%s:%d", localIP, localPort)
	}
	bye.Headers.Add("Via", fmt.Sprintf("SIP/2.0/%s %s;rport;branch=%s",
		strings.ToUpper(t.Proto()), viaSentBy, sipua.Branch()))
	bye.Headers.Add("Max-Forwards", "70")
	for _, rr := range invite.Headers.GetAll("Record-Route") {
		bye.Headers.Add("Route", rr)
	}
	bye.Headers.Add("From", localToHeader)                 // 我方
	bye.Headers.Add("To", invite.Headers.Get("From"))      // 对方
	bye.Headers.Add("Call-ID", invite.Headers.Get("Call-ID"))
	bye.Headers.Add("CSeq", "1 BYE")
	bye.Headers.Add("Contact", uasContact)
	bye.Headers.Add("User-Agent", "dsipper-uas/0.4")

	// dialog mangle 实验通道:env DSIPPER_BYE_MANGLE = callid|fromtag|totag|ruri
	// 用于复现 SBC B2BUA 弄串 leg 三元组导致 pjsip 找不到 dialog 的产线场景。
	if mangle := os.Getenv("DSIPPER_BYE_MANGLE"); mangle != "" {
		mangleBye(bye, mangle, log)
	}

	if rep != nil {
		peer := ""
		if in.From != nil {
			peer = in.From.String()
		}
		rep.Record("TX", bye, peer)
	}
	raw := bye.Build()
	if in.Conn != nil {
		if _, err := in.Conn.Write(raw); err != nil {
			log.Warn("uas BYE write (tls)", "err", err)
		}
		return
	}
	if err := t.Send(raw, in.From); err != nil {
		log.Warn("uas BYE send (udp)", "err", err)
	}
}

// mangleBye 按 env 指令故意弄错 dialog 三元组某字段,用于复现 pjsip dialog miss 场景。
func mangleBye(bye *sipua.Message, mode string, log *slog.Logger) {
	mark := "MANGLED-" + sipua.Branch()[len("z9hG4bK-"):len("z9hG4bK-")+8]
	switch mode {
	case "callid":
		bye.Headers.Set("Call-ID", mark+"@10.211.55.3")
	case "fromtag":
		from := bye.Headers.Get("From")
		if i := strings.Index(from, "tag="); i > 0 {
			bye.Headers.Set("From", from[:i+4]+mark)
		}
	case "totag":
		to := bye.Headers.Get("To")
		if i := strings.Index(to, "tag="); i > 0 {
			bye.Headers.Set("To", to[:i+4]+mark)
		}
	case "ruri":
		bye.RURI = "sip:wrong-ruri@10.99.99.99:9999;transport=tls"
	default:
		log.Warn("unknown DSIPPER_BYE_MANGLE mode", "mode", mode)
		return
	}
	log.Info("BYE mangled", "mode", mode, "mark", mark)
}

// extractContactURI 从 Contact 头抠 SIP URI。
// 支持 "<sip:user@host:port;params>" 和 "sip:user@host:port;params" 两种写法。
// 抠失败返 fallback。
func extractContactURI(contact, fallback string) string {
	contact = strings.TrimSpace(contact)
	if contact == "" {
		return fallback
	}
	if i := strings.Index(contact, "<"); i >= 0 {
		if j := strings.Index(contact[i:], ">"); j > 0 {
			return strings.TrimSpace(contact[i+1 : i+j])
		}
	}
	// 没有尖括号:取分号前(去掉头域参数,但保留 URI 自身参数比较麻烦,
	// 这里折中 — Contact 多数实现都带尖括号,裸 URI 直接整段返回)
	if sp := strings.IndexAny(contact, " \t;,"); sp > 0 && strings.HasPrefix(contact, "sip") {
		// 含分号的话也许是 URI 参数,保留
		if contact[sp] == ';' {
			return contact
		}
		return contact[:sp]
	}
	return contact
}

// buildResponse 通过 echo request 的 Via/From/To/Call-ID/CSeq 拼一条响应。
func buildResponse(req *sipua.Message, code int, reason string) *sipua.Message {
	r := &sipua.Message{
		IsRequest:    false,
		StatusCode:   code,
		ReasonPhrase: reason,
		Headers:      sipua.NewHeaders(),
	}
	for _, h := range []string{"Via", "From", "To", "Call-ID", "CSeq"} {
		for _, v := range req.Headers.GetAll(h) {
			r.Headers.Add(h, v)
		}
	}
	r.Headers.Add("User-Agent", "dsipper-uas/0.4")
	return r
}

// replyStatus 简单回一个无 body 状态码响应。
func replyStatus(t sipua.Transport, in sipua.Inbound, req *sipua.Message, code int, reason string, rep *report.Recorder) {
	r := buildResponse(req, code, reason)
	sendReply(t, in, r, rep)
}

// sendReply 把响应送回:UDP 用 in.From,TLS 必须用原 conn。
// rep 非空时把出栈 message 喂给 recorder。
func sendReply(t sipua.Transport, in sipua.Inbound, r *sipua.Message, rep *report.Recorder) {
	if rep != nil {
		peer := ""
		if in.From != nil {
			peer = in.From.String()
		}
		rep.Record("TX", r, peer)
	}
	raw := r.Build()
	if in.Conn != nil {
		_, _ = in.Conn.Write(raw)
		return
	}
	_ = t.Send(raw, in.From)
}

func localIPForRemote(viaHeader string) string {
	// 简单提取 Via sent-by 来选本机出口 IP
	parts := strings.Fields(viaHeader)
	if len(parts) >= 2 {
		hostPort := parts[1]
		if i := strings.Index(hostPort, ";"); i > 0 {
			hostPort = hostPort[:i]
		}
		if ip, err := sipua.PickLocalIP(hostPort); err == nil {
			return ip
		}
	}
	// fallback
	return "127.0.0.1"
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		cancel()
	}()
	return ctx, cancel
}

// co 把 verbose 等级 + 日志路径转成 logger;默认 tee 到 stderr + cwd 下文件。
// path="" 用 cwd 下 dsipper-<subcmd>-<时间戳>.log;path="-" 只 stderr。
// maxMB > 0 时文件 sink 走 logsink.RotatingFile(达上限滚 .old)。
// onlyFailed=true 时返回的第二个值是 BufHandler,可挂到 recorder.LogCtrl,
// 让 final 成功通的日志被丢弃,失败/退出 pending 才 flush。
func co(v int, path, subcmd string, maxMB int, onlyFailed bool, panelMode bool) (*slog.Logger, *logsink.BufHandler) {
	level := slog.LevelInfo
	if v >= 1 {
		level = slog.LevelDebug
	}
	var out io.Writer = os.Stderr
	if path != "-" {
		p := path
		if p == "" {
			ts := time.Now().Format("20060102-150405")
			p = filepath.Join(".", fmt.Sprintf("dsipper-%s-%s.log", subcmd, ts))
		}
		maxBytes := int64(maxMB) * 1024 * 1024
		rf, err := logsink.NewRotatingFile(p, maxBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARN: open log %s: %v (fallback stderr only)\n", p, err)
		} else {
			fmt.Fprintf(os.Stderr, "log → %s (rotate at %d MB)\n", p, maxMB)
			if panelMode {
				out = rf
			} else {
				out = io.MultiWriter(os.Stderr, rf)
			}
		}
	}
	inner := slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})
	if onlyFailed {
		bh := logsink.NewBufHandler(inner, true)
		return slog.New(bh), bh
	}
	return slog.New(inner), nil
}

