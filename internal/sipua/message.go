// Package sipua 是 dsipper 的最小 SIP UA 协议栈。
//
// 设计取舍:
//   - 只覆盖 UAC 行为(REGISTER / INVITE / OPTIONS / BYE / ACK)与 UAS 接听
//   - 解析时大小写不敏感的 header 名,保留首次出现的 raw value 顺序
//   - Build 时不做严格 RFC 校验,假设调用方传入合法值
//   - Content-Length 必填,TLS 帧解析必须依赖它
package sipua

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// Message 既可以是请求也可以是响应。
type Message struct {
	IsRequest bool

	// 请求字段
	Method string
	RURI   string

	// 响应字段
	StatusCode   int
	ReasonPhrase string

	// 共用
	Version string
	Headers Headers
	Body    []byte
}

// Headers 是 case-insensitive 的多值 header map。
// 内部 key 以小写存,保留每个 header 的多次出现顺序。
type Headers struct {
	keys   []string   // 出现顺序去重后的 key 列表(小写)
	values map[string][]string
}

func NewHeaders() Headers {
	return Headers{values: map[string][]string{}}
}

func (h *Headers) Add(name, value string) {
	if h.values == nil {
		h.values = map[string][]string{}
	}
	k := strings.ToLower(name)
	if _, ok := h.values[k]; !ok {
		h.keys = append(h.keys, k)
	}
	h.values[k] = append(h.values[k], value)
}

func (h *Headers) Set(name, value string) {
	k := strings.ToLower(name)
	if _, ok := h.values[k]; !ok {
		h.keys = append(h.keys, k)
	} else {
		h.values[k] = h.values[k][:0]
	}
	h.values[k] = append(h.values[k], value)
}

func (h *Headers) Get(name string) string {
	v := h.values[strings.ToLower(name)]
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (h *Headers) GetAll(name string) []string {
	return h.values[strings.ToLower(name)]
}

func (h *Headers) Has(name string) bool {
	_, ok := h.values[strings.ToLower(name)]
	return ok
}

// canonical 把小写 key 还原成 SIP 常见拼写,纯美观,SIP 协议本身大小写不敏感
var canonical = map[string]string{
	"via":               "Via",
	"from":              "From",
	"to":                "To",
	"call-id":           "Call-ID",
	"cseq":              "CSeq",
	"contact":           "Contact",
	"max-forwards":      "Max-Forwards",
	"user-agent":        "User-Agent",
	"content-type":      "Content-Type",
	"content-length":    "Content-Length",
	"expires":           "Expires",
	"allow":             "Allow",
	"supported":         "Supported",
	"authorization":     "Authorization",
	"proxy-authorization": "Proxy-Authorization",
	"www-authenticate":  "WWW-Authenticate",
	"proxy-authenticate": "Proxy-Authenticate",
	"record-route":      "Record-Route",
	"route":             "Route",
}

func canonicalKey(k string) string {
	if v, ok := canonical[k]; ok {
		return v
	}
	// 默认首字母大写各段
	parts := strings.Split(k, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "-")
}

// Build 把 message 序列化为 wire 字节流。自动设 Content-Length。
func (m *Message) Build() []byte {
	var b bytes.Buffer
	if m.IsRequest {
		fmt.Fprintf(&b, "%s %s %s\r\n", m.Method, m.RURI, defaultStr(m.Version, "SIP/2.0"))
	} else {
		fmt.Fprintf(&b, "%s %d %s\r\n", defaultStr(m.Version, "SIP/2.0"), m.StatusCode, m.ReasonPhrase)
	}
	bodyLen := len(m.Body)
	for _, k := range m.Headers.keys {
		if k == "content-length" {
			continue // 我们自己写
		}
		for _, v := range m.Headers.values[k] {
			fmt.Fprintf(&b, "%s: %s\r\n", canonicalKey(k), v)
		}
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", bodyLen)
	b.Write(m.Body)
	return b.Bytes()
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// maxHeaderValueBytes 是单个 header 值(含折叠续行累计)的硬上限。
// 8KB 已足够覆盖最长合法 SIP header(Route 集合 / 多段 contacts);超出视为攻击。
const maxHeaderValueBytes = 8 * 1024

// maxRURIBytes 是 Request-URI 的硬上限。RFC 3261 无强制上限,2KB 已是真实部署极限。
const maxRURIBytes = 2 * 1024

// Parse 解析一条完整的 SIP message。raw 必须是 header+CRLFCRLF+body 完整体。
// 不处理流式拆包,流层调用方负责按 Content-Length 切。
func Parse(raw []byte) (*Message, error) {
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		return nil, fmt.Errorf("malformed: no CRLFCRLF separator")
	}
	headPart := string(raw[:idx])
	body := raw[idx+4:]

	lines := strings.Split(headPart, "\r\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("empty message")
	}

	m := &Message{Headers: NewHeaders(), Body: body}

	// First line: request or response
	first := lines[0]
	if strings.HasPrefix(first, "SIP/") {
		// Response: SIP/2.0 200 OK
		parts := strings.SplitN(first, " ", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("malformed status line: %q", first)
		}
		m.IsRequest = false
		m.Version = parts[0]
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid status code: %w", err)
		}
		m.StatusCode = code
		m.ReasonPhrase = parts[2]
	} else {
		// Request: METHOD RURI SIP/2.0
		parts := strings.SplitN(first, " ", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("malformed request line: %q", first)
		}
		if len(parts[1]) > maxRURIBytes {
			return nil, fmt.Errorf("RURI too long: %d bytes (cap %d)", len(parts[1]), maxRURIBytes)
		}
		m.IsRequest = true
		m.Method = parts[0]
		m.RURI = parts[1]
		m.Version = parts[2]
	}

	// Headers (支持折叠行,单 header 值上限 maxHeaderValueBytes 防 DoS)
	var curName, curValue string
	flush := func() error {
		if curName != "" {
			if len(curValue) > maxHeaderValueBytes {
				return fmt.Errorf("header %q value too long: %d bytes (cap %d)", curName, len(curValue), maxHeaderValueBytes)
			}
			m.Headers.Add(curName, strings.TrimSpace(curValue))
		}
		return nil
	}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			next := curValue + " " + strings.TrimSpace(line)
			if len(next) > maxHeaderValueBytes {
				return nil, fmt.Errorf("header %q folded value too long (cap %d)", curName, maxHeaderValueBytes)
			}
			curValue = next
			continue
		}
		if err := flush(); err != nil {
			return nil, err
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return nil, fmt.Errorf("malformed header: %q", line)
		}
		curName = line[:colon]
		curValue = strings.TrimSpace(line[colon+1:])
	}
	if err := flush(); err != nil {
		return nil, err
	}

	return m, nil
}

// ContentLength 返回 Content-Length header 的整数值,缺省 0。
func (m *Message) ContentLength() int {
	v := m.Headers.Get("Content-Length")
	if v == "" {
		v = m.Headers.Get("l") // SIP compact form
	}
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}

// CSeqNumMethod 抽 CSeq header 的数字与方法。
func (m *Message) CSeqNumMethod() (int, string) {
	parts := strings.Fields(m.Headers.Get("CSeq"))
	if len(parts) < 2 {
		return 0, ""
	}
	n, _ := strconv.Atoi(parts[0])
	return n, parts[1]
}

// FromTag / ToTag 抽 From/To 上的 tag 参数。
func tagOf(hdr string) string {
	for _, p := range strings.Split(hdr, ";") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "tag=") {
			return strings.TrimPrefix(p, "tag=")
		}
	}
	return ""
}

func (m *Message) FromTag() string { return tagOf(m.Headers.Get("From")) }
func (m *Message) ToTag() string   { return tagOf(m.Headers.Get("To")) }
