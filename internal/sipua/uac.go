package sipua

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Recorder 是 UAC 信令事件回调接口。dsipper 在 logSIP / SendRaw 路径上调用,
// 把每条 TX / RX message 喂给实现方(internal/report.Recorder)。
// peer 是对端 host:port 字符串,可空。
type Recorder interface {
	Record(dir string, m *Message, peer string)
}

// UAC 是发起方的状态封装,持有 transport + 共享 dialog 信息。
type UAC struct {
	T          Transport
	Log        *slog.Logger
	UserAgent  string
	LocalIP    string // 本机出口 IP,用于 Via / Contact

	// 同一 dialog 共享:
	CallID  string
	FromTag string
	CSeqNum int

	// 默认目的(REGISTRAR / Outbound proxy 地址),Send 时用
	Server string // host:port

	// 可选信令事件回调,挂上后 logSIP / SendRaw 自动喂事件。
	Recorder Recorder

	// PRACKAuto:收到 1xx with Require:100rel + RSeq 时,SendRequest 自动构造并发出
	// PRACK(RFC 3262 §7.1)。SendRequest 期间只把跟原请求同 CSeq method 的 ≥200
	// 当作 final;PRACK 自身的 2xx 会被路过(不当 final)。
	PRACKAuto bool
}

// NewUAC 简单构造。LocalIP 由调用方决定(可走 RouteIP 工具函数选)。
func NewUAC(t Transport, server, localIP string, log *slog.Logger) *UAC {
	return &UAC{
		T:         t,
		Log:       log,
		UserAgent: "dsipper/0.11.2",
		LocalIP:   localIP,
		CallID:    randID(16) + "@" + localIP,
		FromTag:   randID(8),
		CSeqNum:   0,
		Server:    server,
	}
}

// Branch 给一条事务用的随机 magic-cookie branch。
func Branch() string {
	return "z9hG4bK-" + randID(12)
}

func randID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// localPort 从 transport 的 LocalAddr 提端口,UDP/TCP 都支持;无端口时返 0。
func (u *UAC) localPort() int {
	switch a := u.T.LocalAddr().(type) {
	case *net.UDPAddr:
		return a.Port
	case *net.TCPAddr:
		return a.Port
	}
	return 0
}

// LocalContact 给 Via/Contact 用,根据 transport 协议拼。user 为空时不带 user-part,
// 避免 B2BUA 把这里的 user-part 原样复制到 B-leg Contact 上引起后续 in-dialog 请求自指 SBC。
func (u *UAC) LocalContact(user string) string {
	p := u.localPort()
	host := u.LocalIP
	if p > 0 {
		host = fmt.Sprintf("%s:%d", u.LocalIP, p)
	}
	if user == "" {
		return fmt.Sprintf("<sip:%s;transport=%s>", host, u.T.Proto())
	}
	return fmt.Sprintf("<sip:%s@%s;transport=%s>", user, host, u.T.Proto())
}

// ExtractSIPUser 从 "<sip:user@host;...>" 抽 user 部分;失败返回空串。
func ExtractSIPUser(uri string) string {
	s := strings.TrimSpace(uri)
	s = strings.TrimPrefix(strings.TrimSuffix(s, ">"), "<")
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "sips:")
	s = strings.TrimPrefix(s, "sip:")
	if i := strings.IndexByte(s, '@'); i >= 0 {
		return s[:i]
	}
	return ""
}

// LocalViaHost 是 Via 头里的 sent-by。UDP/TCP/TLS 通用;无端口时只写 IP。
func (u *UAC) LocalViaHost() string {
	p := u.localPort()
	if p == 0 {
		return u.LocalIP
	}
	return fmt.Sprintf("%s:%d", u.LocalIP, p)
}

// BuildRequest 创建一个新事务的 SIP 请求。caller 还可在返回值上加 header 后再 Build()。
func (u *UAC) BuildRequest(method, ruri, fromURI, toURI string) *Message {
	u.CSeqNum++
	branch := Branch()
	via := fmt.Sprintf("SIP/2.0/%s %s;rport;branch=%s",
		strings.ToUpper(u.T.Proto()), u.LocalViaHost(), branch)
	m := &Message{
		IsRequest: true,
		Method:    method,
		RURI:      ruri,
		Headers:   NewHeaders(),
	}
	m.Headers.Add("Via", via)
	m.Headers.Add("Max-Forwards", "70")
	m.Headers.Add("From", fmt.Sprintf("<%s>;tag=%s", fromURI, u.FromTag))
	m.Headers.Add("To", fmt.Sprintf("<%s>", toURI))
	m.Headers.Add("Call-ID", u.CallID)
	m.Headers.Add("CSeq", fmt.Sprintf("%d %s", u.CSeqNum, method))
	m.Headers.Add("User-Agent", u.UserAgent)
	return m
}

// SendRequest 发送 + 等响应,直到拿到 final response (≥200) 或 timeout。
// 返回所有响应序列(可能含一个或多个 1xx + 一个 final)。
//
// PRACKAuto=true 时,1xx 携带 Require:100rel + RSeq 会自动触发 PRACK
// (RFC 3262 §7.1)。PRACK 是新事务,自身的 2xx 不会被当成本请求的 final,
// SendRequest 通过 CSeq method 匹配过滤。
func (u *UAC) SendRequest(ctx context.Context, req *Message, dst net.Addr, timeout time.Duration) ([]*Message, error) {
	raw := req.Build()
	u.logSIP("TX", req)
	if err := u.T.Send(raw, dst); err != nil {
		return nil, fmt.Errorf("transport send: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var resps []*Message
	origMethod := req.Method

	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		select {
		case <-ctx.Done():
			return resps, ctx.Err()
		case <-time.After(remaining):
			return resps, fmt.Errorf("timeout waiting for response")
		case in, ok := <-u.T.Recv():
			if !ok {
				return resps, fmt.Errorf("transport closed")
			}
			m, err := Parse(in.Data)
			if err != nil {
				u.Log.Warn("parse failed", "err", err)
				continue
			}
			if m.IsRequest {
				// glare 处理 (RFC 3261 §15.1.1):同 dialog 的 in-dialog request 必须回 200 OK,
				// 不能像旧版那样 ignore。否则 SBC / 对端发 BYE/UPDATE/INFO 撞上我们自家事务时,
				// 对端永远等不到 200 OK → 看着像"我们没处理 BYE"。a1742c0 的 in-call dispatcher
				// 只覆盖到 rtpCtx 期间,自家 BYE 事务窗口仍裸 SendRequest,现网抓包就
				// 看到"ignored unrelated request method=BYE"导致 SBC 那条 glare BYE 没回 200。
				if m.Headers.Get("Call-ID") == u.CallID {
					reply := buildSimpleResponse(m, 200, "OK")
					raw := reply.Build()
					if in.Conn != nil {
						_, _ = in.Conn.Write(raw)
					} else {
						_ = u.T.Send(raw, in.From)
					}
					u.Log.Info("RX in-dialog "+m.Method+" → 200 OK (glare)",
						"call-id", m.Headers.Get("Call-ID"),
						"cseq", m.Headers.Get("CSeq"))
				} else {
					u.Log.Debug("ignored unrelated request", "method", m.Method,
						"call-id", m.Headers.Get("Call-ID"))
				}
				continue
			}
			u.logSIP("RX", m)
			resps = append(resps, m)
			// 自动 PRACK:1xx with Require:100rel
			if m.StatusCode >= 100 && m.StatusCode < 200 && u.PRACKAuto && needsPRACK(m) {
				if err := u.sendPRACKFor(req, m, dst); err != nil {
					u.Log.Warn("PRACK send", "err", err)
				} else {
					u.Log.Info("PRACK TX", "rseq", m.Headers.Get("RSeq"),
						"call-id", m.Headers.Get("Call-ID"))
				}
				continue
			}
			if m.StatusCode >= 200 {
				// 只有跟原请求同 CSeq method 的 final 才算 final;PRACK/UPDATE 等带 dialog
				// 的 2xx 不能挤掉真正的 final。CSeq 缺失或解析失败时按旧行为返回。
				_, respMethod := m.CSeqNumMethod()
				if respMethod == "" || respMethod == origMethod {
					return resps, nil
				}
			}
		}
	}
	return resps, fmt.Errorf("timeout")
}

// needsPRACK 判断 1xx 是否要求 reliable provisional(RFC 3262):
// 同时具备 RSeq 头 与 Require: 100rel 时返回 true。
func needsPRACK(resp *Message) bool {
	if strings.TrimSpace(resp.Headers.Get("RSeq")) == "" {
		return false
	}
	for _, v := range resp.Headers.GetAll("Require") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "100rel") {
				return true
			}
		}
	}
	return false
}

// sendPRACKFor 给定原请求 + 触发的 1xx,构造并发出 PRACK(新事务,新 CSeq,
// dialog 三元组沿用 1xx 的 From/To/Call-ID,RAck = RSeq + 原请求 CSeq)。
// 不等待响应,fire-and-forget;PRACK 的 2xx 由 SendRequest loop 自然消费。
func (u *UAC) sendPRACKFor(origReq, resp *Message, dst net.Addr) error {
	rseq := strings.TrimSpace(resp.Headers.Get("RSeq"))
	parts := strings.Fields(strings.TrimSpace(origReq.Headers.Get("CSeq")))
	if len(parts) < 2 {
		return fmt.Errorf("bad orig CSeq: %q", origReq.Headers.Get("CSeq"))
	}
	rack := rseq + " " + parts[0] + " " + parts[1]

	target := extractContactURIForPRACK(resp.Headers.Get("Contact"))
	if target == "" {
		target = origReq.RURI
	}

	u.CSeqNum++
	via := fmt.Sprintf("SIP/2.0/%s %s;rport;branch=%s",
		strings.ToUpper(u.T.Proto()), u.LocalViaHost(), Branch())
	p := &Message{
		IsRequest: true,
		Method:    "PRACK",
		RURI:      target,
		Headers:   NewHeaders(),
	}
	p.Headers.Add("Via", via)
	p.Headers.Add("Max-Forwards", "70")
	p.Headers.Add("From", resp.Headers.Get("From"))
	p.Headers.Add("To", resp.Headers.Get("To"))
	p.Headers.Add("Call-ID", resp.Headers.Get("Call-ID"))
	p.Headers.Add("CSeq", fmt.Sprintf("%d PRACK", u.CSeqNum))
	p.Headers.Add("RAck", rack)
	p.Headers.Add("User-Agent", u.UserAgent)
	return u.SendRaw(p, dst)
}

// extractContactURIForPRACK 抠 Contact 头里的 SIP URI。
// "<sip:x@y;p>" → "sip:x@y;p";裸 URI 直接返回。
func extractContactURIForPRACK(c string) string {
	c = strings.TrimSpace(c)
	if c == "" {
		return ""
	}
	if i := strings.Index(c, "<"); i >= 0 {
		if j := strings.Index(c[i:], ">"); j > 0 {
			return strings.TrimSpace(c[i+1 : i+j])
		}
	}
	return c
}

// buildSimpleResponse 镜像请求的 Via/From/To/Call-ID/CSeq 拼一条无 body 响应,用于
// SendRequest 内部 glare 处理(收到 in-dialog request 时回 200 OK)。
func buildSimpleResponse(req *Message, code int, reason string) *Message {
	r := &Message{
		IsRequest:    false,
		StatusCode:   code,
		ReasonPhrase: reason,
		Headers:      NewHeaders(),
	}
	for _, h := range []string{"Via", "From", "To", "Call-ID", "CSeq"} {
		for _, v := range req.Headers.GetAll(h) {
			r.Headers.Add(h, v)
		}
	}
	r.Headers.Add("User-Agent", "dsipper-uac/0.11.2")
	return r
}

// SendRaw 发不等待响应(用于 ACK 等不要响应的请求)。
func (u *UAC) SendRaw(req *Message, dst net.Addr) error {
	u.logSIP("TX", req)
	return u.T.Send(req.Build(), dst)
}

// ResolveServer 把 host:port 字符串解析成 transport 用的 net.Addr。
func (u *UAC) ResolveServer() (net.Addr, error) {
	return ResolveAddr(u.T.Proto(), u.Server)
}

// ResolveAddr 给定 transport 类型,把 "host:port" 解析成对应 net.Addr。
// ws/wss 与 tls/tcp 都走 TCPAddr — WS transport 用 host:port 作为 conns map key。
func ResolveAddr(proto, hostport string) (net.Addr, error) {
	switch proto {
	case "udp":
		return net.ResolveUDPAddr("udp", hostport)
	case "tls", "tcp", "ws", "wss":
		return net.ResolveTCPAddr("tcp", hostport)
	}
	return nil, fmt.Errorf("unknown proto: %s", proto)
}

// logSIP 把一条 SIP message 多行打印,带方向 + first line。
// 同时把事件喂给 Recorder(若挂载)。
// Debug 路径会脱敏 Authorization / Proxy-Authorization / WWW-Authenticate / Proxy-Authenticate
// 的敏感字段(response / nonce / cnonce),防止日志泄露 digest hash 与挑战值。
func (u *UAC) logSIP(dir string, m *Message) {
	if u.Recorder != nil {
		u.Recorder.Record(dir, m, u.Server)
	}
	if u.Log == nil {
		return
	}
	first := ""
	if m.IsRequest {
		first = fmt.Sprintf("%s %s", m.Method, m.RURI)
	} else {
		first = fmt.Sprintf("%d %s", m.StatusCode, m.ReasonPhrase)
	}
	u.Log.Info(dir+" "+first,
		"call-id", m.Headers.Get("Call-ID"),
		"cseq", m.Headers.Get("CSeq"),
	)
	// 完整 message 用 Debug — 在 wire 字节流上脱敏 auth 头
	u.Log.Debug(dir+" raw", "msg", "\n"+redactAuthHeaders(string(m.Build())))
}

// redactAuthHeaders 在 SIP 文本中把 Authorization / WWW-Authenticate 等行的
// response="..." / nonce="..." / cnonce="..." 字段值替换为 <redacted>。
// 设计取舍:用纯文本替换而不是结构化重建,避免影响其它字段;只处理 4 类已知敏感头。
func redactAuthHeaders(raw string) string {
	lines := strings.Split(raw, "\r\n")
	for i, line := range lines {
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		switch name {
		case "authorization", "proxy-authorization", "www-authenticate", "proxy-authenticate":
			lines[i] = line[:colon+1] + " " + redactDigestParams(line[colon+1:])
		}
	}
	return strings.Join(lines, "\r\n")
}

func redactDigestParams(v string) string {
	for _, key := range []string{"response", "nonce", "cnonce"} {
		v = redactParam(v, key)
	}
	return v
}

// redactParam 在 v 中把 `<key>="..."` 或 `<key>=...` 的值替换为 <redacted>。
// 仅匹配作为参数 token 出现的 key(前面允许空白/逗号/Digest 开头),避免误伤实质内容。
func redactParam(v, key string) string {
	low := strings.ToLower(v)
	klow := strings.ToLower(key)
	start := 0
	var out strings.Builder
	for {
		idx := strings.Index(low[start:], klow+"=")
		if idx < 0 {
			out.WriteString(v[start:])
			return out.String()
		}
		abs := start + idx
		// 必须是 token 边界(前一字符是空白 / 逗号 / 行首)
		if abs > 0 {
			prev := v[abs-1]
			if prev != ' ' && prev != '\t' && prev != ',' {
				out.WriteString(v[start : abs+len(key)+1])
				start = abs + len(key) + 1
				continue
			}
		}
		out.WriteString(v[start : abs+len(key)+1])
		// 跳过原值
		valStart := abs + len(key) + 1
		end := valStart
		if end < len(v) && v[end] == '"' {
			// quoted: 找下一个 "
			end++
			for end < len(v) && v[end] != '"' {
				end++
			}
			if end < len(v) {
				end++ // 含闭引号
			}
			out.WriteString(`"<redacted>"`)
		} else {
			// 无引号:直到逗号 / 空白
			for end < len(v) && v[end] != ',' && v[end] != ' ' && v[end] != '\t' {
				end++
			}
			out.WriteString("<redacted>")
		}
		start = end
	}
}

// PickLocalIP 选一个能路由到 server 的本机出口 IP。
func PickLocalIP(serverHostPort string) (string, error) {
	host, port, err := net.SplitHostPort(serverHostPort)
	if err != nil {
		return "", err
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return "", fmt.Errorf("DNS: %w", err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("DNS empty for %s", host)
	}
	// UDP "dial" 不实际发包,只让内核选路由
	c, err := net.Dial("udp", net.JoinHostPort(addrs[0], port))
	if err != nil {
		return "", err
	}
	defer c.Close()
	la := c.LocalAddr().(*net.UDPAddr)
	return la.IP.String(), nil
}
