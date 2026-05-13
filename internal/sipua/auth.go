package sipua

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

// BuildDigestResponse 按 RFC 2617(MD5 / qop=auth)算 response,生成 Authorization header value。
func BuildDigestResponse(c DigestChallenge, method, uri, user, pass string, nc int) (string, error) {
	if !strings.EqualFold(c.Algorithm, "MD5") {
		return "", fmt.Errorf("unsupported algorithm: %s", c.Algorithm)
	}
	ha1 := md5hex(fmt.Sprintf("%s:%s:%s", user, c.Realm, pass))
	ha2 := md5hex(fmt.Sprintf("%s:%s", method, uri))

	var response string
	var fields []string
	fields = append(fields,
		fmt.Sprintf(`username="%s"`, user),
		fmt.Sprintf(`realm="%s"`, c.Realm),
		fmt.Sprintf(`nonce="%s"`, c.Nonce),
		fmt.Sprintf(`uri="%s"`, uri),
		`algorithm=MD5`,
	)

	switch strings.ToLower(c.QOP) {
	case "", "auth-int":
		// qop 为空时走 RFC 2069 老算法;auth-int 我们简化按 auth 处理(SIP 实务里很少用 auth-int)
		response = md5hex(ha1 + ":" + c.Nonce + ":" + ha2)
	case "auth":
		ncStr := fmt.Sprintf("%08x", nc)
		cnonce := randHex(8)
		response = md5hex(strings.Join([]string{ha1, c.Nonce, ncStr, cnonce, "auth", ha2}, ":"))
		fields = append(fields,
			fmt.Sprintf(`qop=auth`),
			fmt.Sprintf(`nc=%s`, ncStr),
			fmt.Sprintf(`cnonce="%s"`, cnonce),
		)
	default:
		// 多个 qop 选项,选 auth
		for _, opt := range strings.Split(c.QOP, ",") {
			if strings.EqualFold(strings.TrimSpace(opt), "auth") {
				return BuildDigestResponse(DigestChallenge{
					Realm: c.Realm, Nonce: c.Nonce, Opaque: c.Opaque,
					QOP: "auth", Algorithm: c.Algorithm,
				}, method, uri, user, pass, nc)
			}
		}
		return "", fmt.Errorf("unsupported qop: %s", c.QOP)
	}

	fields = append(fields, fmt.Sprintf(`response="%s"`, response))
	if c.Opaque != "" {
		fields = append(fields, fmt.Sprintf(`opaque="%s"`, c.Opaque))
	}
	return "Digest " + strings.Join(fields, ", "), nil
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
