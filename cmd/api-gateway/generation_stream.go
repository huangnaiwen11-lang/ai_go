package main

import (
	"net/http"
	"time"

	"ai-business-service/internal/realtime"
	"ai-business-service/internal/transport/sessionauth"
)

// newOptionalGenerationStreamHandler 仅装配本地内存 SSE；默认关闭，且不读取 Redis。
func newOptionalGenerationStreamHandler(enabled bool, authenticator *sessionauth.Authenticator) (http.Handler, func()) {
	if !enabled || authenticator == nil {
		return nil, func() {}
	}
	return realtime.NewHandler(authenticator, realtime.NewHub(32), 15*time.Second), func() {}
}
