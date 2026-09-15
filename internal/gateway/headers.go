package gateway

import (
	"net/http"
	"strings"
)

// copyResponseHeaders 排除仅对 Node 到网关这一跳有效的连接级头。对本地金丝雀响应
// 还要移除实体分帧头：响应正文已由本地重新生成，不能继承 Node 的过期压缩或长度元数据。
func copyResponseHeaders(destination, source http.Header, omitEntityFraming bool) {
	connectionHeaders := hopByHopHeaderNames(source)
	for header, values := range source {
		canonicalHeader := http.CanonicalHeaderKey(header)
		if _, hopByHop := connectionHeaders[canonicalHeader]; hopByHop {
			continue
		}
		if omitEntityFraming && (canonicalHeader == "Content-Length" || canonicalHeader == "Transfer-Encoding" || canonicalHeader == "Content-Encoding") {
			continue
		}
		destination.Del(canonicalHeader)
		for _, value := range values {
			destination.Add(canonicalHeader, value)
		}
	}
}

func removeHopByHopHeaders(headers http.Header) {
	for header := range hopByHopHeaderNames(headers) {
		headers.Del(header)
	}
}

func hopByHopHeaderNames(headers http.Header) map[string]struct{} {
	names := map[string]struct{}{
		"Connection":          {},
		"Proxy-Connection":    {},
		"Keep-Alive":          {},
		"Proxy-Authenticate":  {},
		"Proxy-Authorization": {},
		"Te":                  {},
		"Trailer":             {},
		"Transfer-Encoding":   {},
		"Upgrade":             {},
	}
	for _, value := range headers.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if header := strings.TrimSpace(token); header != "" {
				names[http.CanonicalHeaderKey(header)] = struct{}{}
			}
		}
	}
	return names
}
