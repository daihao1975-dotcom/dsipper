package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"dsipper/internal/clui"
	"dsipper/internal/media"
	"dsipper/internal/sdp"
	"dsipper/internal/sipua"
)

// maxScenarioBytes 是单个 scenario YAML 文件的硬上限(10MB)。
// 防御 attacker 给 dsipper 喂 GB 级 YAML 把进程 OOM。
const maxScenarioBytes = 10 * 1024 * 1024

// maxScenarioSteps 单脚本步骤上限,防退化为 DoS 工具。
const maxScenarioSteps = 10_000

// scenarioFile 是 YAML 顶层结构。Default 段给所有步骤铺底,各步骤可覆盖。
type scenarioFile struct {
	Name    string         `yaml:"name"`
	Default scenarioStep   `yaml:"default"`
	Steps   []scenarioStep `yaml:"steps"`
}

// scenarioStep 是一个动作。Action 必填,其它字段按 action 选择性使用。
//
// 通过把所有可能字段塞进同一 struct 来避免 polymorphic YAML —— 写脚本更方便,
// 代码也少 50%(单 struct,switch on Action)。
type scenarioStep struct {
	Action string `yaml:"action"` // options / register / invite / sleep / log

	// 连接相关(覆盖 Default)
	Server    string `yaml:"server"`
	Transport string `yaml:"transport"`
	Insecure  *bool  `yaml:"insecure"` // ptr 区分"未设"与"显式 false"
	WSPath    string `yaml:"ws-path"`

	// options / register / invite 共用
	To      string        `yaml:"to"`
	From    string        `yaml:"from"`
	Timeout time.Duration `yaml:"timeout"`
	Expect  *int          `yaml:"expect"` // nil = 不检查;否则期望 status code

	// register 独有
	User    string `yaml:"user"`
	Pass    string `yaml:"pass"`
	Domain  string `yaml:"domain"`
	Expires int    `yaml:"expires"`

	// invite / sleep 共用:invite 用作通话时长,sleep 用作休眠时长
	Codec    string        `yaml:"codec"`
	Duration time.Duration `yaml:"duration"`

	// log 独有
	Msg string `yaml:"msg"`

	// 流控
	ContinueOnFail bool   `yaml:"continue-on-fail"`
	Label          string `yaml:"label"` // 步骤别名,summary 引用
}

// Scenario 子命令:跑一个 YAML 脚本,按顺序执行步骤,每步可断言 status code。
// 整体 exit code:全部步骤通过 → 0;有失败 → 1。
func Scenario(args []string) {
	fs := flag.NewFlagSet("scenario", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "只打印解析后的步骤,不实际发包")
	verbose := fs.Int("v", 0, "verbose 0/1(debug 含完整 SIP message)")
	logFile := fs.String("log", "-", "日志路径,默认 '-' (stderr only)")
	fs.BoolVar(&Quiet, "quiet", false, "静默模式")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: dsipper scenario [flags] <scenario.yaml>")
		os.Exit(2)
	}
	path := fs.Arg(0)

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: open %s: %v\n", path, err)
		os.Exit(2)
	}
	// 限制读取上限 + 1 字节,触发上限就报错,避免静默截断后解析出半残 YAML。
	data, err := io.ReadAll(io.LimitReader(f, maxScenarioBytes+1))
	f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: read %s: %v\n", path, err)
		os.Exit(2)
	}
	if len(data) > maxScenarioBytes {
		fmt.Fprintf(os.Stderr, "ERR: %s exceeds %d bytes cap\n", path, maxScenarioBytes)
		os.Exit(2)
	}
	var sc scenarioFile
	if err := yaml.Unmarshal(data, &sc); err != nil {
		fmt.Fprintf(os.Stderr, "ERR: parse %s: %v\n", path, err)
		os.Exit(2)
	}
	if len(sc.Steps) == 0 {
		fmt.Fprintln(os.Stderr, "ERR: scenario has no steps")
		os.Exit(2)
	}
	if len(sc.Steps) > maxScenarioSteps {
		fmt.Fprintf(os.Stderr, "ERR: scenario has %d steps, cap %d\n", len(sc.Steps), maxScenarioSteps)
		os.Exit(2)
	}

	// 用 CommonOpts 拼一个 logger;Server 为空时 MakeLogger 不挂 transport,
	// 这里我们直接走 buildLogger 的简化路径 — scenario.go 的 logger 不带文件 rotation
	co := &CommonOpts{Verbose: *verbose, LogFile: *logFile, LogMaxMB: 100}
	log := buildLogger(co, "scenario")

	Banner("scenario", []clui.KV{
		{K: "file", V: clui.Bold(path)},
		{K: "name", V: defaultStr(sc.Name, clui.Dim("(unnamed)"))},
		{K: "steps", V: clui.Bold(fmt.Sprintf("%d", len(sc.Steps)))},
		{K: "default server", V: defaultStr(sc.Default.Server, clui.Dim("(per-step)"))},
		{K: "default transport", V: defaultStr(sc.Default.Transport, clui.Dim("(per-step)"))},
		{K: "dry-run", V: defaultStr(boolStr(*dryRun), clui.Dim("off"))},
	})

	if *dryRun {
		for i, st := range sc.Steps {
			merged := mergeStep(sc.Default, st)
			expStr := "any"
			if merged.Expect != nil {
				expStr = fmt.Sprintf("%d", *merged.Expect)
			}
			fmt.Printf("  [%d] action=%-9s server=%s transport=%s to=%s expect=%s\n",
				i+1, merged.Action, merged.Server, merged.Transport, merged.To, expStr)
		}
		return
	}

	type result struct {
		idx     int
		label   string
		action  string
		status  int    // SIP status,0=N/A
		passed  bool
		errMsg  string
		elapsed time.Duration
	}
	var results []result
	totalStart := time.Now()
	failed := 0

	for i, raw := range sc.Steps {
		st := mergeStep(sc.Default, raw)
		label := st.Label
		if label == "" {
			label = fmt.Sprintf("step%d", i+1)
		}
		stepStart := time.Now()
		status, err := runScenarioStep(log, st)
		elapsed := time.Since(stepStart)
		r := result{idx: i + 1, label: label, action: st.Action, status: status, elapsed: elapsed}
		if err != nil {
			r.errMsg = err.Error()
			r.passed = false
		} else if st.Expect != nil {
			r.passed = status == *st.Expect
			if !r.passed {
				r.errMsg = fmt.Sprintf("expected %d, got %d", *st.Expect, status)
			}
		} else {
			r.passed = err == nil
		}
		results = append(results, r)
		printStepResult(r)
		if !r.passed && !st.ContinueOnFail {
			failed++
			break
		}
		if !r.passed {
			failed++
		}
	}

	// 汇总
	rows := []clui.KV{
		{K: "total", V: clui.Bold(fmt.Sprintf("%d", len(results)))},
		{K: "passed", V: clui.Green(fmt.Sprintf("%d ✓", len(results)-failed))},
		{K: "failed", V: clui.Red(fmt.Sprintf("%d ✗", failed))},
		{K: "elapsed", V: time.Since(totalStart).Round(time.Millisecond).String()},
	}
	fmt.Println(clui.BannerBox("scenario summary", rows))
	if failed > 0 {
		os.Exit(1)
	}
}

// mergeStep 把 default 段铺到 step,step 字段非零则覆盖。
// 用反射不优雅;手写 N 行也够稳。
func mergeStep(d, s scenarioStep) scenarioStep {
	out := s
	if out.Server == "" {
		out.Server = d.Server
	}
	if out.Transport == "" {
		out.Transport = d.Transport
	}
	if out.Insecure == nil {
		out.Insecure = d.Insecure
	}
	if out.WSPath == "" {
		out.WSPath = d.WSPath
	}
	if out.To == "" {
		out.To = d.To
	}
	if out.From == "" {
		out.From = d.From
	}
	if out.Timeout == 0 {
		out.Timeout = d.Timeout
	}
	if out.Codec == "" {
		out.Codec = d.Codec
	}
	if out.Duration == 0 {
		out.Duration = d.Duration
	}
	if out.User == "" {
		out.User = d.User
	}
	if out.Pass == "" {
		out.Pass = d.Pass
	}
	if out.Domain == "" {
		out.Domain = d.Domain
	}
	if out.Expires == 0 {
		out.Expires = d.Expires
	}
	return out
}

// printStepResult 在 stdout 打一行带色彩 + 详情。
func printStepResult(r struct {
	idx     int
	label   string
	action  string
	status  int
	passed  bool
	errMsg  string
	elapsed time.Duration
}) {
	mark, c := "✓", clui.Green
	if !r.passed {
		mark, c = "✗", clui.Red
	}
	statusStr := ""
	if r.status > 0 {
		statusStr = fmt.Sprintf(" status=%d", r.status)
	}
	line := fmt.Sprintf("  [%d] %s %s %s%s  %s",
		r.idx, c(mark), clui.Bold(r.label),
		clui.Dim(r.action), clui.Dim(statusStr),
		clui.Dim(r.elapsed.Round(time.Millisecond).String()))
	if r.errMsg != "" {
		line += "  " + clui.Red(r.errMsg)
	}
	fmt.Println(line)
}

// runScenarioStep 分派到具体 action handler。返回 (status, err)。
// status > 0 时表示拿到了 SIP final;为 0 表示 action 没有 status 概念(sleep / log)。
func runScenarioStep(log *slog.Logger, st scenarioStep) (int, error) {
	switch strings.ToLower(st.Action) {
	case "sleep":
		if st.Duration <= 0 {
			return 0, fmt.Errorf("sleep: duration 必填")
		}
		log.Info("sleep", "duration", st.Duration)
		time.Sleep(st.Duration)
		return 0, nil
	case "log":
		if st.Msg == "" {
			return 0, fmt.Errorf("log: msg 必填")
		}
		log.Info("scenario log", "msg", st.Msg)
		return 0, nil
	case "options":
		return runScenarioOptions(log, st)
	case "register":
		return runScenarioRegister(log, st)
	case "invite":
		return runScenarioInvite(log, st)
	}
	return 0, fmt.Errorf("unknown action: %q", st.Action)
}

// scenarioTransport 按 step 配置建一个一次性 transport。
func scenarioTransport(st scenarioStep) (sipua.Transport, error) {
	insecure := true
	if st.Insecure != nil {
		insecure = *st.Insecure
	}
	co := &CommonOpts{
		Server:    st.Server,
		Transport: st.Transport,
		Insecure:  insecure,
		WSPath:    st.WSPath,
	}
	if co.Transport == "" {
		co.Transport = "udp"
	}
	return co.MakeTransport()
}

func runScenarioOptions(log *slog.Logger, st scenarioStep) (int, error) {
	if st.Server == "" {
		return 0, fmt.Errorf("options: server 必填")
	}
	t, err := scenarioTransport(st)
	if err != nil {
		return 0, fmt.Errorf("transport: %w", err)
	}
	defer t.Close()
	localIP, err := sipua.PickLocalIP(st.Server)
	if err != nil {
		return 0, fmt.Errorf("pick local IP: %w", err)
	}
	uac := sipua.NewUAC(t, st.Server, localIP, log)
	dst, err := uac.ResolveServer()
	if err != nil {
		return 0, err
	}
	ruri := st.To
	if ruri == "" {
		ruri = "sip:" + st.Server
	}
	from := fmt.Sprintf("sip:probe@%s", localIP)
	req := uac.BuildRequest("OPTIONS", ruri, from, ruri)
	req.Headers.Add("Contact", uac.LocalContact("probe"))
	req.Headers.Add("Accept", "application/sdp")
	to := st.Timeout
	if to == 0 {
		to = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), to+time.Second)
	defer cancel()
	resps, err := uac.SendRequest(ctx, req, dst, to)
	if err != nil {
		return 0, err
	}
	return resps[len(resps)-1].StatusCode, nil
}

func runScenarioRegister(log *slog.Logger, st scenarioStep) (int, error) {
	if st.Server == "" || st.User == "" {
		return 0, fmt.Errorf("register: server + user 必填")
	}
	t, err := scenarioTransport(st)
	if err != nil {
		return 0, fmt.Errorf("transport: %w", err)
	}
	defer t.Close()
	localIP, err := sipua.PickLocalIP(st.Server)
	if err != nil {
		return 0, fmt.Errorf("pick local IP: %w", err)
	}
	dom := st.Domain
	if dom == "" {
		h := st.Server
		if i := strings.IndexByte(h, ':'); i > 0 {
			h = h[:i]
		}
		dom = h
	}
	aor := fmt.Sprintf("sip:%s@%s", st.User, dom)
	uac := sipua.NewUAC(t, st.Server, localIP, log)
	uac.FromTag = sipua.Branch()[len("z9hG4bK-"):]
	dst, err := uac.ResolveServer()
	if err != nil {
		return 0, err
	}
	to := st.Timeout
	if to == 0 {
		to = 8 * time.Second
	}
	exp := st.Expires
	if exp == 0 {
		exp = 60
	}
	build := func(authHeader, authHeaderName string) *sipua.Message {
		req := uac.BuildRequest("REGISTER", "sip:"+dom, aor, aor)
		req.Headers.Add("Contact", uac.LocalContact(st.User))
		req.Headers.Add("Expires", fmt.Sprintf("%d", exp))
		req.Headers.Add("Allow", "INVITE,ACK,CANCEL,BYE,OPTIONS")
		if authHeader != "" {
			req.Headers.Add(authHeaderName, authHeader)
		}
		return req
	}
	ctx, cancel := context.WithTimeout(context.Background(), to*3+time.Second)
	defer cancel()
	resps, err := uac.SendRequest(ctx, build("", ""), dst, to)
	if err != nil {
		return 0, err
	}
	final := resps[len(resps)-1]
	if final.StatusCode == 401 || final.StatusCode == 407 {
		challengeHdr, respHdr := "WWW-Authenticate", "Authorization"
		if final.StatusCode == 407 {
			challengeHdr, respHdr = "Proxy-Authenticate", "Proxy-Authorization"
		}
		ch, err := sipua.ParseDigestChallenge(final.Headers.Get(challengeHdr))
		if err != nil {
			return final.StatusCode, fmt.Errorf("parse challenge: %w", err)
		}
		auth, err := sipua.BuildDigestResponse(ch, "REGISTER", "sip:"+dom, st.User, st.Pass, 1)
		if err != nil {
			return final.StatusCode, fmt.Errorf("build digest: %w", err)
		}
		resps, err = uac.SendRequest(ctx, build(auth, respHdr), dst, to)
		if err != nil {
			return 0, err
		}
		final = resps[len(resps)-1]
	}
	return final.StatusCode, nil
}

func runScenarioInvite(log *slog.Logger, st scenarioStep) (int, error) {
	if st.Server == "" || st.To == "" {
		return 0, fmt.Errorf("invite: server + to 必填")
	}
	co := &CommonOpts{
		Server:    st.Server,
		Transport: defaultStrPlain(st.Transport, "udp"),
		WSPath:    st.WSPath,
	}
	if st.Insecure != nil {
		co.Insecure = *st.Insecure
	} else {
		co.Insecure = true
	}
	dur := st.Duration
	if dur == 0 {
		dur = 2 * time.Second
	}
	to := st.Timeout
	if to == 0 {
		to = 10 * time.Second
	}
	codec := st.Codec
	if codec == "" {
		codec = "PCMA"
	}
	pcm := media.SineTone(440, dur.Seconds(), 8000, 0.3)
	pcm = fitPCM(pcm, dur, 8000)
	ctx, cancel := context.WithTimeout(context.Background(), dur+30*time.Second)
	defer cancel()
	res := runInviteOnce(ctx, callParams{
		Log:        log,
		CommonOpts: co,
		To:         st.To,
		FromTemplate: st.From,
		User:       st.User,
		Pass:       st.Pass,
		CodecName:  codec,
		PCM:        pcm,
		Duration:   dur,
		Timeout:    to,
	})
	if res.Err != nil && res.Status == 0 {
		return 0, res.Err
	}
	return res.Status, nil
}

// defaultStrPlain 同 defaultStr 但不带 color codes — 用于 transport/codec 等枚举回退。
func defaultStrPlain(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// 静态分配防 import 失败:确保 invite 路径用到 SineTone 等。
var _ = sdp.PCMA
var _ = media.SineTone
