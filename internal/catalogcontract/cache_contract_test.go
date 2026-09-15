package catalogcontract

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func Test回放拒绝DataStale合同差异(t *testing.T) {
	testCases := []struct {
		name             string
		baselineHeaders  http.Header
		candidateHeaders http.Header
		privateValue     string
	}{
		{
			name:             "出现性不一致",
			baselineHeaders:  http.Header{"X-Data-Stale": {"1"}},
			candidateHeaders: make(http.Header),
		},
		{
			name:             "基线值非法",
			baselineHeaders:  http.Header{"X-Data-Stale": {"baseline-private-stale"}},
			candidateHeaders: http.Header{"X-Data-Stale": {"1"}},
			privateValue:     "baseline-private-stale",
		},
		{
			name:             "候选值重复",
			baselineHeaders:  http.Header{"X-Data-Stale": {"1"}},
			candidateHeaders: http.Header{"X-Data-Stale": {"1", "1"}},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := newCacheContractReplayServer(t, testCase.baselineHeaders)
			defer baseline.Close()
			candidate := newCacheContractReplayServer(t, testCase.candidateHeaders)
			defer candidate.Close()

			err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
				Name:   "data-stale-" + testCase.name,
				Path:   "/api/homepage/video-templates",
				Header: http.Header{"X-Request-Id": {"catalog-contract-data-stale"}},
			})
			if err == nil {
				t.Fatal("回放必须拒绝 X-Data-Stale 契约差异")
			}
			if !strings.Contains(err.Error(), "X-Data-Stale") {
				t.Fatalf("回放错误必须仅标识 X-Data-Stale 差异类别：%q", err)
			}
			if testCase.privateValue != "" && strings.Contains(err.Error(), testCase.privateValue) {
				t.Fatalf("回放错误不得泄露 X-Data-Stale 私有值：%q", err)
			}
		})
	}
}

func Test回放执行必需CacheStatus合同(t *testing.T) {
	testCases := []struct {
		name             string
		baselineHeaders  http.Header
		candidateHeaders http.Header
		privateValues    []string
	}{
		{
			name:             "基线缺失",
			baselineHeaders:  make(http.Header),
			candidateHeaders: http.Header{"X-Cache-Status": {"hit"}},
		},
		{
			name:             "候选缺失",
			baselineHeaders:  http.Header{"X-Cache-Status": {"miss"}},
			candidateHeaders: make(http.Header),
		},
		{
			name:             "两侧均缺失",
			baselineHeaders:  make(http.Header),
			candidateHeaders: make(http.Header),
		},
		{
			name:             "基线重复",
			baselineHeaders:  http.Header{"X-Cache-Status": {"baseline-private-cache", "baseline-private-cache"}},
			candidateHeaders: http.Header{"X-Cache-Status": {"hit"}},
			privateValues:    []string{"baseline-private-cache"},
		},
		{
			name:             "候选重复",
			baselineHeaders:  http.Header{"X-Cache-Status": {"miss"}},
			candidateHeaders: http.Header{"X-Cache-Status": {"candidate-private-cache", "candidate-private-cache"}},
			privateValues:    []string{"candidate-private-cache"},
		},
		{
			name:             "基线值为空",
			baselineHeaders:  http.Header{"X-Cache-Status": {""}},
			candidateHeaders: http.Header{"X-Cache-Status": {"candidate-private-cache"}},
			privateValues:    []string{"candidate-private-cache"},
		},
		{
			name:             "候选值为空",
			baselineHeaders:  http.Header{"X-Cache-Status": {"baseline-private-cache"}},
			candidateHeaders: http.Header{"X-Cache-Status": {""}},
			privateValues:    []string{"baseline-private-cache"},
		},
		{
			name:             "两侧值均为空",
			baselineHeaders:  http.Header{"X-Cache-Status": {""}},
			candidateHeaders: http.Header{"X-Cache-Status": {""}},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := newCacheContractReplayServer(t, testCase.baselineHeaders)
			defer baseline.Close()
			candidate := newCacheContractReplayServer(t, testCase.candidateHeaders)
			defer candidate.Close()

			err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
				Name:          "required-cache-status-" + testCase.name,
				Path:          "/api/homepage/video-templates",
				Header:        cacheContractRequestHeaders(),
				CacheContract: CacheContract{RequireCacheStatus: true},
			})
			assertCacheContractError(t, err, "X-Cache-Status is missing or invalid", testCase.privateValues...)
		})
	}
}

func Test回放执行ETag策略(t *testing.T) {
	testCases := []struct {
		name             string
		contract         CacheContract
		baselineHeaders  http.Header
		candidateHeaders http.Header
		wantCategory     string
		privateValues    []string
	}{
		{
			name:             "禁止 ETag 时基线携带",
			contract:         CacheContract{ETagPolicy: ETagForbidden},
			baselineHeaders:  http.Header{"ETag": {"baseline-private-etag"}},
			candidateHeaders: make(http.Header),
			wantCategory:     "ETag must be absent",
			privateValues:    []string{"baseline-private-etag"},
		},
		{
			name:             "禁止 ETag 时候选携带",
			contract:         CacheContract{ETagPolicy: ETagForbidden},
			baselineHeaders:  make(http.Header),
			candidateHeaders: http.Header{"ETag": {"candidate-private-etag"}},
			wantCategory:     "ETag must be absent",
			privateValues:    []string{"candidate-private-etag"},
		},
		{
			name:             "要求 ETag 时基线缺失",
			contract:         CacheContract{ETagPolicy: ETagRequired},
			baselineHeaders:  make(http.Header),
			candidateHeaders: http.Header{"ETag": {"candidate-private-etag"}},
			wantCategory:     "ETag is missing or invalid",
			privateValues:    []string{"candidate-private-etag"},
		},
		{
			name:             "要求 ETag 时基线重复",
			contract:         CacheContract{ETagPolicy: ETagRequired},
			baselineHeaders:  http.Header{"ETag": {"baseline-private-etag-a", "baseline-private-etag-b"}},
			candidateHeaders: http.Header{"ETag": {"candidate-private-etag"}},
			wantCategory:     "ETag is missing or invalid",
			privateValues:    []string{"baseline-private-etag-a", "baseline-private-etag-b", "candidate-private-etag"},
		},
		{
			name:             "要求 ETag 时候选为空",
			contract:         CacheContract{ETagPolicy: ETagRequired},
			baselineHeaders:  http.Header{"ETag": {"baseline-private-etag"}},
			candidateHeaders: http.Header{"ETag": {""}},
			wantCategory:     "ETag is missing or invalid",
			privateValues:    []string{"baseline-private-etag"},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			baseline := newCacheContractReplayServer(t, testCase.baselineHeaders)
			defer baseline.Close()
			candidate := newCacheContractReplayServer(t, testCase.candidateHeaders)
			defer candidate.Close()

			err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
				Name:          "etag-policy-" + testCase.name,
				Path:          "/api/homepage/video-templates",
				Header:        cacheContractRequestHeaders(),
				CacheContract: testCase.contract,
			})
			assertCacheContractError(t, err, testCase.wantCategory, testCase.privateValues...)
		})
	}
}

func Test回放允许独立CacheStatus值不同(t *testing.T) {
	baseline := newCacheContractReplayServer(t, http.Header{"X-Cache-Status": {"miss"}})
	defer baseline.Close()
	candidate := newCacheContractReplayServer(t, http.Header{"X-Cache-Status": {"hit"}})
	defer candidate.Close()

	err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name:          "required-cache-status-different-values",
		Path:          "/api/homepage/video-templates",
		Header:        cacheContractRequestHeaders(),
		CacheContract: CacheContract{RequireCacheStatus: true},
	})
	if err != nil {
		t.Fatalf("独立缓存的 X-Cache-Status 值可以不同：%v", err)
	}
}

func Test零值缓存合同只校验CacheStatus出现性(t *testing.T) {
	baseline := newCacheContractReplayServer(t, http.Header{"X-Cache-Status": {""}})
	defer baseline.Close()
	candidate := newCacheContractReplayServer(t, http.Header{"X-Cache-Status": {""}})
	defer candidate.Close()

	err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name:   "default-cache-status-empty-values",
		Path:   "/api/homepage/video-templates",
		Header: cacheContractRequestHeaders(),
	})
	if err != nil {
		t.Fatalf("零值缓存合同只要求 X-Cache-Status 出现性一致：%v", err)
	}
}

func Test回放允许要求唯一非空ETag(t *testing.T) {
	baseline := newCacheContractReplayServer(t, http.Header{"ETag": {"baseline-etag"}})
	defer baseline.Close()
	candidate := newCacheContractReplayServer(t, http.Header{"ETag": {"candidate-etag"}})
	defer candidate.Close()

	err := Replay(context.Background(), baseline.Client(), baseline.URL, candidate.URL, RequestCase{
		Name:          "required-etag-unique-nonempty",
		Path:          "/api/homepage/content",
		Header:        cacheContractRequestHeaders(),
		CacheContract: CacheContract{ETagPolicy: ETagRequired},
	})
	if err != nil {
		t.Fatalf("要求 ETag 时应接受两个唯一非空值：%v", err)
	}
}

func cacheContractRequestHeaders() http.Header {
	return http.Header{
		"Authorization":       {"Bearer authorization-secret"},
		"Cookie":              {"session=cookie-secret"},
		"Proxy-Authorization": {"Basic proxy-secret"},
		"X-Request-Id":        {"catalog-contract-cache-policy"},
	}
}

func assertCacheContractError(t *testing.T, err error, wantCategory string, privateValues ...string) {
	t.Helper()

	if err == nil {
		t.Fatalf("回放必须拒绝缓存合同差异：%s", wantCategory)
	}
	if !strings.Contains(err.Error(), "compare") || !strings.Contains(err.Error(), wantCategory) {
		t.Fatalf("回放错误必须只包含差异类别 %q：%q", wantCategory, err)
	}
	for _, privateValue := range append(privateValues, "authorization-secret", "cookie-secret", "proxy-secret", `{"success":true,"data":[]}`) {
		if strings.Contains(err.Error(), privateValue) {
			t.Fatalf("回放错误不得泄露私有值：%q", err)
		}
	}
}

func Test回放禁止自动跟随重定向(t *testing.T) {
	baselineInitialRequests := make(chan struct{}, 2)
	baselineRedirectRequests := make(chan struct{}, 2)
	baseline := newRedirectReplayServer(t, baselineInitialRequests, baselineRedirectRequests, `{"success":true,"data":"baseline-private-target"}`)
	defer baseline.Close()
	candidateInitialRequests := make(chan struct{}, 2)
	candidateRedirectRequests := make(chan struct{}, 2)
	candidate := newRedirectReplayServer(t, candidateInitialRequests, candidateRedirectRequests, `{"success":true,"data":"candidate-private-target"}`)
	defer candidate.Close()

	client := baseline.Client()
	if client.CheckRedirect != nil {
		t.Fatal("测试调用方客户端不得预设重定向策略")
	}
	err := Replay(context.Background(), client, baseline.URL, candidate.URL, RequestCase{
		Name:   "redirect-initial-response",
		Path:   "/api/homepage/video-templates",
		Header: http.Header{"X-Request-Id": {"catalog-contract-redirect"}},
	})
	if err != nil {
		t.Fatalf("回放必须比较两个初始 3xx 响应：%v", err)
	}
	if client.CheckRedirect != nil {
		t.Fatal("回放不得修改调用方客户端的重定向策略")
	}
	for _, upstream := range []struct {
		name             string
		initialRequests  <-chan struct{}
		redirectRequests <-chan struct{}
	}{
		{name: "baseline", initialRequests: baselineInitialRequests, redirectRequests: baselineRedirectRequests},
		{name: "candidate", initialRequests: candidateInitialRequests, redirectRequests: candidateRedirectRequests},
	} {
		if count := len(upstream.initialRequests); count != 1 {
			t.Fatalf("%s 上游初始请求数为 %d，期望为 1", upstream.name, count)
		}
		if count := len(upstream.redirectRequests); count != 0 {
			t.Fatalf("%s 上游不得访问重定向目标，实际请求数为 %d", upstream.name, count)
		}
	}
}

func newCacheContractReplayServer(t *testing.T, responseHeaders http.Header) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		for name, values := range responseHeaders {
			for _, value := range values {
				writer.Header().Add(name, value)
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "public, max-age=60")
		writer.Header().Set("X-Request-Id", request.Header.Get("X-Request-Id"))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"success":true,"data":[]}`))
	}))
}

func newRedirectReplayServer(t *testing.T, initialRequests, redirectRequests chan<- struct{}, redirectBody string) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/homepage/video-templates":
			initialRequests <- struct{}{}
			writer.Header().Set("Location", "/redirect-target")
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("X-Request-Id", request.Header.Get("X-Request-Id"))
			writer.WriteHeader(http.StatusFound)
			_, _ = writer.Write([]byte(`{"success":true,"data":[]}`))
		case "/redirect-target":
			redirectRequests <- struct{}{}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("X-Request-Id", request.Header.Get("X-Request-Id"))
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(redirectBody))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
}
