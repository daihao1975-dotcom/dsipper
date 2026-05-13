// Package media 提供 G.711 alaw/ulaw 编解码、WAV 读写、tone 合成、RTP 收发。
package media

// G.711 编解码 — 完整实现 ITU-T G.711 alaw/ulaw 互转 16-bit PCM。
// 参考 ITU-T G.191 sample 实现,以及 wireshark / ffmpeg 中通用算法。

// LinearToAlaw 把单个 16-bit signed PCM 样本压成 8-bit A-law。
func LinearToAlaw(pcm int16) byte {
	sign := int(0)
	v := int(pcm)
	if v < 0 {
		v = -v - 1
		sign = 0x80
	}
	if v > 32635 {
		v = 32635
	}
	// 找 segment
	exp := 7
	for ; exp > 0; exp-- {
		if v >= (1 << (exp + 3)) {
			break
		}
	}
	mantissa := (v >> (exp + 3)) & 0x0F
	if exp == 0 {
		mantissa = (v >> 4) & 0x0F
	}
	alaw := sign | (exp << 4) | mantissa
	return byte(alaw ^ 0x55)
}

// AlawToLinear 解 A-law 到 16-bit PCM。
func AlawToLinear(a byte) int16 {
	a ^= 0x55
	sign := a & 0x80
	exp := int(a&0x70) >> 4
	mantissa := int(a & 0x0F)
	var sample int
	if exp == 0 {
		sample = (mantissa << 4) | 0x08
	} else {
		sample = ((mantissa << 4) | 0x108) << (exp - 1)
	}
	if sign == 0 {
		sample = -sample
	}
	return int16(sample)
}

// LinearToUlaw 16-bit PCM → 8-bit μ-law。
func LinearToUlaw(pcm int16) byte {
	const bias = 0x84
	const clip = 32635
	sign := int(0)
	v := int(pcm)
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > clip {
		v = clip
	}
	v += bias
	exp := 7
	for ; exp > 0; exp-- {
		if v >= (1 << (exp + 7)) {
			break
		}
	}
	mantissa := (v >> (exp + 3)) & 0x0F
	ulaw := ^(sign | (exp << 4) | mantissa)
	return byte(ulaw)
}

// UlawToLinear μ-law → 16-bit PCM。
func UlawToLinear(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exp := int(u&0x70) >> 4
	mantissa := int(u & 0x0F)
	sample := ((mantissa << 3) + 0x84) << exp
	sample -= 0x84
	if sign != 0 {
		sample = -sample
	}
	return int16(sample)
}

// EncodePCM 把 PCM 切片编成 G.711 字节切片,每个样本对应 1 字节。
func EncodePCM(pcm []int16, codecName string) []byte {
	out := make([]byte, len(pcm))
	switch codecName {
	case "PCMA", "pcma", "alaw":
		for i, s := range pcm {
			out[i] = LinearToAlaw(s)
		}
	case "PCMU", "pcmu", "ulaw":
		for i, s := range pcm {
			out[i] = LinearToUlaw(s)
		}
	}
	return out
}

// DecodePCM 反向。
func DecodePCM(data []byte, codecName string) []int16 {
	out := make([]int16, len(data))
	switch codecName {
	case "PCMA", "pcma", "alaw":
		for i, b := range data {
			out[i] = AlawToLinear(b)
		}
	case "PCMU", "pcmu", "ulaw":
		for i, b := range data {
			out[i] = UlawToLinear(b)
		}
	}
	return out
}
