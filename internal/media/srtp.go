package media

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/pion/srtp/v3"
)

// SRTP SDES(RFC 4568)的最小实现:只支持业界压倒性常见的 AES_CM_128_HMAC_SHA1_80
// 套件 — 16 字节 master key + 14 字节 master salt,合计 30 字节 base64 编码后挂在
// SDP a=crypto inline 参数里。
//
// 不实现:DTLS-SRTP / AES_GCM / SRTP-MKI / 多 crypto tag 协商。
// 这些功能在 mock 工具语境下基本用不上,真要测 WebRTC 链路用 dsipper 也不是好选择。

// SRTPProfileName 是我们渲染到 SDP a=crypto 的套件名。
const SRTPProfileName = "AES_CM_128_HMAC_SHA1_80"

// SRTPProfile 是 pion 的对应枚举值。
const SRTPProfile = srtp.ProtectionProfileAes128CmHmacSha1_80

// SRTPKeyBytes 是 master key + salt 合计长度(16 + 14)。
const SRTPKeyBytes = 30

// GenerateSRTPKey 生成一份随机 master key + salt(crypto/rand)。
func GenerateSRTPKey() ([]byte, error) {
	k := make([]byte, SRTPKeyBytes)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// EncodeSRTPInline 把 30 字节密钥编码成 SDES inline 字段值(纯 base64,无 lifetime / MKI)。
func EncodeSRTPInline(key []byte) string {
	return base64.StdEncoding.EncodeToString(key)
}

// DecodeSRTPInline 解一个 SDES inline 串(可能带 |lifetime|mki 等参数,用 '|' 切掉)。
// 返回 30 字节 master key+salt。
func DecodeSRTPInline(inline string) ([]byte, error) {
	s := inline
	if i := strings.IndexByte(s, '|'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// 兼容某些实现给 URL-safe base64
		raw, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("inline base64: %w", err)
		}
	}
	if len(raw) != SRTPKeyBytes {
		return nil, fmt.Errorf("SRTP inline 长度 %d, 期望 %d", len(raw), SRTPKeyBytes)
	}
	return raw, nil
}

// NewSRTPContext 用 (16 key + 14 salt) 构造 pion srtp.Context;
// 注意 Context 只能用于"加密"或"解密"单方向(pion 设计),
// 调用方需要双向时建两个 Context。
func NewSRTPContext(key []byte) (*srtp.Context, error) {
	if len(key) != SRTPKeyBytes {
		return nil, fmt.Errorf("SRTP key 长度必须是 %d, 实际 %d", SRTPKeyBytes, len(key))
	}
	return srtp.CreateContext(key[:16], key[16:], SRTPProfile)
}
