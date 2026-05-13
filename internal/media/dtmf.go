// DTMF (RFC 4733 / RFC 2833 telephone-event) 与带内双音生成。
//
// 工程师调 SBC / IVR 时常用两种 DTMF 路径:
//
//  1. **带外 RFC 4733** — payload type 101 的特殊 RTP 包,SDP `a=fmtp:101 0-15`
//     声明,接收端解 payload 拿 event/duration/end-flag。SIPp 的 [dtmf_2833] 段同款。
//  2. **带内** — 把 DTMF 双音(行频+列频两个正弦叠加)合成 PCM 直接 mix/替换
//     进主语音流,跟 G.711 数据一起走 PT 0/8。老 PBX / ATA / 真实电话场景常见,
//     部分 SBC 无 RFC 4733 转码能力时只剩这条路。
//
// 两路可同发(`both` 模式),最大化兼容性。
package media

import (
	"fmt"
	"math"
	"strings"
)

// DTMF 行频(rows)与列频(columns),ITU-T Q.23 标准频率。
//
//	1209 1336 1477 1633
//	697   1    2    3    A
//	770   4    5    6    B
//	852   7    8    9    C
//	941   *    0    #    D
var dtmfRowHz = [4]float64{697, 770, 852, 941}
var dtmfColHz = [4]float64{1209, 1336, 1477, 1633}

// DTMFKey 是 16 键 DTMF 表;index 与 RFC 4733 event code 一致(0-15)。
var dtmfKey = [16]byte{'1', '2', '3', 'A',
	'4', '5', '6', 'B',
	'7', '8', '9', 'C',
	'*', '0', '#', 'D'}

// DTMFEventCode 把 ASCII digit 转 RFC 4733 event code (0-15)。
// 不识别返 (0, false)。'a'-'d' 自动大写化。
func DTMFEventCode(ch byte) (byte, bool) {
	if ch >= 'a' && ch <= 'd' {
		ch -= 32
	}
	for i, k := range dtmfKey {
		if k == ch {
			return byte(i), true
		}
	}
	return 0, false
}

// DTMFTonePCM 生成单个 digit 的带内 PCM(双音叠加)。
// amplitude 推荐 0.25(双音叠加后峰值 ~0.5,留 6dB headroom);ITU-T 实际电话振幅
// 约 -3 to -9 dBm0 per tone,这里走工程级"够检测"的振幅,不追求严格合规。
// 不识别 digit 时返回 nil。
func DTMFTonePCM(digit byte, durMs int, sampleRate int, amplitude float64) []int16 {
	idx, ok := DTMFEventCode(digit)
	if !ok {
		return nil
	}
	row := dtmfRowHz[idx/4]
	col := dtmfColHz[idx%4]
	n := durMs * sampleRate / 1000
	out := make([]int16, n)
	twoPiR := 2.0 * math.Pi * row
	twoPiC := 2.0 * math.Pi * col
	for i := 0; i < n; i++ {
		t := float64(i) / float64(sampleRate)
		v := amplitude * (math.Sin(twoPiR*t) + math.Sin(twoPiC*t))
		if v > 1.0 {
			v = 1.0
		} else if v < -1.0 {
			v = -1.0
		}
		out[i] = int16(v * 32767)
	}
	return out
}

// SpliceDTMFInband 把 digits 按 (delayMs, durMs, gapMs) 节奏 splice (覆盖) 进 pcm。
// pcm 不够长则按需补零(silence)。返回新切片,不修改入参(让 stress 模式 worker 间共享 pcm 安全)。
// 不识别的 digit 跳过(不抛错)。
func SpliceDTMFInband(pcm []int16, digits string, sampleRate, delayMs, durMs, gapMs int) []int16 {
	if digits == "" {
		return pcm
	}
	// 估算所需总长 = delay + N * (dur + gap),够就照 pcm 长度,不够则扩到所需长度
	perStep := durMs + gapMs
	tailMs := delayMs + len(digits)*perStep
	tailSamples := tailMs * sampleRate / 1000
	out := make([]int16, len(pcm))
	copy(out, pcm)
	if tailSamples > len(out) {
		extra := make([]int16, tailSamples-len(out))
		out = append(out, extra...)
	}
	cursor := delayMs * sampleRate / 1000
	for i := 0; i < len(digits); i++ {
		tone := DTMFTonePCM(digits[i], durMs, sampleRate, 0.25)
		if tone != nil {
			end := cursor + len(tone)
			if end > len(out) {
				end = len(out)
			}
			copy(out[cursor:end], tone[:end-cursor])
		}
		cursor += perStep * sampleRate / 1000
	}
	return out
}

// BuildDTMFEventPayload 拼一条 RFC 4733 4-byte event payload。
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//	|     event     |E|R| volume    |          duration             |
//	+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
//
// volume 单位 dBm0,典型 -10 即 10(取负数后绝对值);duration 单位 sample @ 8kHz。
func BuildDTMFEventPayload(event byte, end bool, volume byte, duration uint16) []byte {
	b := make([]byte, 4)
	b[0] = event & 0x0F
	b[1] = volume & 0x3F
	if end {
		b[1] |= 0x80 // E bit
	}
	b[2] = byte(duration >> 8)
	b[3] = byte(duration & 0xFF)
	return b
}

// ParseDTMFEventPayload 解析 RFC 4733 4-byte event payload。len < 4 返 err。
func ParseDTMFEventPayload(b []byte) (event byte, end bool, volume byte, duration uint16, err error) {
	if len(b) < 4 {
		err = fmt.Errorf("DTMF payload too short: %d bytes", len(b))
		return
	}
	event = b[0] & 0x0F
	end = b[1]&0x80 != 0
	volume = b[1] & 0x3F
	duration = uint16(b[2])<<8 | uint16(b[3])
	return
}

// DTMFEventASCII 把 event code (0-15) 转回 ASCII digit ('0'-'9','*','#','A'-'D'),
// 用于 log / report 显示。越界返 '?'。
func DTMFEventASCII(event byte) byte {
	if int(event) >= len(dtmfKey) {
		return '?'
	}
	return dtmfKey[event]
}

// NormalizeDTMFString 过滤 digits 字符串,去掉空白与不识别字符,返回净版。
// CLI 用 `--dtmf "1 2 3 #"` 这种带空格写法时方便。
func NormalizeDTMFString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if _, ok := DTMFEventCode(s[i]); ok {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
