// Package sdp 是给 SIP mock client 用的最小 SDP 构造/解析。
// 不追求完整 RFC 4566 兼容,只覆盖 G.711a / G.711u + RFC 2833 telephone-event
// 这种压倒性常见组合。
package sdp

import (
	"fmt"
	"strconv"
	"strings"
)

// Codec PT 编号 + 名称 + 时钟率,RFC 3551 静态 PT 表的子集。
type Codec struct {
	PT     int    // payload type
	Name   string // PCMA / PCMU / telephone-event
	Clock  int    // 8000 / 90000
	Params string // fmtp 内容,可空
}

var (
	PCMA = Codec{PT: 8, Name: "PCMA", Clock: 8000}
	PCMU = Codec{PT: 0, Name: "PCMU", Clock: 8000}
	// RFC 2833 DTMF — 动态 PT 这里固定 101,业界事实标准
	TelephoneEvent = Codec{PT: 101, Name: "telephone-event", Clock: 8000, Params: "0-15"}
)

// MediaDirection 是 SDP a=sendrecv / sendonly / recvonly / inactive 四态。
// 空字符串等同 sendrecv(RFC 4566 默认)。
type MediaDirection string

const (
	DirSendRecv MediaDirection = "sendrecv"
	DirSendOnly MediaDirection = "sendonly"
	DirRecvOnly MediaDirection = "recvonly"
	DirInactive MediaDirection = "inactive"
)

// MirrorDirection 给 UAS 回 200 OK 用,镜像对端 offer direction:
// sendonly → recvonly (对端只发我只收),recvonly → sendonly,inactive → inactive,
// 其他全 sendrecv。
func MirrorDirection(d MediaDirection) MediaDirection {
	switch d {
	case DirSendOnly:
		return DirRecvOnly
	case DirRecvOnly:
		return DirSendOnly
	case DirInactive:
		return DirInactive
	default:
		return DirSendRecv
	}
}

// Offer 是发起方的 SDP 描述。
type Offer struct {
	SessionID  uint64
	SessionVer uint64
	Username   string // o= 字段
	Origin     string // o= 字段的 IP4 地址(本机出口)
	ConnIP     string // c= 字段
	AudioPort  int    // m=audio
	Codecs     []Codec
	Direction  MediaDirection // 空=sendrecv
}

// Build 渲染成 SDP 文本。
func (o Offer) Build() string {
	var b strings.Builder
	user := o.Username
	if user == "" {
		user = "dsipper"
	}
	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=%s %d %d IN IP4 %s\r\n", user, o.SessionID, o.SessionVer, o.Origin)
	fmt.Fprintf(&b, "s=dsipper\r\n")
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", o.ConnIP)
	fmt.Fprintf(&b, "t=0 0\r\n")
	pts := make([]string, 0, len(o.Codecs))
	for _, c := range o.Codecs {
		pts = append(pts, strconv.Itoa(c.PT))
	}
	fmt.Fprintf(&b, "m=audio %d RTP/AVP %s\r\n", o.AudioPort, strings.Join(pts, " "))
	for _, c := range o.Codecs {
		fmt.Fprintf(&b, "a=rtpmap:%d %s/%d\r\n", c.PT, c.Name, c.Clock)
		if c.Params != "" {
			fmt.Fprintf(&b, "a=fmtp:%d %s\r\n", c.PT, c.Params)
		}
	}
	dir := o.Direction
	if dir == "" {
		dir = DirSendRecv
	}
	fmt.Fprintf(&b, "a=%s\r\n", dir)
	fmt.Fprintf(&b, "a=ptime:20\r\n")
	return b.String()
}

// Answer 是从对端 SDP 抽出的关键字段。
type Answer struct {
	ConnIP    string
	AudioPort int
	Codec     Codec          // 对端真正选定的第一个 audio codec
	Direction MediaDirection // 对端 SDP 声明的 direction,默认 sendrecv
}

// Parse 解析对端 SDP,只抽我们关心的字段。
func Parse(body string) (Answer, error) {
	var a Answer
	var rtpmap = make(map[int]Codec)
	var firstPT int = -1
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			a.ConnIP = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))
		case strings.HasPrefix(line, "m=audio "):
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				port, err := strconv.Atoi(parts[1])
				if err != nil {
					return a, fmt.Errorf("invalid m=audio port: %w", err)
				}
				a.AudioPort = port
				if pt, err := strconv.Atoi(parts[3]); err == nil {
					firstPT = pt
				}
			}
		case line == "a=sendrecv":
			a.Direction = DirSendRecv
		case line == "a=sendonly":
			a.Direction = DirSendOnly
		case line == "a=recvonly":
			a.Direction = DirRecvOnly
		case line == "a=inactive":
			a.Direction = DirInactive
		case strings.HasPrefix(line, "a=rtpmap:"):
			rest := strings.TrimPrefix(line, "a=rtpmap:")
			fields := strings.Fields(rest)
			if len(fields) < 2 {
				continue
			}
			pt, err := strconv.Atoi(fields[0])
			if err != nil {
				continue
			}
			nameClock := strings.SplitN(fields[1], "/", 2)
			c := Codec{PT: pt, Name: nameClock[0]}
			if len(nameClock) == 2 {
				c.Clock, _ = strconv.Atoi(nameClock[1])
			}
			rtpmap[pt] = c
		}
	}
	if a.ConnIP == "" || a.AudioPort == 0 {
		return a, fmt.Errorf("SDP 缺 c= 或 m=audio")
	}
	if a.Direction == "" {
		a.Direction = DirSendRecv
	}
	// 优先用 m= 第一个 PT;若 PT < 96 是静态,可不依赖 rtpmap
	if firstPT >= 0 {
		if c, ok := rtpmap[firstPT]; ok {
			a.Codec = c
		} else {
			switch firstPT {
			case 0:
				a.Codec = PCMU
			case 8:
				a.Codec = PCMA
			default:
				a.Codec = Codec{PT: firstPT, Name: "unknown", Clock: 8000}
			}
		}
	}
	return a, nil
}
