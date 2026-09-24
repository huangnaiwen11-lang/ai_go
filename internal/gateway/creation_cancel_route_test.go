package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreationCancelRoute已登记为精确本地路由(t *testing.T) {
	if !isConfirmedExactRoute(exactRouteKey{method: http.MethodPost, path: "/api/creations/:id/cancel"}) {
		t.Fatal("creation cancellation route is not registered as an exact local route")
	}
}

func TestGatewayCreationCancel仅在处理器与精确开关同时启用时接管(t *testing.T) {
	var nodeHits, localHits int
	node := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		nodeHits++
		_, _ = io.WriteString(writer, "node")
	}))
	defer node.Close()
	local := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		localHits++
		_, _ = io.WriteString(writer, "go-cancel")
	})
	route := exactRouteKey{method: http.MethodPost, path: "/api/creations/:id/cancel"}

	disabled := New(Config{DefaultUpstream: mustURL(t, node.URL), CreationCancelHandler: local})
	disabledRecorder := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(disabledRecorder, httptest.NewRequest(http.MethodPost, "/api/creations/creation-1/cancel", nil))
	if disabledRecorder.Body.String() != "node" || localHits != 0 || nodeHits != 1 {
		t.Fatalf("disabled response/local/node = %q/%d/%d", disabledRecorder.Body.String(), localHits, nodeHits)
	}

	enabled := New(Config{DefaultUpstream: mustURL(t, node.URL), RouteSwitch: enabledRouteSwitch{enabled: route}, CreationCancelHandler: local})
	recorder := httptest.NewRecorder()
	enabled.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/creations/creation-1/cancel", nil))
	if recorder.Body.String() != "go-cancel" || localHits != 1 {
		t.Fatalf("enabled response/local = %q/%d", recorder.Body.String(), localHits)
	}

	for _, target := range []string{
		"/api/creations/creation-1/cancel?ignored=true",
		"/api/creations/creation-1/cancel/extra",
		"/api/creations//cancel",
		"/api/creations%2Fcreation-1%2Fcancel",
	} {
		recorder := httptest.NewRecorder()
		enabled.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
		if recorder.Body.String() != "node" {
			t.Fatalf("target=%q response=%q, want Node proxy", target, recorder.Body.String())
		}
	}
	if localHits != 1 || nodeHits != 5 {
		t.Fatalf("final local/node = %d/%d", localHits, nodeHits)
	}
}
