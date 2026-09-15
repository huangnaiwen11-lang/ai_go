package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMatchNotificationRouteUsesDynamicPlaceholder(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   exactRouteKey
	}{
		{http.MethodGet, "/api/notifications", exactRouteKey{http.MethodGet, "/api/notifications"}},
		{http.MethodPost, "/api/notifications/n-1/read", exactRouteKey{http.MethodPost, "/api/notifications/:id/read"}},
		{http.MethodDelete, "/api/notifications/n-1", exactRouteKey{http.MethodDelete, "/api/notifications/:id"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+testCase.path, func(t *testing.T) {
			route, ok := matchNotificationRoute(httptest.NewRequest(testCase.method, testCase.path, nil))
			if !ok || route != testCase.want {
				t.Fatalf("route=%+v ok=%v want=%+v", route, ok, testCase.want)
			}
			if !isConfirmedExactRoute(route) {
				t.Fatalf("route %+v is not an approved exact route", route)
			}
		})
	}
	if route, ok := matchNotificationRoute(httptest.NewRequest(http.MethodGet, "/api/notifications?limit=20", nil)); !ok || route.path != "/api/notifications" {
		t.Fatalf("notification list query unexpectedly rejected: route=%+v ok=%v", route, ok)
	}
	if _, ok := matchNotificationRoute(httptest.NewRequest(http.MethodDelete, "/api/notifications/n-1?x=1", nil)); ok {
		t.Fatal("notification delete query unexpectedly matched")
	}
}

func TestMatchNotificationRouteRejectsNestedOrEncodedIDs(t *testing.T) {
	for _, path := range []string{"/api/notifications/a/b/read", "/api/notifications/a%2Fb/read", "/api/notifications/"} {
		if _, ok := matchNotificationRoute(httptest.NewRequest(http.MethodPost, path, nil)); ok {
			t.Fatalf("path %q unexpectedly matched", path)
		}
	}
}
