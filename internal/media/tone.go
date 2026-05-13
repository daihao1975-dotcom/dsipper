package media

import "math"

// SineTone 生成指定频率/时长的 16-bit PCM mono。
//   freqHz   = 440 标准 A
//   durSec   = 持续秒
//   sampleRate = 8000(电话标准)
//   amplitude  = 0..1,推荐 0.3 防截顶
func SineTone(freqHz float64, durSec float64, sampleRate int, amplitude float64) []int16 {
	n := int(float64(sampleRate) * durSec)
	out := make([]int16, n)
	twoPiF := 2.0 * math.Pi * freqHz
	for i := 0; i < n; i++ {
		t := float64(i) / float64(sampleRate)
		v := amplitude * math.Sin(twoPiF*t)
		out[i] = int16(v * 32767)
	}
	return out
}
