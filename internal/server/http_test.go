package server

import (
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"ai-business-service/internal/conf"
)

func TestHTTPServerDoesNotExposeTodoRoutes(t *testing.T) {
	srv := newTestHTTPServer(t)
	res := httptest.NewRecorder()
	requestID := "req-todo-route-404"
	req := httptest.NewRequest(stdhttp.MethodGet, "/v1/todos/list", nil)
	req.Header.Set("X-Request-Id", requestID)

	srv.ServeHTTP(res, req)

	assertRootErrorEnvelope(t, res, stdhttp.StatusNotFound, "NOT_FOUND", requestID)
}

func TestHTTPServerRejectsUnsupportedMethodWithRootEnvelope(t *testing.T) {
	srv := newTestHTTPServer(t)
	res := httptest.NewRecorder()
	requestID := "req-health-method-405"
	req := httptest.NewRequest(stdhttp.MethodPost, "/healthz", nil)
	req.Header.Set("X-Request-Id", requestID)

	srv.ServeHTTP(res, req)

	assertRootErrorEnvelope(t, res, stdhttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", requestID)
}

func TestHTTPServerExposesLivenessProbe(t *testing.T) {
	srv := newTestHTTPServer(t)
	res := httptest.NewRecorder()

	srv.ServeHTTP(res, httptest.NewRequest(stdhttp.MethodGet, "/healthz", nil))

	if res.Code != stdhttp.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, stdhttp.StatusOK)
	}

	var body struct {
		Success bool   `json:"success"`
		Data    string `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Success || body.Data != "ok" {
		t.Fatalf("body = %#v, want successful liveness envelope", body)
	}
	if res.Header().Get("X-Request-Id") == "" {
		t.Fatal("response is missing X-Request-Id")
	}
}

func TestHTTPServerPreservesValidRequestID(t *testing.T) {
	srv := newTestHTTPServer(t)
	res := httptest.NewRecorder()
	requestID := "b3d60f1a-73a2-4b8d-86fb-b5655ccaf2a7"
	req := httptest.NewRequest(stdhttp.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-Id", requestID)

	srv.ServeHTTP(res, req)

	if got := res.Header().Get("X-Request-Id"); got != requestID {
		t.Fatalf("X-Request-Id = %q, want %q", got, requestID)
	}
}

func TestNewHTTPServerHandlesMissingHTTPConfig(t *testing.T) {
	cases := []struct {
		name   string
		server *conf.Server
	}{
		{
			name: "缺少 Server",
		},
		{
			name:   "缺少 Server HTTP",
			server: &conf.Server{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("NewHTTPServer() panic = %v", recovered)
				}
			}()

			srv := NewHTTPServer(tc.server)
			res := httptest.NewRecorder()
			srv.ServeHTTP(res, httptest.NewRequest(stdhttp.MethodGet, "/healthz", nil))
			if res.Code != stdhttp.StatusOK {
				t.Fatalf("status = %d, want %d", res.Code, stdhttp.StatusOK)
			}
		})
	}
}

func newTestHTTPServer(t *testing.T) *HTTPServerForTest {
	t.Helper()
	return &HTTPServerForTest{server: NewHTTPServer(&conf.Server{Http: &conf.Server_HTTP{}})}
}

// HTTPServerForTest 收窄测试依赖，仅暴露路由分发能力。
type HTTPServerForTest struct {
	server interface {
		ServeHTTP(stdhttp.ResponseWriter, *stdhttp.Request)
	}
}

func (s *HTTPServerForTest) ServeHTTP(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	s.server.ServeHTTP(writer, request)
}

// assertRootErrorEnvelope 统一校验现网失败响应结构，避免 404/405 退化为框架默认文本。
func assertRootErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode, wantRequestID string) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d", recorder.Code, wantStatus)
	}
	if contentType := recorder.Result().Header.Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want JSON contract", contentType)
	}

	var body errorEnvelope
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body.Success {
		t.Fatal("success = true, want false")
	}
	if body.Code != wantCode {
		t.Fatalf("code = %q, want %q", body.Code, wantCode)
	}
	if body.Details != nil {
		t.Fatalf("details = %#v, want nil", body.Details)
	}
	if body.RequestID != wantRequestID {
		t.Fatalf("requestId = %q, want %q", body.RequestID, wantRequestID)
	}
}
