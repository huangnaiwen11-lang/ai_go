package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorks仅在精确路由开关与处理器同时启用时接管(t *testing.T) {
	var nodeHits, localHits int
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nodeHits++
		_, _ = io.WriteString(writer, "node")
	}))
	defer node.Close()
	local := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		localHits++
		_, _ = io.WriteString(writer, "go-works")
	})

	disabled := New(Config{DefaultUpstream: mustURL(t, node.URL), WorksHandler: local})
	disabledRecorder := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(disabledRecorder, httptest.NewRequest(http.MethodGet, "/api/works?kind=image&limit=20", nil))
	if disabledRecorder.Body.String() != "node" || localHits != 0 || nodeHits != 1 {
		t.Fatalf("未启用时 response=%q local=%d node=%d", disabledRecorder.Body.String(), localHits, nodeHits)
	}

	for _, testCase := range []struct {
		target string
		route  exactRouteKey
	}{
		{target: "/api/works?kind=image&limit=20", route: exactRouteKey{method: http.MethodGet, path: "/api/works"}},
		{target: "/api/works/work-1", route: exactRouteKey{method: http.MethodGet, path: "/api/works/:id"}},
	} {
		enabled := New(Config{
			DefaultUpstream: mustURL(t, node.URL),
			RouteSwitch:     enabledRouteSwitch{enabled: testCase.route},
			WorksHandler:    local,
		})
		recorder := httptest.NewRecorder()
		enabled.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.target, nil))
		if recorder.Body.String() != "go-works" {
			t.Fatalf("target=%q body=%q，期望 Go 接管", testCase.target, recorder.Body.String())
		}
	}
	enabled := New(Config{DefaultUpstream: mustURL(t, node.URL), RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{method: http.MethodGet, path: "/api/works"}}, WorksHandler: local})
	for _, target := range []string{"/api/works/", "/api/works%2Fwork-1", "/api/v1/works", "/api/works/work-1?x=1"} {
		recorder := httptest.NewRecorder()
		enabled.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Body.String() != "node" {
			t.Fatalf("target=%q body=%q，期望代理 Node", target, recorder.Body.String())
		}
	}
	if localHits != 2 || nodeHits != 5 {
		t.Fatalf("final local=%d node=%d", localHits, nodeHits)
	}
}
