package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler将精确生成回调交给本地处理器(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	called := false
	gateway := New(Config{
		DefaultUpstream: mustURL(t, upstream.URL),
		GenerationCallback: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}),
	})
	recorder := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/internal/generation-callback", nil))

	if !called {
		t.Fatal("本地回调处理器未被调用")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if upstreamHits != 0 {
		t.Fatalf("Node upstream hits = %d, want 0", upstreamHits)
	}
}

func TestHandler仅将精确生成回调交给本地处理器(t *testing.T) {
	upstream := newCallbackTestUpstream(t)
	defer upstream.Close()

	testCases := []struct {
		name   string
		method string
		target string
		want   bool
	}{
		{name: "legacy 路径", method: http.MethodPost, target: "/api/internal/generation-callback", want: true},
		{name: "GET", method: http.MethodGet, target: "/api/v1/internal/generation-callback", want: false},
		{name: "query", method: http.MethodPost, target: "/api/v1/internal/generation-callback?attempt=1", want: false},
		{name: "空 query", method: http.MethodPost, target: "/api/v1/internal/generation-callback?", want: false},
		{name: "编码斜杠", method: http.MethodPost, target: "/api/v1/internal%2fgeneration-callback", want: false},
		{name: "编码点", method: http.MethodPost, target: "/api/v1/internal/%2e%2e/internal/generation-callback", want: false},
		{name: "点路径", method: http.MethodPost, target: "/api/v1/internal/../internal/generation-callback", want: false},
		{name: "重复斜杠", method: http.MethodPost, target: "/api/v1//internal/generation-callback", want: false},
		{name: "其他路径", method: http.MethodPost, target: "/api/v1/internal/generation-callback/extra", want: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			gateway := New(Config{
				DefaultUpstream: mustURL(t, upstream.URL),
				GenerationCallback: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					w.WriteHeader(http.StatusNoContent)
				}),
			})
			recorder := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(testCase.method, testCase.target, nil))

			if called != testCase.want {
				t.Fatalf("local callback called = %t, want %t", called, testCase.want)
			}
			if testCase.want {
				if recorder.Code != http.StatusNoContent {
					t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
				}
				return
			}
			if recorder.Code != http.StatusAccepted || recorder.Body.String() != "node" {
				t.Fatalf("response = %d %q, want Node fallback", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandler将网关请求ID交给本地生成回调处理器(t *testing.T) {
	testCases := []struct {
		name              string
		incomingRequestID string
		wantRequestID     string
	}{
		{name: "生成请求 ID"},
		{name: "保留请求 ID", incomingRequestID: "entry-request-id", wantRequestID: "entry-request-id"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := newCallbackTestUpstream(t)
			defer upstream.Close()

			var gotRequestID string
			gateway := New(Config{
				DefaultUpstream: mustURL(t, upstream.URL),
				GenerationCallback: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotRequestID = r.Header.Get("X-Request-Id")
					w.WriteHeader(http.StatusNoContent)
				}),
			})
			request := httptest.NewRequest(http.MethodPost, generationCallbackPathV1, nil)
			if testCase.incomingRequestID != "" {
				request.Header.Set("X-Request-Id", testCase.incomingRequestID)
			}
			recorder := httptest.NewRecorder()

			gateway.Handler().ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
			}
			if gotRequestID == "" {
				t.Fatal("本地回调处理器未收到网关请求 ID")
			}
			if testCase.wantRequestID != "" && gotRequestID != testCase.wantRequestID {
				t.Fatalf("request ID = %q, want %q", gotRequestID, testCase.wantRequestID)
			}
		})
	}
}

func newCallbackTestUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("node"))
	}))
}
