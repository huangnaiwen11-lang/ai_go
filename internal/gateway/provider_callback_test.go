package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// B2B 回调路径是发给平台的外部合同（callbackUrl 的路径部分），改动它就是改合同。
// 这条断言的作用是让任何改动都必须被显式看到，而不是顺手改掉。
func TestProviderCallbackPath与外部合同一致(t *testing.T) {
	if providerCallbackPath != "/api/v1/internal/polarstar-callback" {
		t.Fatalf("providerCallbackPath = %q，与发给平台的 callbackUrl 路径不一致", providerCallbackPath)
	}
}

func TestHandler将精确B2B回调交给本地处理器(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	called := false
	instance := New(Config{
		DefaultUpstream: mustURL(t, upstream.URL),
		ProviderCallback: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}),
	})
	recorder := httptest.NewRecorder()
	instance.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, providerCallbackPath, nil))

	if !called {
		t.Fatal("本地 B2B 回调处理器未被调用")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if upstreamHits != 0 {
		t.Fatalf("Node upstream hits = %d, want 0", upstreamHits)
	}
}

func TestHandler仅将精确B2B回调交给本地处理器(t *testing.T) {
	upstream := newCallbackTestUpstream(t)
	defer upstream.Close()

	testCases := []struct {
		name   string
		method string
		target string
		want   bool
	}{
		{name: "精确路径", method: http.MethodPost, target: providerCallbackPath, want: true},
		{name: "GET", method: http.MethodGet, target: providerCallbackPath, want: false},
		{name: "query", method: http.MethodPost, target: providerCallbackPath + "?attempt=1", want: false},
		{name: "空 query", method: http.MethodPost, target: providerCallbackPath + "?", want: false},
		{name: "编码斜杠", method: http.MethodPost, target: "/api/v1/internal%2fpolarstar-callback", want: false},
		{name: "编码点", method: http.MethodPost, target: "/api/v1/internal/%2e%2e/internal/polarstar-callback", want: false},
		{name: "点路径", method: http.MethodPost, target: "/api/v1/internal/../internal/polarstar-callback", want: false},
		{name: "重复斜杠", method: http.MethodPost, target: "/api/v1//internal/polarstar-callback", want: false},
		{name: "尾随斜杠", method: http.MethodPost, target: providerCallbackPath + "/", want: false},
		{name: "路径前缀相同", method: http.MethodPost, target: "/api/v1/internal/polarstar-callback-extra", want: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			instance := New(Config{
				DefaultUpstream: mustURL(t, upstream.URL),
				ProviderCallback: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					called = true
					w.WriteHeader(http.StatusNoContent)
				}),
			})
			recorder := httptest.NewRecorder()
			instance.Handler().ServeHTTP(recorder, httptest.NewRequest(testCase.method, testCase.target, nil))

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

// 两套回调是不同合同：同时装配时各自的路径必须只命中自己的处理器。
// 若它们互相串台，一份 execution.v2 报文会进入 B2B 验签器（必然失败），
// 而一次 B2B 投递会被旧合同的关联逻辑当成「找不到步骤」丢弃。
func TestHandler两套回调互不串台(t *testing.T) {
	upstream := newCallbackTestUpstream(t)
	defer upstream.Close()

	var generationCalls, providerCalls int
	instance := New(Config{
		DefaultUpstream: mustURL(t, upstream.URL),
		GenerationCallback: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			generationCalls++
			w.WriteHeader(http.StatusNoContent)
		}),
		ProviderCallback: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			providerCalls++
			w.WriteHeader(http.StatusNoContent)
		}),
	})

	instance.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, generationCallbackPathV1, nil))
	instance.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, providerCallbackPath, nil))

	if generationCalls != 1 || providerCalls != 1 {
		t.Fatalf("生成回调命中 %d 次、B2B 回调命中 %d 次，want 1/1", generationCalls, providerCalls)
	}
}

// 未配置时 B2B 回调路径必须继续走 Node 代理，不能在本地造一条兜底路径。
func TestHandler未配置时B2B回调回退Node(t *testing.T) {
	upstream := newCallbackTestUpstream(t)
	defer upstream.Close()

	instance := New(Config{DefaultUpstream: mustURL(t, upstream.URL)})
	recorder := httptest.NewRecorder()
	instance.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, providerCallbackPath, nil))

	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "node" {
		t.Fatalf("response = %d %q, want Node fallback", recorder.Code, recorder.Body.String())
	}
}
