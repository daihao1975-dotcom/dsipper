package cmd

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dsipper/internal/clui"
	"dsipper/internal/media"
	"dsipper/internal/report"
	"dsipper/internal/sdp"
	"dsipper/internal/sipua"
)

// Invite 子命令:发起一通真实呼叫,带 RTP 音频。
// --total > 1 时进 stress 模式:N worker 并发 + CPS 限流 + 共享 Recorder 汇总。
func Invite(args []string) {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	co := AttachCommon(fs)
	to := fs.String("to", "", "Request-URI / To header,例如 sip:1000@sbc.example.com (必填)")
	from := fs.String("from", "", "From URI,默认 sip:dsipper@<localip>")
	user := fs.String("user", "", "Digest 用户名 (有 401 时使用)")
	pass := fs.String("pass", "", "Digest 密码")
	codecName := fs.String("codec", "PCMA", "音频编码: PCMA / PCMU")
	wavFile := fs.String("wav", "", "发送 WAV 文件 (16-bit mono 8kHz);为空则发 440Hz 正弦波")
	tone := fs.Float64("tone", 440, "正弦波频率 Hz (--wav 留空时生效)")
	duration := fs.Duration("duration", 10*time.Second, "通话总时长")
	saveRecv := fs.String("save-recv", "recv.wav", "把收到的 RTP 解码后保存为 WAV;空字符串=不保存(stress 模式下若非空会加 -NNNN 序号后缀)")
	timeout := fs.Duration("timeout", 10*time.Second, "INVITE 单事务超时")
	rtpPortMin := fs.Int("rtp-port-min", 0, "RTP 本地端口下界 (与 --rtp-port-max 同时 >0 时启用)")
	rtpPortMax := fs.Int("rtp-port-max", 0, "RTP 本地端口上界")
	reportPath := fs.String("report", "", "退出时把信令落一份 HTML report 到该路径(目录或具体 .html);空=不生成")
	reportMaxFailed := fs.Int("report-max-failed", 50, "HTML 详情区保留多少条失败通的信令图(成功不展示);超出只计入顶部汇总")

	// stress 模式
	total := fs.Int("total", 1, "压测模式:总呼叫数(默认 1 = 单通)")
	concurrency := fs.Int("concurrency", 1, "压测模式:并发 worker 数")
	cps := fs.Float64("cps", 0, "压测模式:每秒新呼叫数限速 (0 = 不限速,worker 自由跑)")

	// DTMF
	dtmfStr := fs.String("dtmf", "", "通话期发的 DTMF 串(0-9 * # A-D);空=不发")
	dtmfMode := fs.String("dtmf-mode", "rfc4733", "DTMF 模式:rfc4733 (带外 PT101) / inband (带内双音 splice 进 PCM) / both (两路同发)")
	dtmfDelay := fs.Duration("dtmf-delay", 500*time.Millisecond, "通话建立后多久开始发 DTMF")
	dtmfDur := fs.Duration("dtmf-duration", 120*time.Millisecond, "每个 digit 持续时长")
	dtmfGap := fs.Duration("dtmf-gap", 80*time.Millisecond, "digit 之间空隙")

	// Re-INVITE / hold(RFC 3261 §14):通话期发 re-INVITE 切 SDP direction
	holdAfter := fs.Duration("hold-after", 0, "通话建立后 N 秒发 re-INVITE with a=sendonly(进 hold 态);0=不 hold")
	holdDur := fs.Duration("hold-duration", 0, "hold 持续 M 秒后再发 re-INVITE with a=sendrecv(resume);0=进 hold 后不 resume")

	pcapOpts := AttachPcap(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	co.MustValidate()
	if *to == "" {
		fmt.Fprintln(os.Stderr, "ERR: --to 必填")
		os.Exit(2)
	}
	if *total < 1 {
		*total = 1
	}
	if *concurrency < 1 {
		*concurrency = 1
	}
	if *concurrency > *total {
		*concurrency = *total
	}
	stressMode := *total > 1

	// DTMF 校验 + 规范化
	dtmfDigits := media.NormalizeDTMFString(*dtmfStr)
	dtmfWantRFC4733 := false
	dtmfWantInband := false
	if dtmfDigits != "" {
		switch strings.ToLower(*dtmfMode) {
		case "rfc4733", "2833", "outofband", "oob":
			dtmfWantRFC4733 = true
		case "inband", "pcm", "audio":
			dtmfWantInband = true
		case "both", "all":
			dtmfWantRFC4733 = true
			dtmfWantInband = true
		default:
			fmt.Fprintf(os.Stderr, "ERR: --dtmf-mode 必须是 rfc4733 / inband / both,实际 %q\n", *dtmfMode)
			os.Exit(2)
		}
	}
	// stress 模式启 LivePanel,告诉 MakeLogger 别把日志 tee 到 stderr(否则撞面板)
	if stressMode {
		co.PanelMode = true
	}
	log := co.MakeLogger("invite")

	banner := []clui.KV{
		{K: "server", V: clui.Bold(clui.Blue(co.Server)) + clui.Slate(" ("+co.Transport+")")},
		{K: "to", V: clui.Bold(*to)},
		{K: "from", V: defaultStr(*from, clui.Dim("auto"))},
		{K: "codec", V: clui.Green(*codecName)},
		{K: "duration", V: duration.String()},
		{K: "wav", V: defaultStr(*wavFile, clui.Dim(fmt.Sprintf("sine %.0fHz", *tone)))},
		{K: "report", V: defaultStr(*reportPath, clui.Dim("off"))},
	}
	if stressMode {
		banner = append(banner,
			clui.KV{K: "total", V: clui.Bold(fmt.Sprintf("%d", *total))},
			clui.KV{K: "concurrency", V: clui.Green(fmt.Sprintf("%d", *concurrency))},
			clui.KV{K: "cps", V: stressCPSStr(*cps)},
		)
	}
	if dtmfDigits != "" {
		banner = append(banner,
			clui.KV{K: "dtmf", V: clui.Green(dtmfDigits) + clui.Dim(fmt.Sprintf(" (%s, %s/digit +%s gap, after %s)", strings.ToLower(*dtmfMode), *dtmfDur, *dtmfGap, *dtmfDelay))},
		)
	}
	if *holdAfter > 0 {
		desc := fmt.Sprintf("hold after %s", *holdAfter)
		if *holdDur > 0 {
			desc += fmt.Sprintf(", resume after %s", *holdDur)
		}
		banner = append(banner, clui.KV{K: "re-INVITE", V: clui.Green(desc)})
	}
	Banner("invite", banner)

	stopPcap, err := pcapOpts.Start(log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: pcap: %v\n", err)
		os.Exit(1)
	}
	defer stopPcap()

	// 预解 WAV 一次,worker 间共享(避免 N 次 IO)
	var pcmShared []int16
	if *wavFile != "" {
		samples, sr, err := media.ReadWAV16Mono(*wavFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERR: read WAV: %v\n", err)
			os.Exit(1)
		}
		if sr != 8000 {
			fmt.Fprintf(os.Stderr, "WARN: WAV sample rate %d != 8000, may sound off\n", sr)
		}
		pcmShared = samples
	} else {
		pcmShared = media.SineTone(*tone, duration.Seconds(), 8000, 0.3)
	}
	pcmShared = fitPCM(pcmShared, *duration, 8000)

	// 带内 DTMF:在 PCM 顶部 splice 一次,worker 间共享;不修改原数组(SpliceDTMFInband 已做 copy)
	if dtmfWantInband && dtmfDigits != "" {
		delayMs := int(dtmfDelay.Milliseconds())
		durMs := int(dtmfDur.Milliseconds())
		gapMs := int(dtmfGap.Milliseconds())
		pcmShared = media.SpliceDTMFInband(pcmShared, dtmfDigits, 8000, delayMs, durMs, gapMs)
		log.Info("DTMF inband spliced", "digits", dtmfDigits, "delay_ms", delayMs, "dur_ms", durMs, "gap_ms", gapMs)
	}

	// Recorder 启用条件:
	//   - --report 启用 → 同时写 HTML
	//   - --log-only-failed → 内置一个用作 final 信号源
	//   - stress 模式 → 总建一个,用作 panel/summary 的 status 分布数据源(不一定写 HTML)
	var rep *report.Recorder
	wantReport := *reportPath != ""
	if wantReport || co.BufHandler != nil || stressMode {
		title := fmt.Sprintf("dsipper invite report — %s", co.Server)
		if stressMode {
			title = fmt.Sprintf("dsipper stress — %s × %d", co.Server, *total)
		}
		rep = report.New("invite", title)
		rep.MaxFailedKept = *reportMaxFailed
		if co.BufHandler != nil {
			rep.LogCtrl = co.BufHandler
		}
	}

	// 收尾 once:写 HTML + flush buffered logs
	var saveOnce sync.Once
	saveReport := func() {
		saveOnce.Do(func() {
			if rep != nil && wantReport {
				if p, err := rep.SaveHTML(*reportPath); err != nil {
					fmt.Fprintf(os.Stderr, "WARN: save report: %v\n", err)
				} else {
					fmt.Printf("report HTML: %s\n", p)
				}
			}
			if co.BufHandler != nil {
				co.BufHandler.FlushAll()
			}
		})
	}
	defer saveReport()

	// --- 单通模式:直接跑,保持原行为(打印 OK / FAIL 一行,exit code 反映)
	if !stressMode {
		ctx, cancel := context.WithTimeout(context.Background(), *duration+30*time.Second)
		defer cancel()
		p := callParams{
			Log:           log,
			Recorder:      rep,
			CommonOpts:    co,
			To:            *to,
			FromTemplate:  *from,
			User:          *user,
			Pass:          *pass,
			CodecName:     *codecName,
			PCM:           pcmShared,
			Duration:      *duration,
			SaveRecvPath:  *saveRecv,
			Timeout:       *timeout,
			RTPPortMin:    *rtpPortMin,
			RTPPortMax:    *rtpPortMax,
		}
		if dtmfWantRFC4733 {
			p.DTMFDigits = dtmfDigits
			p.DTMFDelay = *dtmfDelay
			p.DTMFPerDigit = *dtmfDur
			p.DTMFGap = *dtmfGap
		}
		p.HoldAfter = *holdAfter
		p.HoldDuration = *holdDur
		res := runInviteOnce(ctx, p)
		if rep != nil && res.WallDur > 0 {
			rep.AddWallDuration(res.WallDur)
		}
		if res.Err != nil || (res.Status != 0 && res.Status >= 300) {
			if res.Status != 0 {
				fmt.Printf("FAIL: %d\n", res.Status)
			} else {
				fmt.Printf("FAIL: %v\n", res.Err)
			}
			saveReport()
			os.Exit(1)
		}
		fmt.Printf("OK: call %s,RTP tx=%d pkts/%d B  rx=%d pkts/%d B\n",
			res.CallDur, res.TxPkts, res.TxBytes, res.RxPkts, res.RxBytes)
		if res.SavedWAV != "" {
			fmt.Printf("recv WAV: %s\n", res.SavedWAV)
		}
		if res.RxDTMF != "" {
			fmt.Printf("RX DTMF (RFC 4733): %s\n", res.RxDTMF)
		}
		return
	}

	// --- stress 模式:worker pool + CPS 限速 + 汇总 ---
	sp := stressParams{
		Total:        *total,
		Concurrency:  *concurrency,
		CPS:          *cps,
		To:           *to,
		From:         *from,
		User:         *user,
		Pass:         *pass,
		CodecName:    *codecName,
		Duration:     *duration,
		SaveRecvBase: *saveRecv,
		Timeout:      *timeout,
		RTPPortMin:   *rtpPortMin,
		RTPPortMax:   *rtpPortMax,
	}
	if dtmfWantRFC4733 {
		sp.DTMFDigits = dtmfDigits
		sp.DTMFDelay = *dtmfDelay
		sp.DTMFPerDigit = *dtmfDur
		sp.DTMFGap = *dtmfGap
	}
	sp.HoldAfter = *holdAfter
	sp.HoldDuration = *holdDur
	runStress(log, co, rep, pcmShared, sp)
}

// callParams 是 runInviteOnce 的入参 — 每通呼叫所需的全部上下文。
type callParams struct {
	Log      *slog.Logger
	Recorder *report.Recorder // 可空;非空时事件汇入共享 recorder
	CommonOpts *CommonOpts

	To           string
	FromTemplate string // 空→sip:dsipper@<localip>
	User, Pass   string

	CodecName    string
	PCM          []int16 // 待发送 PCM(已 fit 到 Duration,带内 DTMF 已 splice 完)
	Duration     time.Duration
	SaveRecvPath string // 空=不保存
	Timeout      time.Duration
	RTPPortMin   int
	RTPPortMax   int

	// RFC 4733 DTMF:非空 digits 触发通话开始 DTMFDelay 后启异步 goroutine 走 PT 101 流
	DTMFDigits    string
	DTMFDelay     time.Duration
	DTMFPerDigit  time.Duration
	DTMFGap       time.Duration

	// Re-INVITE / hold:HoldAfter>0 时通话建立后 N 秒发 re-INVITE a=sendonly;
	// HoldDuration>0 时再 M 秒后发 re-INVITE a=sendrecv 恢复。
	HoldAfter    time.Duration
	HoldDuration time.Duration
}

type callResult struct {
	CallID   string
	Status   int    // SIP final status;0 = 传输错误 / 超时
	TxPkts   uint64
	RxPkts   uint64
	TxBytes  uint64
	RxBytes  uint64
	CallDur  time.Duration
	WallDur  time.Duration // INVITE 起到 BYE 完结
	SavedWAV string        // 落盘 WAV 路径,空=未保存
	RxDTMF   string        // 入站 DTMF (RFC 4733) digits 字符串
	Err      error
}

// runInviteOnce 执行一通完整呼叫(INVITE → SDP → RTP → BYE)。
// 每次调用自建 transport / UAC / RTP session(独立 socket 端口),适合并发。
// 不调用 os.Exit;所有错误通过 callResult.Err 返回。
func runInviteOnce(ctx context.Context, p callParams) callResult {
	res := callResult{}

	t, err := p.CommonOpts.MakeTransport()
	if err != nil {
		res.Err = fmt.Errorf("transport: %w", err)
		return res
	}
	defer t.Close()

	localIP, err := sipua.PickLocalIP(p.CommonOpts.Server)
	if err != nil {
		res.Err = fmt.Errorf("pick local IP: %w", err)
		return res
	}
	uac := sipua.NewUAC(t, p.CommonOpts.Server, localIP, p.Log)
	if p.Recorder != nil {
		uac.Recorder = p.Recorder
	}
	res.CallID = uac.CallID

	fromURI := p.FromTemplate
	if fromURI == "" {
		fromURI = fmt.Sprintf("sip:dsipper@%s", localIP)
	}

	codecObj := sdp.PCMA
	if strings.EqualFold(p.CodecName, "PCMU") {
		codecObj = sdp.PCMU
	}
	var rtp *media.RTPSession
	if p.RTPPortMin > 0 && p.RTPPortMax > 0 {
		rtp, err = media.NewRTPSessionInRange(localIP, p.RTPPortMin, p.RTPPortMax, uint8(codecObj.PT), codecObj.Name)
	} else {
		rtp, err = media.NewRTPSession(localIP, uint8(codecObj.PT), codecObj.Name)
	}
	if err != nil {
		res.Err = fmt.Errorf("rtp: %w", err)
		return res
	}
	defer rtp.Close()

	dst, err := uac.ResolveServer()
	if err != nil {
		res.Err = fmt.Errorf("resolve: %w", err)
		return res
	}

	wallStart := time.Now()

	build := func(authHeader, authHeaderName string) *sipua.Message {
		req := uac.BuildRequest("INVITE", p.To, fromURI, p.To)
		req.Headers.Add("Contact", uac.LocalContact(sipua.ExtractSIPUser(fromURI)))
		req.Headers.Add("Allow", "INVITE,ACK,CANCEL,BYE,OPTIONS,UPDATE")
		req.Headers.Add("Content-Type", "application/sdp")
		offer := sdp.Offer{
			SessionID:  uint64(rand.Uint32()),
			SessionVer: uint64(rand.Uint32()),
			Username:   "dsipper",
			Origin:     localIP,
			ConnIP:     localIP,
			AudioPort:  rtp.LocalPort(),
			Codecs:     []sdp.Codec{codecObj, sdp.TelephoneEvent},
		}
		req.Body = []byte(offer.Build())
		if authHeader != "" {
			req.Headers.Add(authHeaderName, authHeader)
		}
		return req
	}

	resps, err := uac.SendRequest(ctx, build("", ""), dst, p.Timeout)
	if err != nil {
		res.Err = fmt.Errorf("INVITE: %w", err)
		res.WallDur = time.Since(wallStart)
		return res
	}
	final := resps[len(resps)-1]

	if final.StatusCode == 401 || final.StatusCode == 407 {
		challengeHdr, respHdr := "WWW-Authenticate", "Authorization"
		if final.StatusCode == 407 {
			challengeHdr, respHdr = "Proxy-Authenticate", "Proxy-Authorization"
		}
		ch, err := sipua.ParseDigestChallenge(final.Headers.Get(challengeHdr))
		if err != nil {
			res.Err = fmt.Errorf("parse challenge: %w", err)
			res.Status = final.StatusCode
			res.WallDur = time.Since(wallStart)
			return res
		}
		auth, err := sipua.BuildDigestResponse(ch, "INVITE", p.To, p.User, p.Pass, 1)
		if err != nil {
			res.Err = fmt.Errorf("digest: %w", err)
			res.Status = final.StatusCode
			res.WallDur = time.Since(wallStart)
			return res
		}
		resps, err = uac.SendRequest(ctx, build(auth, respHdr), dst, p.Timeout)
		if err != nil {
			res.Err = fmt.Errorf("INVITE auth: %w", err)
			res.WallDur = time.Since(wallStart)
			return res
		}
		final = resps[len(resps)-1]
	}

	res.Status = final.StatusCode
	if final.StatusCode != 200 {
		if final.StatusCode >= 300 {
			ackInvite(uac, final, p.To, dst)
		}
		res.WallDur = time.Since(wallStart)
		return res
	}

	answer, err := sdp.Parse(string(final.Body))
	if err != nil {
		res.Err = fmt.Errorf("SDP parse: %w", err)
		res.WallDur = time.Since(wallStart)
		return res
	}
	if err := rtp.SetRemote(answer.ConnIP, answer.AudioPort); err != nil {
		res.Err = fmt.Errorf("rtp remote: %w", err)
		res.WallDur = time.Since(wallStart)
		return res
	}
	p.Log.Info("CALL ESTABLISHED",
		"call-id", uac.CallID,
		"remote-rtp", fmt.Sprintf("%s:%d", answer.ConnIP, answer.AudioPort),
		"codec", answer.Codec.Name,
	)

	ackInvite(uac, final, p.To, dst)

	rtpCtx, rtpCancel := context.WithTimeout(ctx, p.Duration)
	defer rtpCancel()

	// in-dialog dispatcher:接住对端 BYE / INFO / UPDATE,在原 socket / TLS conn 回 200 OK。
	// re-INVITE worker 启用时(reinviteActive=true),把响应路由到 reinviteResp 让 worker 拿到。
	var (
		remoteByeMu       sync.Mutex
		remoteByeSeen     bool
		reinviteActive    atomic.Bool
		reinviteResp      = make(chan *sipua.Message, 16)
	)
	go func() {
		for {
			select {
			case <-rtpCtx.Done():
				return
			case in, ok := <-t.Recv():
				if !ok {
					return
				}
				m, perr := sipua.Parse(in.Data)
				if perr != nil {
					continue
				}
				// 响应路由到 re-INVITE worker(若 active)
				if !m.IsRequest {
					if reinviteActive.Load() && m.Headers.Get("Call-ID") == uac.CallID {
						select {
						case reinviteResp <- m:
						default:
						}
					}
					continue
				}
				if m.Headers.Get("Call-ID") != uac.CallID {
					continue
				}
				switch m.Method {
				case "BYE", "CANCEL":
					if p.Recorder != nil {
						p.Recorder.Record("RX", m, "")
					}
					reply := buildResponse(m, 200, "OK")
					if p.Recorder != nil {
						p.Recorder.Record("TX", reply, "")
					}
					raw := reply.Build()
					if in.Conn != nil {
						_, _ = in.Conn.Write(raw)
					} else {
						_ = t.Send(raw, in.From)
					}
					p.Log.Info("RX BYE → 200 OK",
						"call-id", m.Headers.Get("Call-ID"),
						"cseq", m.Headers.Get("CSeq"))
					remoteByeMu.Lock()
					remoteByeSeen = true
					remoteByeMu.Unlock()
					rtpCancel()
					return
				case "ACK":
					// noop
				default:
					if p.Recorder != nil {
						p.Recorder.Record("RX", m, "")
					}
					reply := buildResponse(m, 200, "OK")
					if p.Recorder != nil {
						p.Recorder.Record("TX", reply, "")
					}
					raw := reply.Build()
					if in.Conn != nil {
						_, _ = in.Conn.Write(raw)
					} else {
						_ = t.Send(raw, in.From)
					}
				}
			}
		}
	}()

	go rtp.Recv(rtpCtx, p.Log)

	// RFC 4733 DTMF goroutine — 通话建立后 DTMFDelay 启动,与主语音流共享 RTPSession
	// (Send 期间会被 dtmfActive 标志静音,wire 上 DTMF 包独占)。
	if p.DTMFDigits != "" {
		go func() {
			select {
			case <-rtpCtx.Done():
				return
			case <-time.After(p.DTMFDelay):
			}
			perMs := int(p.DTMFPerDigit.Milliseconds())
			gapMs := int(p.DTMFGap.Milliseconds())
			if err := rtp.SendDTMF(rtpCtx, p.DTMFDigits, perMs, gapMs); err != nil && rtpCtx.Err() == nil {
				p.Log.Warn("dtmf send", "err", err)
			} else {
				p.Log.Info("DTMF sent (RFC 4733)", "digits", p.DTMFDigits, "call-id", uac.CallID)
			}
		}()
	}

	// re-INVITE / hold goroutine:HoldAfter 后发 a=sendonly,HoldDuration 后发 a=sendrecv 恢复。
	// 复用同一 RTP socket(m=audio port 不变),走 dispatcher 路由响应。
	doReInvite := func(dir sdp.MediaDirection) {
		req := uac.BuildRequest("INVITE", p.To, fromURI, p.To)
		// in-dialog re-INVITE 必须带初始 200 OK 给的 to-tag
		req.Headers.Set("To", final.Headers.Get("To"))
		req.Headers.Add("Contact", uac.LocalContact(sipua.ExtractSIPUser(fromURI)))
		req.Headers.Add("Allow", "INVITE,ACK,CANCEL,BYE,OPTIONS,UPDATE")
		req.Headers.Add("Content-Type", "application/sdp")
		offer := sdp.Offer{
			SessionID:  uint64(rand.Uint32()),
			SessionVer: uint64(rand.Uint32()),
			Username:   "dsipper",
			Origin:     localIP,
			ConnIP:     localIP,
			AudioPort:  rtp.LocalPort(),
			Codecs:     []sdp.Codec{codecObj, sdp.TelephoneEvent},
			Direction:  dir,
		}
		req.Body = []byte(offer.Build())

		reinviteActive.Store(true)
		defer reinviteActive.Store(false)
		// 清空残留响应
		for {
			select {
			case <-reinviteResp:
			default:
				goto sendIt
			}
		}
	sendIt:
		if p.Recorder != nil {
			p.Recorder.Record("TX", req, "")
		}
		raw := req.Build()
		if err := t.Send(raw, dst); err != nil {
			p.Log.Warn("re-INVITE send", "err", err, "dir", string(dir))
			return
		}
		p.Log.Info("re-INVITE TX", "dir", string(dir), "call-id", uac.CallID)

		deadline := time.After(p.Timeout)
		for {
			select {
			case <-rtpCtx.Done():
				return
			case <-deadline:
				p.Log.Warn("re-INVITE timeout", "dir", string(dir))
				return
			case m := <-reinviteResp:
				if p.Recorder != nil {
					p.Recorder.Record("RX", m, "")
				}
				if m.StatusCode < 200 {
					continue
				}
				if m.StatusCode == 200 {
					ackInvite(uac, m, p.To, dst)
					p.Log.Info("re-INVITE ACK", "dir", string(dir), "status", 200)
				} else {
					p.Log.Warn("re-INVITE non-2xx", "status", m.StatusCode, "dir", string(dir))
				}
				return
			}
		}
	}
	if p.HoldAfter > 0 {
		go func() {
			select {
			case <-rtpCtx.Done():
				return
			case <-time.After(p.HoldAfter):
			}
			doReInvite(sdp.DirSendOnly)
			if p.HoldDuration > 0 {
				select {
				case <-rtpCtx.Done():
					return
				case <-time.After(p.HoldDuration):
				}
				doReInvite(sdp.DirSendRecv)
			}
		}()
	}

	if err := rtp.Send(rtpCtx, p.PCM); err != nil && rtpCtx.Err() == nil {
		p.Log.Warn("rtp send", "err", err)
	}

	remoteByeMu.Lock()
	skipLocalBye := remoteByeSeen
	remoteByeMu.Unlock()
	if !skipLocalBye {
		bye := uac.BuildRequest("BYE", p.To, fromURI, p.To)
		if tt := final.ToTag(); tt != "" {
			_ = tt
			bye.Headers.Set("To", final.Headers.Get("To"))
		}
		bye.Headers.Add("Contact", uac.LocalContact("dsipper"))
		bresps, err := uac.SendRequest(ctx, bye, dst, p.Timeout)
		if err != nil {
			p.Log.Warn("BYE", "err", err)
		} else {
			p.Log.Info("BYE done", "status", bresps[len(bresps)-1].StatusCode)
		}
	} else {
		p.Log.Info("skip UAC BYE (remote BYE first)", "call-id", uac.CallID)
	}

	tx, rx, txB, rxB := rtp.Stats()
	res.TxPkts, res.RxPkts, res.TxBytes, res.RxBytes = tx, rx, txB, rxB
	res.CallDur = p.Duration
	res.WallDur = time.Since(wallStart)
	res.RxDTMF = rtp.RxDTMF()
	if p.Recorder != nil {
		p.Recorder.SetRTP(uac.CallID, tx, rx, txB, rxB)
	}
	if p.SaveRecvPath != "" {
		if err := rtp.DumpWAV(p.SaveRecvPath); err != nil {
			p.Log.Warn("save recv WAV", "err", err)
		} else {
			res.SavedWAV = p.SaveRecvPath
		}
	}
	return res
}

// --- stress orchestrator ---

type stressParams struct {
	Total        int
	Concurrency  int
	CPS          float64
	To           string
	From         string
	User, Pass   string
	CodecName    string
	Duration     time.Duration
	SaveRecvBase string // 非空 → 每通 base-NNNN.wav;空 → 不保存
	Timeout      time.Duration
	RTPPortMin   int
	RTPPortMax   int

	DTMFDigits   string
	DTMFDelay    time.Duration
	DTMFPerDigit time.Duration
	DTMFGap      time.Duration

	HoldAfter    time.Duration
	HoldDuration time.Duration
}

func runStress(log *slog.Logger, co *CommonOpts, rep *report.Recorder, pcm []int16, p stressParams) {
	var (
		launched atomic.Int64
		done     atomic.Int64
		okCnt    atomic.Int64
		failCnt  atomic.Int64

		latMu sync.Mutex
		lats  []time.Duration // 成功通 wall duration,用于 p50/p95
	)
	errCounts := newErrBucket()

	jobs := make(chan int, p.Concurrency*2)
	var wg sync.WaitGroup

	// CPS 限速器:cps>0 时每通 launch 前等 token
	var tickC <-chan time.Time
	if p.CPS > 0 {
		interval := time.Duration(float64(time.Second) / p.CPS)
		if interval < time.Millisecond {
			interval = time.Millisecond
		}
		tk := time.NewTicker(interval)
		defer tk.Stop()
		tickC = tk.C
	}

	// 实时面板:多行 LivePanel,1Hz 刷;非 tty 时 LivePanel.Start 静默 noop。
	panelStart := time.Now()
	panel := clui.NewLivePanel(os.Stderr, "invite stress", time.Second, func() []clui.KV {
		l := launched.Load()
		d := done.Load()
		ok := okCnt.Load()
		fail := failCnt.Load()
		inflight := l - d
		el := time.Since(panelStart).Seconds()
		if el < 0.001 {
			el = 0.001
		}

		// p50 / p95 wall(读 lats 走 latMu,copy 后再 sort)
		var p50, p95 time.Duration
		latMu.Lock()
		if n := len(lats); n > 0 {
			s := make([]time.Duration, n)
			copy(s, lats)
			latMu.Unlock()
			sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
			p50 = s[n*50/100]
			p95 = s[n*95/100]
			if p95 == 0 {
				p95 = s[n-1]
			}
		} else {
			latMu.Unlock()
		}

		// ETA:剩余 (total - done) 按当前 cps 估
		etaStr := clui.Dim("—")
		if d > 0 && int64(p.Total) > d {
			eta := time.Duration(float64(int64(p.Total)-d)/float64(d)*el) * time.Second
			etaStr = eta.Round(time.Second).String()
		}

		bar := clui.ProgressBar(int(d), p.Total, 24)
		rows := []clui.KV{
			{K: "progress", V: fmt.Sprintf("%s %s",
				bar,
				clui.Bold(fmt.Sprintf("%d/%d", d, p.Total)) + clui.Dim(fmt.Sprintf("  %.0f%%", float64(d)/float64(p.Total)*100)))},
			{K: "launched", V: clui.Bold(clui.Blue(fmt.Sprintf("%d", l))) +
				clui.Dim("  inflight ") + clui.Bold(clui.Yellow(fmt.Sprintf("%d", inflight))) +
				clui.Dim(fmt.Sprintf("  workers %d", p.Concurrency))},
			{K: "ok / fail", V: clui.Bold(clui.Green(fmt.Sprintf("%d ✓", ok))) +
				"   " + clui.Bold(clui.Red(fmt.Sprintf("%d ✗", fail)))},
			{K: "cps", V: clui.Yellow(fmt.Sprintf("%.2f", float64(d)/el)) +
				clui.Dim(fmt.Sprintf("  target %s", stressCPSStrPlain(p.CPS)))},
			{K: "wall", V: fmt.Sprintf("p50 %s   p95 %s",
				clui.Bold(p50.Round(time.Millisecond).String()),
				clui.Bold(p95.Round(time.Millisecond).String()))},
		}
		if rep != nil {
			if line := statusDistLine(rep.Snapshot().Status); line != "" {
				rows = append(rows, clui.KV{K: "status", V: line})
			}
		}
		rows = append(rows, clui.KV{K: "eta", V: etaStr + clui.Dim("  elapsed ") + time.Since(panelStart).Round(time.Second).String()})
		return rows
	})
	panel.Start()
	defer panel.Stop()

	// worker
	for i := 0; i < p.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for idx := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), p.Duration+30*time.Second)
				cp := callParams{
					Log:          log,
					Recorder:     rep,
					CommonOpts:   co,
					To:           p.To,
					FromTemplate: p.From,
					User:         p.User,
					Pass:         p.Pass,
					CodecName:    p.CodecName,
					PCM:          pcm,
					Duration:     p.Duration,
					Timeout:      p.Timeout,
					RTPPortMin:   p.RTPPortMin,
					RTPPortMax:   p.RTPPortMax,
				}
				if p.SaveRecvBase != "" {
					cp.SaveRecvPath = indexedWAV(p.SaveRecvBase, idx)
				}
				if p.DTMFDigits != "" {
					cp.DTMFDigits = p.DTMFDigits
					cp.DTMFDelay = p.DTMFDelay
					cp.DTMFPerDigit = p.DTMFPerDigit
					cp.DTMFGap = p.DTMFGap
				}
				cp.HoldAfter = p.HoldAfter
				cp.HoldDuration = p.HoldDuration
				res := runInviteOnce(ctx, cp)
				cancel()
				done.Add(1)
				if rep != nil && res.WallDur > 0 {
					rep.AddWallDuration(res.WallDur)
				}
				if res.Status == 200 && res.Err == nil {
					okCnt.Add(1)
					latMu.Lock()
					lats = append(lats, res.WallDur)
					latMu.Unlock()
				} else {
					failCnt.Add(1)
					// 错误聚合:status>0 用 "N <reason>" 形式;否则用 err.Error()
					switch {
					case res.Status >= 300:
						errCounts.add(fmt.Sprintf("SIP %d", res.Status))
					case res.Err != nil:
						errCounts.add(res.Err.Error())
					default:
						errCounts.add("unknown")
					}
					log.Warn("call failed",
						"idx", idx,
						"call-id", res.CallID,
						"status", res.Status,
						"err", res.Err)
				}
			}
		}(i)
	}

	// dispatcher
	start := time.Now()
	for i := 1; i <= p.Total; i++ {
		if tickC != nil {
			<-tickC
		}
		launched.Add(1)
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	panel.Stop()
	elapsed := time.Since(start)

	// 汇总
	ok := okCnt.Load()
	fail := failCnt.Load()
	p50, p95 := percentiles(lats)
	rows := []clui.KV{
		{K: "total", V: clui.Bold(fmt.Sprintf("%d", p.Total))},
		{K: "ok", V: clui.Bold(clui.Green(fmt.Sprintf("%d ✓", ok)))},
		{K: "fail", V: clui.Bold(clui.Red(fmt.Sprintf("%d ✗", fail)))},
		{K: "elapsed", V: elapsed.Round(time.Millisecond).String()},
		{K: "actual cps", V: clui.Yellow(fmt.Sprintf("%.2f", float64(p.Total)/elapsed.Seconds()))},
		{K: "p50 wall", V: p50.Round(time.Millisecond).String()},
		{K: "p95 wall", V: p95.Round(time.Millisecond).String()},
	}
	if rep != nil {
		if line := statusDistLine(rep.Snapshot().Status); line != "" {
			rows = append(rows, clui.KV{K: "status", V: line})
		}
	}
	// 错误 top-3(按 error 字符串聚合)
	if errs := topErrors(errCounts.snapshot(), 3); errs != "" {
		rows = append(rows, clui.KV{K: "err top", V: errs})
	}
	fmt.Println(clui.BannerBox("stress summary", rows))

	if fail > 0 {
		os.Exit(1)
	}
}

// stressCPSStrPlain 给 panel/box dim 段拼接用,不带色。
func stressCPSStrPlain(c float64) string {
	if c <= 0 {
		return "unbounded"
	}
	return fmt.Sprintf("%.1f", c)
}

// statusDistLine 把 status code → count 渲染成 inline 串("200 ✓ 25  486 ✗ 3  503 ✗ 1")。
// 2xx 绿勾,≥300 红叉,1xx 灰点。按 count 降序,最多展示 6 项。
func statusDistLine(st map[int]int64) string {
	if len(st) == 0 {
		return ""
	}
	type kv struct {
		code int
		cnt  int64
	}
	pairs := make([]kv, 0, len(st))
	for c, n := range st {
		pairs = append(pairs, kv{c, n})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].cnt > pairs[j].cnt })
	if len(pairs) > 6 {
		pairs = pairs[:6]
	}
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteString("  ")
		}
		sym, color := "·", clui.Slate
		switch {
		case p.code >= 200 && p.code < 300:
			sym, color = "✓", clui.Green
		case p.code >= 300:
			sym, color = "✗", clui.Red
		}
		b.WriteString(fmt.Sprintf("%s %s %s",
			clui.Bold(color(fmt.Sprintf("%d", p.code))),
			color(sym),
			clui.Dim(fmt.Sprintf("%d", p.cnt))))
	}
	return b.String()
}

// errBucket 是 thread-safe 错误字符串计数器,workers 写入,panel/summary 读。
type errBucket struct {
	mu sync.Mutex
	m  map[string]int64
}

func newErrBucket() *errBucket { return &errBucket{m: map[string]int64{}} }
func (b *errBucket) add(msg string) {
	if msg == "" {
		return
	}
	b.mu.Lock()
	b.m[msg]++
	b.mu.Unlock()
}
func (b *errBucket) snapshot() map[string]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int64, len(b.m))
	for k, v := range b.m {
		out[k] = v
	}
	return out
}

// topErrors 把 err msg → count 排序后取 top N,渲染成 "msg×cnt; msg×cnt; ..."。
func topErrors(m map[string]int64, n int) string {
	if len(m) == 0 {
		return ""
	}
	type kv struct {
		s   string
		cnt int64
	}
	pairs := make([]kv, 0, len(m))
	for s, c := range m {
		pairs = append(pairs, kv{s, c})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].cnt > pairs[j].cnt })
	if len(pairs) > n {
		pairs = pairs[:n]
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		short := p.s
		if len(short) > 36 {
			short = short[:33] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s%s", clui.Red(short), clui.Dim(fmt.Sprintf("×%d", p.cnt))))
	}
	return strings.Join(parts, clui.Dim("  ;  "))
}

func stressCPSStr(c float64) string {
	if c <= 0 {
		return clui.Dim("unbounded")
	}
	return clui.Green(fmt.Sprintf("%.1f", c))
}

// indexedWAV 把 base 路径转成 base-0001.wav 形式;保留 .wav 扩展名。
func indexedWAV(base string, idx int) string {
	dir := filepath.Dir(base)
	name := filepath.Base(base)
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if ext == "" {
		ext = ".wav"
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%04d%s", stem, idx, ext))
}

func percentiles(ds []time.Duration) (p50, p95 time.Duration) {
	if len(ds) == 0 {
		return 0, 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)*50/100]
	p95 = sorted[len(sorted)*95/100]
	if p95 == 0 && len(sorted) > 0 {
		p95 = sorted[len(sorted)-1]
	}
	return
}

func ackInvite(uac *sipua.UAC, resp *sipua.Message, ruri string, dst net.Addr) {
	ack := &sipua.Message{
		IsRequest: true,
		Method:    "ACK",
		RURI:      ruri,
		Headers:   sipua.NewHeaders(),
	}
	ack.Headers.Add("Via", fmt.Sprintf("SIP/2.0/%s %s;rport;branch=%s",
		strings.ToUpper(uac.T.Proto()), uac.LocalViaHost(), sipua.Branch()))
	ack.Headers.Add("Max-Forwards", "70")
	ack.Headers.Add("From", resp.Headers.Get("From"))
	ack.Headers.Add("To", resp.Headers.Get("To"))
	ack.Headers.Add("Call-ID", resp.Headers.Get("Call-ID"))
	cseqNum, _ := resp.CSeqNumMethod()
	ack.Headers.Add("CSeq", fmt.Sprintf("%d ACK", cseqNum))
	ack.Headers.Add("User-Agent", uac.UserAgent)
	uac.SendRaw(ack, dst)
}

// fitPCM 把 PCM 调整到指定 duration:不够循环,过长截断。
func fitPCM(pcm []int16, dur time.Duration, sampleRate int) []int16 {
	want := int(float64(sampleRate) * dur.Seconds())
	if len(pcm) >= want {
		return pcm[:want]
	}
	out := make([]int16, want)
	for i := 0; i < want; i++ {
		out[i] = pcm[i%len(pcm)]
	}
	return out
}
