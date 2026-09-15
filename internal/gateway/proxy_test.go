package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type enabledRouteSwitch struct {
	enabled exactRouteKey
}

func (switcher enabledRouteSwitch) Enabled(route exactRouteKey) bool {
	return route == switcher.enabled
}

type responseStartRecorder struct {
	*httptest.ResponseRecorder
	started   chan struct{}
	startOnce sync.Once
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type timeoutNetError struct{}

func (timeoutNetError) Error() string {
	return "upstream response header timed out"
}

func (timeoutNetError) Timeout() bool {
	return true
}

func (timeoutNetError) Temporary() bool {
	return true
}

type timeoutAsAdapterError struct{}

func (timeoutAsAdapterError) Error() string {
	return "timeout available only through errors.As"
}

func (timeoutAsAdapterError) As(target any) bool {
	networkError, ok := target.(*net.Error)
	if !ok {
		return false
	}
	*networkError = timeoutNetError{}
	return true
}

type interruptedReadCloser struct {
	body   []byte
	read   bool
	closed bool
}

func (reader *interruptedReadCloser) Read(destination []byte) (int, error) {
	if reader.read {
		return 0, io.ErrUnexpectedEOF
	}
	reader.read = true
	return copy(destination, reader.body), io.ErrUnexpectedEOF
}

func (reader *interruptedReadCloser) Close() error {
	reader.closed = true
	return nil
}

func newResponseStartRecorder() *responseStartRecorder {
	return &responseStartRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		started:          make(chan struct{}),
	}
}

func (recorder *responseStartRecorder) WriteHeader(statusCode int) {
	recorder.startOnce.Do(func() { close(recorder.started) })
	recorder.ResponseRecorder.WriteHeader(statusCode)
}

func (recorder *responseStartRecorder) Write(body []byte) (int, error) {
	recorder.startOnce.Do(func() { close(recorder.started) })
	return recorder.ResponseRecorder.Write(body)
}

func TestHandlerPreservesAPIPathHeadersAndEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/homepage/content" {
			t.Fatalf("path = %s, want /api/homepage/content", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("X-Request-Id"); got != "req-123" {
			t.Fatalf("request id = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"data":{"source":"node"}}`)
	}))
	defer upstream.Close()

	g := New(Config{DefaultUpstream: mustURL(t, upstream.URL)})
	req := httptest.NewRequest(http.MethodGet, "/api/homepage/content", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Request-Id", "req-123")
	req.Header.Set("X-Language", "zh-CN")
	req.Header.Set("X-Device-Id", "device-1")
	rec := httptest.NewRecorder()

	g.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if values := rec.Result().Header.Values("X-Request-Id"); len(values) != 1 || values[0] != "req-123" {
		t.Fatalf("request id headers = %#v, want exactly [req-123]", values)
	}
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid envelope: %v", err)
	}
	if envelope["success"] != true || envelope["data"].(map[string]any)["source"] != "node" {
		t.Fatalf("unexpected envelope: %#v", envelope)
	}
}

func TestHandlerFallsBackForPathsOutsideConfirmedExactRoutes(t *testing.T) {
	const growthPingBody = `{"success":true,"data":{"ok":true}}`
	const nodeBody = `{"success":true,"data":{"source":"node"}}`
	var nodeHits int
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeHits++
		if r.URL.Path == growthPingPath {
			_, _ = io.WriteString(w, growthPingBody)
			return
		}
		_, _ = io.WriteString(w, nodeBody)
	}))
	defer node.Close()

	g := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	})

	// 阶段 0 矩阵只确认这一精确本地路由；相似路径必须继续交给 Node。
	growthPingRecorder := httptest.NewRecorder()
	g.Handler().ServeHTTP(growthPingRecorder, httptest.NewRequest(http.MethodGet, "/api/growth/ping", nil))
	if growthPingRecorder.Code != http.StatusOK {
		t.Fatalf("growth ping status = %d, want 200", growthPingRecorder.Code)
	}
	if got := growthPingRecorder.Body.String(); got != growthPingBody {
		t.Fatalf("growth ping response body = %q, want %q", got, growthPingBody)
	}
	if nodeHits != 1 {
		t.Fatalf("nodeHits after confirmed route = %d, want 1 admission request", nodeHits)
	}

	for _, path := range []string{
		"/api/homepage/content",
		"/api/homepage/video-templates",
		"/api/v1/homepage/video-templates",
		"/api/discover/templates",
	} {
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("path %s status = %d, want 200", path, rec.Code)
		}
		if got := rec.Body.String(); got != nodeBody {
			t.Fatalf("path %s response body = %q, want %q", path, got, nodeBody)
		}
	}
	if nodeHits != 5 {
		t.Fatalf("nodeHits = %d, want 5", nodeHits)
	}
}

// TestAuthEntryUsesOnlyExactEnabledRoute 防止 OAuth、编码路径和带查询参数的认证请求被
// 意外接管；这些请求必须继续保持 Node 的既有语义。
func TestAuthEntryUsesOnlyExactEnabledRoute(t *testing.T) {
	var nodeHits, localHits int
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nodeHits++
		_, _ = io.WriteString(writer, "node")
	}))
	defer node.Close()
	local := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		localHits++
		_, _ = io.WriteString(writer, "go")
	})

	gateway := New(Config{
		DefaultUpstream:  mustURL(t, node.URL),
		RouteSwitch:      enabledRouteSwitch{enabled: exactRouteKey{method: http.MethodPost, path: "/api/auth/register"}},
		AuthEntryHandler: local,
	})

	localRecorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(localRecorder, httptest.NewRequest(http.MethodPost, "/api/auth/register", nil))
	if localRecorder.Code != http.StatusOK || localRecorder.Body.String() != "go" || localHits != 1 || nodeHits != 0 {
		t.Fatalf("exact route = status %d, body %q, local %d, node %d", localRecorder.Code, localRecorder.Body.String(), localHits, nodeHits)
	}

	for _, target := range []string{
		"/api/auth/register?provider=google",
		"/api/auth%2Fregister",
		"/api/auth/login",
	} {
		recorder := httptest.NewRecorder()
		gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
		if recorder.Body.String() != "node" {
			t.Fatalf("target %q body = %q, want Node fallback", target, recorder.Body.String())
		}
	}
	if localHits != 1 || nodeHits != 3 {
		t.Fatalf("route counters = local %d, node %d", localHits, nodeHits)
	}
}

// 本地支付入口只能在精确 POST 路由、处理器已装配且 route-switch 明确放行时接管。
// 所有相近 URL 继续由 Node 处理，避免误截获现网其他钱包和支付 API。
func TestPaymentEntry只在精确路由和开关开启时接管(t *testing.T) {
	var nodeHits, localHits int
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nodeHits++
		_, _ = io.WriteString(writer, "node")
	}))
	defer node.Close()
	local := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		localHits++
		_, _ = io.WriteString(writer, "go-payment")
	})

	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodPost,
			path:   "/api/wallet/verify-purchase",
		}},
		PaymentEntryHandler: local,
	})

	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/wallet/verify-purchase", nil))
	if recorder.Body.String() != "go-payment" || localHits != 1 || nodeHits != 0 {
		t.Fatalf("精确本地路由 = %q，local=%d node=%d", recorder.Body.String(), localHits, nodeHits)
	}

	for _, target := range []string{
		"/api/wallet/verify-purchase?retry=1",
		"/api/wallet%2Fverify-purchase",
		"/api/wallet/create-external-checkout",
	} {
		recorder = httptest.NewRecorder()
		gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
		if recorder.Body.String() != "node" {
			t.Fatalf("target %q 返回 %q，期望 Node 代理", target, recorder.Body.String())
		}
	}
	if localHits != 1 || nodeHits != 3 {
		t.Fatalf("路由计数 local=%d node=%d，期望 1/3", localHits, nodeHits)
	}
}

func TestHandler在未配置生成回调处理器时继续代理(t *testing.T) {
	upstream := newCallbackTestUpstream(t)
	defer upstream.Close()

	gateway := New(Config{DefaultUpstream: mustURL(t, upstream.URL)})
	for _, path := range []string{
		"/api/v1/internal/generation-callback",
		"/api/internal/generation-callback",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
			if recorder.Code != http.StatusAccepted || recorder.Body.String() != "node" {
				t.Fatalf("response = %d %q, want Node fallback", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandlerGeneratesRequestIDAndPreservesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Request-Id") == "" {
			t.Error("upstream did not receive generated request id")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream does not support flushing")
		}
		_, _ = io.WriteString(w, "data: {\"kind\":\"snapshot\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := New(Config{DefaultUpstream: mustURL(t, upstream.URL)})
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/users/me/generations/stream", nil))

	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("gateway did not return request id")
	}
	if rec.Body.String() != "data: {\"kind\":\"snapshot\"}\n\n" {
		t.Fatalf("unexpected SSE body: %q", rec.Body.String())
	}
}

func TestHandlerRelaysNodeRateLimitBeforeLocalPing(t *testing.T) {
	const (
		requestID         = "req-from-client"
		upstreamRequestID = "req-rate-limited"
		rateLimitedBody   = `{"success":false,"code":"RATE_LIMITED","message":"Too many requests","details":null,"requestId":"req-rate-limited"}`
	)
	var received struct {
		method        string
		path          string
		authorization string
		requestID     string
		language      string
		deviceID      string
		forwardedFor  string
	}
	var nodeHits int
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeHits++
		received.method = r.Method
		received.path = r.URL.Path
		received.authorization = r.Header.Get("Authorization")
		received.requestID = r.Header.Get("X-Request-Id")
		received.language = r.Header.Get("X-Language")
		received.deviceID = r.Header.Get("X-Device-Id")
		received.forwardedFor = r.Header.Get("X-Forwarded-For")

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("RateLimit-Limit", "60")
		w.Header().Set("RateLimit-Remaining", "0")
		w.Header().Set("RateLimit-Reset", "1735689660")
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Request-Id", upstreamRequestID)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, rateLimitedBody)
	}))
	defer node.Close()

	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	})
	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
	request.Header.Set("Authorization", "Bearer rate-limited-token")
	request.Header.Set("X-Request-Id", requestID)
	request.Header.Set("X-Language", "zh-CN")
	request.Header.Set("X-Device-Id", "device-rate-limited")
	request.Header.Set("X-Forwarded-For", "198.51.100.10")
	request.RemoteAddr = "203.0.113.20:12345"
	recorder := httptest.NewRecorder()

	gateway.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if got := recorder.Body.String(); got != rateLimitedBody {
		t.Fatalf("body = %q, want byte-for-byte upstream rate limit envelope", got)
	}
	for header, want := range map[string]string{
		"Content-Type":        "application/json",
		"RateLimit-Limit":     "60",
		"RateLimit-Remaining": "0",
		"RateLimit-Reset":     "1735689660",
		"Retry-After":         "30",
		// 即使 Node 准入返回限流，客户端也必须保留入口关联 ID；否则同一请求在
		// 金丝雀开关两侧会出现不同的可追踪标识。
		"X-Request-Id": requestID,
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if nodeHits != 1 {
		t.Fatalf("nodeHits = %d, want 1", nodeHits)
	}
	if received.method != http.MethodGet || received.path != growthPingPath {
		t.Fatalf("Node request = %s %s, want GET %s", received.method, received.path, growthPingPath)
	}
	if received.authorization != "Bearer rate-limited-token" {
		t.Fatalf("Node Authorization = %q", received.authorization)
	}
	if received.requestID != requestID {
		t.Fatalf("Node X-Request-Id = %q, want %q", received.requestID, requestID)
	}
	if received.language != "zh-CN" {
		t.Fatalf("Node X-Language = %q", received.language)
	}
	if received.deviceID != "device-rate-limited" {
		t.Fatalf("Node X-Device-Id = %q", received.deviceID)
	}
	if received.forwardedFor != "198.51.100.10, 203.0.113.20" {
		t.Fatalf("Node X-Forwarded-For = %q, want appended client IP", received.forwardedFor)
	}
}

func TestHandlerUsesNodeAdmissionBeforeLocalPing(t *testing.T) {
	testCases := []struct {
		name                      string
		upstreamBody              string
		expectedResponseRequestID string
	}{
		{
			name:                      "compatible success envelope",
			upstreamBody:              growthPingResponse,
			expectedResponseRequestID: "entry-request-id",
		},
		{
			name:                      "different success envelope preserves entry request id",
			upstreamBody:              "{\"data\":{\"ok\":true},\"success\":true}\n",
			expectedResponseRequestID: "entry-request-id",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var nodeHits int
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nodeHits++
				if r.Method != http.MethodGet || r.URL.Path != growthPingPath {
					t.Fatalf("Node request = %s %s, want GET %s", r.Method, r.URL.Path, growthPingPath)
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("RateLimit-Remaining", "49")
				w.Header().Set("X-Request-Id", "node-admission-request-id")
				_, _ = io.WriteString(w, testCase.upstreamBody)
			}))
			defer node.Close()

			gateway := New(Config{
				DefaultUpstream: mustURL(t, node.URL),
				RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
					method: http.MethodGet,
					path:   growthPingPath,
				}},
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
			request.Header.Set("X-Request-Id", "entry-request-id")
			gateway.Handler().ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if got := recorder.Body.String(); got != testCase.upstreamBody {
				t.Fatalf("body = %q, want byte-for-byte upstream body %q", got, testCase.upstreamBody)
			}
			if got := recorder.Header().Get("RateLimit-Remaining"); got != "49" {
				t.Fatalf("RateLimit-Remaining = %q, want 49", got)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want Node value with charset preserved", got)
			}
			if got := recorder.Header().Get("X-Request-Id"); got != testCase.expectedResponseRequestID {
				t.Fatalf("X-Request-Id = %q, want %q", got, testCase.expectedResponseRequestID)
			}
			if nodeHits != 1 {
				t.Fatalf("nodeHits = %d, want 1", nodeHits)
			}
		})
	}
}

func TestHandlerRelaysEncodedNodeSuccessWithoutDecompression(t *testing.T) {
	encodedBody := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	var nodeHits int
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nodeHits++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("RateLimit-Remaining", "48")
		w.Header().Set("X-Request-Id", "node-encoded-response")
		_, _ = w.Write(encodedBody)
	}))
	defer node.Close()

	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	})
	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
	request.Header.Set("X-Request-Id", "encoded-entry-request-id")
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !bytes.Equal(recorder.Body.Bytes(), encodedBody) {
		t.Fatalf("encoded body = %x, want %x", recorder.Body.Bytes(), encodedBody)
	}
	for header, want := range map[string]string{
		"Content-Encoding":    "gzip",
		"RateLimit-Remaining": "48",
		"X-Request-Id":        "encoded-entry-request-id",
	} {
		if got := recorder.Header().Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if nodeHits != 1 {
		t.Fatalf("nodeHits = %d, want 1", nodeHits)
	}
}

func TestHandlerRelaysLargeDriftResponseAfterBoundedPrefixInspection(t *testing.T) {
	firstChunk := append([]byte(growthPingResponse), '!')
	remainingBody := bytes.Repeat([]byte("x"), 128*1024)
	wantBody := append(append([]byte{}, firstChunk...), remainingBody...)
	nodeFirstChunkWritten := make(chan struct{})
	releaseNodeResponse := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseNodeResponse) })
	}
	defer release()

	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("RateLimit-Remaining", "47")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(firstChunk)
		w.(http.Flusher).Flush()
		close(nodeFirstChunkWritten)
		<-releaseNodeResponse
		_, _ = w.Write(remainingBody)
	}))
	defer node.Close()

	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	})
	recorder := newResponseStartRecorder()
	handlerDone := make(chan struct{})
	go func() {
		gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, growthPingPath, nil))
		close(handlerDone)
	}()

	select {
	case <-nodeFirstChunkWritten:
	case <-time.After(time.Second):
		t.Fatal("Node did not write the drift-response prefix")
	}
	select {
	case <-recorder.started:
		// 前缀不匹配时必须在尾部到达前就开始流式转发 Node 响应。
	case <-time.After(250 * time.Millisecond):
		release()
		<-handlerDone
		t.Fatal("gateway waited for the complete Node response before relaying it")
	}

	release()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("gateway did not finish relaying the Node response")
	}

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !bytes.Equal(recorder.Body.Bytes(), wantBody) {
		t.Fatalf("body length = %d, want %d bytes relayed without truncation or duplication", recorder.Body.Len(), len(wantBody))
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want Node value", got)
	}
	if got := recorder.Header().Get("RateLimit-Remaining"); got != "47" {
		t.Fatalf("RateLimit-Remaining = %q, want Node value", got)
	}
}

func TestHandlerUsesNodeEnvelopeForDefaultProxyUpstreamFailures(t *testing.T) {
	testCases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{
			name:    "context deadline exceeded",
			err:     context.DeadlineExceeded,
			status:  http.StatusGatewayTimeout,
			code:    "UPSTREAM_TIMEOUT",
			message: "Upstream service timed out",
		},
		{
			name:    "wrapped network timeout",
			err:     fmt.Errorf("proxy request failed: %w", timeoutNetError{}),
			status:  http.StatusGatewayTimeout,
			code:    "UPSTREAM_TIMEOUT",
			message: "Upstream service timed out",
		},
		{
			name:    "connection failure",
			err:     errors.New("dial tcp 127.0.0.1:3000: connect: connection refused"),
			status:  http.StatusBadGateway,
			code:    "UPSTREAM_BAD_RESPONSE",
			message: "Upstream service returned an invalid response",
		},
		{
			name:    "errors As timeout adapter",
			err:     timeoutAsAdapterError{},
			status:  http.StatusGatewayTimeout,
			code:    "UPSTREAM_TIMEOUT",
			message: "Upstream service timed out",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gateway := New(Config{DefaultUpstream: mustURL(t, "http://node.invalid")})
			gateway.defaultProxy.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, testCase.err
			})
			request := httptest.NewRequest(http.MethodGet, "/api/homepage/content", nil)
			request.Header.Set("X-Request-Id", "proxy-upstream-failure")
			recorder := httptest.NewRecorder()

			gateway.Handler().ServeHTTP(recorder, request)

			assertUpstreamFailureEnvelope(t, recorder, testCase.status, testCase.code, testCase.message, "proxy-upstream-failure", testCase.err.Error())
		})
	}
}

func TestHandlerReturnsBadResponseWhenAdmissionRequestFails(t *testing.T) {
	gateway := newEnabledGrowthPingGateway(t)
	admissionError := errors.New("dial tcp 127.0.0.1:3000: connect: connection refused")
	gateway.admissionClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, admissionError
	})}

	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
	request.Header.Set("X-Request-Id", "admission-request-failure")
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)

	assertUpstreamFailureEnvelope(t, recorder, http.StatusBadGateway, "UPSTREAM_BAD_RESPONSE", "Upstream service returned an invalid response", "admission-request-failure", admissionError.Error())
}

func TestHandlerReturnsTimeoutWhenAdmissionRequestFails(t *testing.T) {
	testCases := []struct {
		name string
		err  error
	}{
		{name: "context deadline exceeded", err: context.DeadlineExceeded},
		{name: "wrapped network timeout", err: fmt.Errorf("admission request failed: %w", timeoutNetError{})},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gateway := newEnabledGrowthPingGateway(t)
			gateway.admissionClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, testCase.err
			})}

			request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
			request.Header.Set("X-Request-Id", "admission-timeout")
			recorder := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(recorder, request)

			assertUpstreamFailureEnvelope(t, recorder, http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", "Upstream service timed out", "admission-timeout", testCase.err.Error())
		})
	}
}

func TestHandler使用配置的准入超时并保持Node错误信封(t *testing.T) {
	nodeRequestCancelled := make(chan struct{})
	node := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
			close(nodeRequestCancelled)
		case <-time.After(time.Second):
			t.Error("准入超时后 Node 请求没有被取消")
		}
	}))
	defer node.Close()

	gateway := New(Config{
		DefaultUpstream:  mustURL(t, node.URL),
		AdmissionTimeout: 20 * time.Millisecond,
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	})
	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
	request.Header.Set("X-Request-Id", "configured-admission-timeout")
	recorder := httptest.NewRecorder()

	gateway.Handler().ServeHTTP(recorder, request)

	assertUpstreamFailureEnvelope(
		t,
		recorder,
		http.StatusGatewayTimeout,
		"UPSTREAM_TIMEOUT",
		"Upstream service timed out",
		"configured-admission-timeout",
		"Client.Timeout exceeded",
	)
	select {
	case <-nodeRequestCancelled:
	case <-time.After(time.Second):
		t.Fatal("Gateway 返回超时后未取消 Node 准入请求")
	}
}

func TestHandlerReturnsBadResponseWhenAdmissionPrefixReadIsTruncated(t *testing.T) {
	body := &interruptedReadCloser{body: []byte(`{"par`)}
	gateway := newEnabledGrowthPingGateway(t)
	gateway.admissionClient = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          body,
			ContentLength: int64(growthPingInspectionLimit + 32),
			Request:       request,
		}, nil
	})}

	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
	request.Header.Set("X-Request-Id", "truncated-admission")
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)

	assertUpstreamFailureEnvelope(t, recorder, http.StatusBadGateway, "UPSTREAM_BAD_RESPONSE", "Upstream service returned an invalid response", "truncated-admission", io.ErrUnexpectedEOF.Error())
	if !body.closed {
		t.Fatal("truncated Node response body was not closed")
	}
}

func newEnabledGrowthPingGateway(t *testing.T) *Gateway {
	t.Helper()
	return New(Config{
		DefaultUpstream: mustURL(t, "http://node.invalid"),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	})
}

func assertUpstreamFailureEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, message, requestID, hiddenError string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d", recorder.Code, status)
	}
	if values := recorder.Result().Header.Values("X-Request-Id"); len(values) != 1 || values[0] != requestID {
		t.Fatalf("X-Request-Id values = %#v, want exactly [%s]", values, requestID)
	}
	if values := recorder.Result().Header.Values("Content-Type"); len(values) != 1 || values[0] != "application/json" {
		t.Fatalf("Content-Type values = %#v, want exactly [application/json]", values)
	}
	var envelope struct {
		Success   bool   `json:"success"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		Details   any    `json:"details"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode upstream failure envelope: %v", err)
	}
	if envelope.Success || envelope.Code != code || envelope.Message != message || envelope.Details != nil || envelope.RequestID != requestID {
		t.Fatalf("unexpected upstream failure envelope: %#v", envelope)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte(hiddenError)) {
		t.Fatalf("upstream failure envelope leaked internal error %q", hiddenError)
	}
}

func TestHandlerRelaysRedirectWithoutFollowingIt(t *testing.T) {
	var nodeHits int
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeHits++
		if r.URL.Path == "/redirect-target" {
			t.Fatal("admission client followed a Node redirect")
		}
		w.Header().Set("Location", "/redirect-target")
		w.Header().Set("X-Request-Id", "node-redirect-request-id")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "Node redirect")
	}))
	defer node.Close()

	gateway := New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	})
	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
	request.Header.Set("X-Request-Id", "redirect-entry-request-id")
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", recorder.Code)
	}
	if got := recorder.Header().Get("Location"); got != "/redirect-target" {
		t.Fatalf("Location = %q, want /redirect-target", got)
	}
	if got := recorder.Header().Get("X-Request-Id"); got != "redirect-entry-request-id" {
		t.Fatalf("X-Request-Id = %q, want entry request ID", got)
	}
	if got := recorder.Body.String(); got != "Node redirect" {
		t.Fatalf("body = %q, want Node redirect", got)
	}
	if nodeHits != 1 {
		t.Fatalf("nodeHits = %d, want exactly one admission request", nodeHits)
	}
}

func TestHandlerFallsBackWhenFileRouteSwitchFailsClosed(t *testing.T) {
	testCases := []struct {
		name  string
		setup func(t *testing.T, switchPath string)
	}{
		{
			name: "explicitly disabled route",
			setup: func(t *testing.T, switchPath string) {
				writeRouteSwitchFile(t, switchPath, `{"routes":{"GET /api/growth/ping":false}}`)
			},
		},
		{
			name: "missing route switch file",
			setup: func(_ *testing.T, _ string) {
				// 故意保持文件缺失：缺失开关绝不能意外放行本地流量。
			},
		},
		{
			name: "malformed route switch file",
			setup: func(t *testing.T, switchPath string) {
				writeRouteSwitchFile(t, switchPath, `{`)
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			switchPath := filepath.Join(t.TempDir(), "routes.json")
			testCase.setup(t, switchPath)

			const nodeBody = `{"success":true,"data":{"source":"node-fallback"}}`
			var nodeHits int
			node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nodeHits++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, nodeBody)
			}))
			defer node.Close()

			gateway := New(Config{
				DefaultUpstream: mustURL(t, node.URL),
				RouteSwitch:     NewFileRouteSwitch(switchPath),
			})
			recorder := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, growthPingPath, nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if got := recorder.Body.String(); got != nodeBody {
				t.Fatalf("body = %q, want raw Node fallback body %q", got, nodeBody)
			}
			if nodeHits != 1 {
				t.Fatalf("nodeHits = %d, want 1", nodeHits)
			}
		})
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestNewRejectsMissingUpstream(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New should reject missing default upstream")
		}
	}()
	_ = New(Config{})
}

func TestNewRequestIDUses32HexWhenRandomReadSucceeds(t *testing.T) {
	requestID := newRequestID()
	if len(requestID) != hex.EncodedLen(16) {
		t.Fatalf("request ID length = %d, want %d", len(requestID), hex.EncodedLen(16))
	}
	if _, err := hex.DecodeString(requestID); err != nil {
		t.Fatalf("request ID is not hexadecimal: %v", err)
	}
}

func TestNewRequestIDFallbackIsUniqueAfterRandomReadFails(t *testing.T) {
	previousRead := readRequestIDRandom
	readRequestIDRandom = func([]byte) (int, error) {
		return 0, errors.New("random source unavailable")
	}
	t.Cleanup(func() {
		readRequestIDRandom = previousRead
	})

	first := newRequestID()
	second := newRequestID()
	if len(first) < len("gateway-") || first[:len("gateway-")] != "gateway-" {
		t.Fatalf("first fallback request ID = %q, want gateway- prefix", first)
	}
	if len(second) < len("gateway-") || second[:len("gateway-")] != "gateway-" {
		t.Fatalf("second fallback request ID = %q, want gateway- prefix", second)
	}
	if first == second {
		t.Fatalf("fallback request IDs must differ, got %q twice", first)
	}
}
