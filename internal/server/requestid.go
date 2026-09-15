package server

import (
	"strings"
	"unicode"

	"github.com/google/uuid"
	stdhttp "net/http"
)

const requestIDHeader = "X-Request-Id"

// RequestIDFilter 在路由、恢复中间件和错误编码器之前建立同一个请求标识。
// 合法的调用方标识会原样保留，便于端到端排查；空值、控制字符或过长值会替换为
// 服务端 UUID，避免把不安全的 HTTP 头写回响应。
func RequestIDFilter(next stdhttp.Handler) stdhttp.Handler {
	return stdhttp.HandlerFunc(func(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
		requestID := normalizeRequestID(request.Header.Get(requestIDHeader))
		if requestID == "" {
			requestID = uuid.NewString()
		}

		request.Header.Set(requestIDHeader, requestID)
		writer.Header().Set(requestIDHeader, requestID)
		next.ServeHTTP(writer, request)
	})
}

// normalizeRequestID 仅接受能够安全回写到 HTTP 头的短文本。
func normalizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return ""
		}
	}
	return value
}
