package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandlerServesGrowthPingCanaryAfterNodeAdmission(t *testing.T) {
	nodeHits := 0
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeHits++
		if r.Method != http.MethodGet || r.URL.Path != growthPingPath {
			t.Fatalf("Node request = %s %s, want GET %s", r.Method, r.URL.Path, growthPingPath)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(growthPingResponse))
	}))
	defer node.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)
	request.Header.Set("X-Request-Id", "growth-ping-test")

	New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	}).Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	requestIDs := recorder.Header().Values("X-Request-Id")
	if len(requestIDs) != 1 {
		t.Fatalf("request id values = %q, want one value", requestIDs)
	}
	if got := requestIDs[0]; got != "growth-ping-test" {
		t.Fatalf("request id = %q, want growth-ping-test", got)
	}
	if got := recorder.Body.String(); got != "{\"success\":true,\"data\":{\"ok\":true}}" {
		t.Fatalf("body = %q, want growth ping envelope", got)
	}
	if nodeHits != 1 {
		t.Fatalf("nodeHits = %d, want 1 admission request", nodeHits)
	}
}

func TestHandlerFallsBackOutsideGrowthPingCanary(t *testing.T) {
	const nodeBody = "{\"success\":true,\"data\":{\"source\":\"node\"}}"
	nodeHits := 0
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nodeHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nodeBody))
	}))
	defer node.Close()

	testCases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "other method", method: http.MethodPost, path: growthPingPath},
		{name: "trailing slash", method: http.MethodGet, path: "/api/growth/ping/"},
		{name: "nested path", method: http.MethodGet, path: "/api/growth/ping/extra"},
		{name: "versioned path", method: http.MethodGet, path: "/api/v1/growth/ping"},
		{name: "escaped slash", method: http.MethodGet, path: "/api/growth%2Fping"},
		{name: "escaped character", method: http.MethodGet, path: "/api/growth/%70ing"},
	}

	gateway := New(Config{DefaultUpstream: mustURL(t, node.URL)})
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			recorder := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("%s %s status = %d, want 200", request.Method, request.URL.EscapedPath(), recorder.Code)
			}
			if got := recorder.Body.String(); got != nodeBody {
				t.Fatalf("%s %s body = %q, want Node fallback", request.Method, request.URL.EscapedPath(), got)
			}
		})
	}

	if nodeHits != len(testCases) {
		t.Fatalf("nodeHits = %d, want %d", nodeHits, len(testCases))
	}
}

func TestHandlerFallsBackToNodeWhenRouteSwitchIsNotConfigured(t *testing.T) {
	const nodeBody = "{\"success\":true,\"data\":{\"source\":\"node-default\"}}"
	var nodeHits int
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nodeHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nodeBody))
	}))
	defer node.Close()

	recorder := httptest.NewRecorder()
	New(Config{DefaultUpstream: mustURL(t, node.URL)}).Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, growthPingPath, nil),
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Body.String(); got != nodeBody {
		t.Fatalf("body = %q, want Node fallback body %q", got, nodeBody)
	}
	if nodeHits != 1 {
		t.Fatalf("nodeHits = %d, want 1", nodeHits)
	}
}

func TestHandlerAssignsRequestIDToGrowthPingCanary(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(growthPingResponse))
	}))
	defer node.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, growthPingPath, nil)

	New(Config{
		DefaultUpstream: mustURL(t, node.URL),
		RouteSwitch: enabledRouteSwitch{enabled: exactRouteKey{
			method: http.MethodGet,
			path:   growthPingPath,
		}},
	}).Handler().ServeHTTP(recorder, request)

	requestIDs := recorder.Header().Values("X-Request-Id")
	if len(requestIDs) != 1 || requestIDs[0] == "" {
		t.Fatalf("request id values = %q, want one generated value", requestIDs)
	}
}
