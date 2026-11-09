package main

import (
	"net"
	"net/http"
	"strings"
)

// detailBodyLimit 请求体快照上限：超出部分丢弃，只留头部便于排查。
// 带图推送的 body 里是 base64 图片，动辄数百 KB，因此必须截断。
const detailBodyLimit = 16 << 10

// sensitiveHeader 判断请求头是否为密钥类，需要在快照里打码。
func sensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	for _, k := range []string{"token", "secret", "authorization", "cookie", "signature", "password", "apikey", "api-key"} {
		if strings.Contains(n, k) {
			return true
		}
	}
	return false
}

// captureDetail 采集一次入站请求的快照；body 为已读到的原始字节。
// 敏感请求头走 maskSecret，请求体超 limit 截断并标记。
func captureDetail(r *http.Request, body []byte) *RequestDetail {
	d := &RequestDetail{
		Method:      r.Method,
		Path:        r.URL.Path,
		RemoteIP:    clientIP(r),
		ContentType: r.Header.Get("Content-Type"),
		Headers:     make(map[string]string, len(r.Header)),
		BodyBytes:   len(body),
	}
	for k, vs := range r.Header {
		v := strings.Join(vs, ", ")
		if sensitiveHeader(k) {
			v = maskSecret(v)
		}
		d.Headers[k] = v
	}
	if len(body) > detailBodyLimit {
		// 按 rune 边界裁剪，避免切碎 UTF-8
		cut := detailBodyLimit
		for cut > 0 && !utf8Start(body[cut]) {
			cut--
		}
		d.Body = string(body[:cut])
		d.BodyTruncated = true
	} else {
		d.Body = string(body)
	}
	return d
}

// utf8Start 判断该字节是否为 UTF-8 字符起始字节（非 10xxxxxx）。
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// clientIP 取来源 IP：优先 X-Forwarded-For 首个，其次 RemoteAddr。
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
