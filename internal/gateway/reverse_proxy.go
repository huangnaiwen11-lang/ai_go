package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

var readRequestIDRandom = rand.Read

var fallbackRequestIDSequence atomic.Uint64

func newProxy(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	// 禁用缓冲定时器，确保 SSE 与其他流式响应在 Node 刷新后能及时透传给客户端。
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(resp *http.Response) error {
		// 外层 Handler 已在代理前确定 request id；若采纳上游值，内部服务就能篡改
		// 对外关联标识，破坏跨服务的可追踪性。
		if requestID := resp.Request.Header.Get("X-Request-Id"); requestID != "" {
			resp.Header.Set("X-Request-Id", requestID)
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		// 不回显上游网络错误，避免泄露内部拓扑，并维持统一的公开错误信封。
		writeUpstreamFailure(w, req, err)
	}
	return proxy
}

type upstreamFailureContract struct {
	status  int
	code    string
	message string
}

func classifyUpstreamFailure(err error) upstreamFailureContract {
	if isUpstreamTimeout(err) {
		return upstreamTimeoutContract()
	}
	return upstreamFailureContract{
		status:  http.StatusBadGateway,
		code:    "UPSTREAM_BAD_RESPONSE",
		message: "Upstream service returned an invalid response",
	}
}

func isUpstreamTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var urlError *url.Error
	return errors.As(err, &urlError) && urlError.Err != nil && isUpstreamTimeout(urlError.Err)
}

func upstreamTimeoutContract() upstreamFailureContract {
	return upstreamFailureContract{
		status:  http.StatusGatewayTimeout,
		code:    "UPSTREAM_TIMEOUT",
		message: "Upstream service timed out",
	}
}

func writeUpstreamFailure(w http.ResponseWriter, req *http.Request, err error) {
	contract := classifyUpstreamFailure(err)
	requestID := req.Header.Get("X-Request-Id")
	w.Header().Set("X-Request-Id", requestID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(contract.status)
	_ = json.NewEncoder(w).Encode(struct {
		Success   bool   `json:"success"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		Details   any    `json:"details"`
		RequestID string `json:"requestId"`
	}{false, contract.code, contract.message, nil, requestID})
}

func newRequestID() string {
	var bytes [16]byte
	if _, err := readRequestIDRandom(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("gateway-%d-%d", time.Now().UnixNano(), fallbackRequestIDSequence.Add(1))
}
