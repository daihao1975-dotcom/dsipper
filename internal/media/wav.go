package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// WAV 读写 — 仅支持 16-bit PCM mono @ 任意采样率。
// SIP mock 场景里输入/输出都是 8000 Hz mono,但读时不强制采样率,
// 调用方自行处理(若 != 8000 需要 resample,本工具不做)。

// maxWAVChunkBytes 是单个 RIFF chunk 的硬上限(50 MB),防御恶意 WAV 把
// 4 GiB size 字段直接喂进 make([]byte, size) 触发 OOM。50MB @ 8kHz 16-bit
// mono 已是 ~52 分钟音频,SIP mock 场景绝对够用。
const maxWAVChunkBytes = 50 * 1024 * 1024

// ReadWAV16Mono 读一个 16-bit mono WAV 文件,返回 PCM samples + sampleRate。
func ReadWAV16Mono(path string) (samples []int16, sampleRate int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, 0, fmt.Errorf("read RIFF header: %w", err)
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a WAV file")
	}

	for {
		var ck [8]byte
		if _, err := io.ReadFull(f, ck[:]); err != nil {
			return nil, 0, err
		}
		size := binary.LittleEndian.Uint32(ck[4:8])
		if size > maxWAVChunkBytes {
			return nil, 0, fmt.Errorf("WAV chunk %q too large: %d bytes (cap %d)", string(ck[0:4]), size, maxWAVChunkBytes)
		}
		switch string(ck[0:4]) {
		case "fmt ":
			if size < 16 {
				return nil, 0, fmt.Errorf("WAV fmt chunk too small: %d bytes", size)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil, 0, err
			}
			audioFmt := binary.LittleEndian.Uint16(body[0:2])
			channels := binary.LittleEndian.Uint16(body[2:4])
			sampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
			bitsPerSample := binary.LittleEndian.Uint16(body[14:16])
			if audioFmt != 1 {
				return nil, 0, fmt.Errorf("only PCM (audio_fmt=1) supported, got %d", audioFmt)
			}
			if channels != 1 {
				return nil, 0, fmt.Errorf("only mono supported, got %d channels", channels)
			}
			if bitsPerSample != 16 {
				return nil, 0, fmt.Errorf("only 16-bit PCM supported, got %d bits", bitsPerSample)
			}
		case "data":
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil, 0, err
			}
			samples = make([]int16, len(body)/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(body[i*2:]))
			}
			return samples, sampleRate, nil
		default:
			// 跳过未知 chunk
			if _, err := io.CopyN(io.Discard, f, int64(size)); err != nil {
				return nil, 0, err
			}
		}
		// chunk 字节奇数时有 1 字节 padding
		if size%2 == 1 {
			f.Seek(1, io.SeekCurrent)
		}
	}
}

// WriteWAV16Mono 把 16-bit mono PCM 写入 WAV 文件。文件权限 0600(可能含敏感语音录音)。
func WriteWAV16Mono(path string, samples []int16, sampleRate int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	dataSize := len(samples) * 2
	totalSize := 36 + dataSize

	hdr := make([]byte, 0, 44)
	hdr = append(hdr, []byte("RIFF")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(totalSize))
	hdr = append(hdr, []byte("WAVE")...)
	hdr = append(hdr, []byte("fmt ")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, 16)         // chunk size
	hdr = binary.LittleEndian.AppendUint16(hdr, 1)          // PCM
	hdr = binary.LittleEndian.AppendUint16(hdr, 1)          // channels
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(sampleRate))
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(sampleRate*2)) // byte rate
	hdr = binary.LittleEndian.AppendUint16(hdr, 2)          // block align
	hdr = binary.LittleEndian.AppendUint16(hdr, 16)         // bits per sample
	hdr = append(hdr, []byte("data")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(dataSize))

	if _, err := f.Write(hdr); err != nil {
		return err
	}
	body := make([]byte, dataSize)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(body[i*2:], uint16(s))
	}
	_, err = f.Write(body)
	return err
}
