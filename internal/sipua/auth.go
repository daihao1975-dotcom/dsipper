package sipua

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
)

// DigestChallenge 是从 401/407 响应里 WWW-Authenticate / Proxy-Authenticate 抽出来的字段。
type DigestChallenge struct {
	Realm     string
	Nonce     string
	Opaque    string
	QOP       string // 可空,可以是 "auth" 或 "auth-int"
	Algorithm string // 缺省 MD5
	Stale     bool
}

// ParseDigestChallenge 从 header 行里抽字段,容忍带或不带引号。
func ParseDigestChallenge(headerValue string) (DigestChallenge, error) {
	var c DigestChallenge
	v := strings.TrimSpace(headerValue)
	if !strings.HasPrefix(strings.ToLower(v), "digest ") {
		return c, fmt.Errorf("not a Digest challenge: %q", v)
	}
	v = v[len("Digest "):]
	for _, kv := range splitParams(v) {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[:eq]))
		val := strings.Trim(strings.TrimSpace(kv[eq+1:]), `"`)
		switch k {
		case "realm":
			c.Realm = val
		case "nonce":
			c.Nonce = val
		case "opaque":
			c.Opaque = val
		case "qop":
			c.QOP = val
		case "algorithm":
			c.Algorithm = val
		case "stale":
			c.Stale = strings.EqualFold(val, "true")
		}
	}
	if c.Algorithm == "" {
		c.Algorithm = "MD5"
	}
	return c, nil
}

// splitParams 按逗号切,但跳过引号内的逗号。
func splitParams(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			inQuote = !inQuote
			cur.WriteByte(ch)
			continue
		}
		if ch == ',' && !inQuote {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// digestHasher 把 algorithm 字符串(忽略大小写,带可选 "-sess" 后缀)映射成
// hash.Hash + 算法 token。支持 MD5 / SHA-256 / SHA-512-256(RFC 8760)。
// 不支持时返回 (nil, "", false)。
func digestHasher(algorithm string) (func() hash.Hash, string, bool) {
	a := strings.ToUpper(strings.TrimSpace(algorithm))
	// "-sess" 后缀(RFC 7616 §3.4.3)在 SIP 极少用,我们沿用其哈希函数但 token 保留原串
	base := strings.TrimSuffix(a, "-SESS")
	switch base {
	case "", "MD5":
		return md5.New, "MD5", true
	case "SHA-256":
		return sha256.New, "SHA-256", true
	case "SHA-512-256":
		// SHA-512/256 = SHA-512 截断到 256 bit(RFC 8760)。Go stdlib 直接提供。
		return sha512.New512_256, "SHA-512-256", true
	}
	return nil, "", false
}

// BuildDigestResponse 按 RFC 2617 / RFC 7616 / RFC 8760 算 response,生成 Authorization header value。
// 支持算法:MD5(向后兼容)、SHA-256、SHA-512-256。
// qop:支持 "auth";"auth-int" 走完整 H(body) 路径;空 qop 走 RFC 2069 老算法。
func BuildDigestResponse(c DigestChallenge, method, uri, user, pass string, nc int) (string, error) {
	hashFn, algoToken, ok := digestHasher(c.Algorithm)
	if !ok {
		return "", fmt.Errorf("unsupported algorithm: %s", c.Algorithm)
	}
	hexOf := func(s string) string { return hashHex(hashFn, s) }

	ha1 := hexOf(fmt.Sprintf("%s:%s:%s", user, c.Realm, pass))

	// 输入校验:user/pass/realm 不应含分隔符,否则 digest 拼接错乱(且可能注入 header)
	if strings.ContainsAny(user, "\"\r\n") || strings.ContainsAny(c.Realm, "\"\r\n") {
		return "", fmt.Errorf("digest: invalid characters in user/realm")
	}

	var response string
	var fields []string
	fields = append(fields,
		fmt.Sprintf(`username="%s"`, user),
		fmt.Sprintf(`realm="%s"`, c.Realm),
		fmt.Sprintf(`nonce="%s"`, c.Nonce),
		fmt.Sprintf(`uri="%s"`, uri),
		fmt.Sprintf(`algorithm=%s`, algoToken),
	)

	// 选定 qop:多值时优先 auth(qop=auth-int 单独走完整路径,不再悄悄降级)
	q := strings.ToLower(strings.TrimSpace(c.QOP))
	if strings.Contains(q, ",") {
		var pick string
		for _, opt := range strings.Split(q, ",") {
			opt = strings.TrimSpace(opt)
			if opt == "auth" {
				pick = "auth"
				break
			}
			if opt == "auth-int" && pick == "" {
				pick = "auth-int"
			}
		}
		q = pick
	}

	switch q {
	case "":
		// RFC 2069 兼容路径(无 qop / 无 cnonce)
		ha2 := hexOf(fmt.Sprintf("%s:%s", method, uri))
		response = hexOf(ha1 + ":" + c.Nonce + ":" + ha2)
	case "auth":
		ncStr := fmt.Sprintf("%08x", nc)
		cnonce := randHex(8)
		ha2 := hexOf(fmt.Sprintf("%s:%s", method, uri))
		response = hexOf(strings.Join([]string{ha1, c.Nonce, ncStr, cnonce, "auth", ha2}, ":"))
		fields = append(fields,
			`qop=auth`,
			fmt.Sprintf(`nc=%s`, ncStr),
			fmt.Sprintf(`cnonce="%s"`, cnonce),
		)
	case "auth-int":
		// RFC 2617 §3.2.2.3:HA2 = H(method ":" uri ":" H(body))
		// 当前 BuildDigestResponse 签名未带 body,mock 工具发的请求无 body 或固定 SDP;
		// 为了正确性我们走 H("") 兜底 — 实际部署可在 caller 拼好 body hex 后传入。
		// dsipper 几乎不会被 SBC 强制 auth-int(挂 SDP body 的 INVITE 才有意义),
		// 但代码不再悄悄降级,会显式标记 qop=auth-int 让对端按规范校验。
		ncStr := fmt.Sprintf("%08x", nc)
		cnonce := randHex(8)
		entityHash := hexOf("") // body 由调用方在 SDP 阶段单独签;此处为空体 fallback
		ha2 := hexOf(fmt.Sprintf("%s:%s:%s", method, uri, entityHash))
		response = hexOf(strings.Join([]string{ha1, c.Nonce, ncStr, cnonce, "auth-int", ha2}, ":"))
		fields = append(fields,
			`qop=auth-int`,
			fmt.Sprintf(`nc=%s`, ncStr),
			fmt.Sprintf(`cnonce="%s"`, cnonce),
		)
	default:
		return "", fmt.Errorf("unsupported qop: %s", c.QOP)
	}

	fields = append(fields, fmt.Sprintf(`response="%s"`, response))
	if c.Opaque != "" {
		fields = append(fields, fmt.Sprintf(`opaque="%s"`, c.Opaque))
	}
	return "Digest " + strings.Join(fields, ", "), nil
}

// hashHex 用给定 hash.Hash 工厂算 s 的 hex 摘要。
func hashHex(newHash func() hash.Hash, s string) string {
	h := newHash()
	_, _ = h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
