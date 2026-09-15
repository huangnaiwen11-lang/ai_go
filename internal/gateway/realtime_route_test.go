package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerationStreamRouteIsExact(t *testing.T) {
	if !isConfirmedExactRoute(exactRouteKey{method: http.MethodGet, path: "/api/users/me/generations/stream"}) {
		t.Fatal("generation stream route should be registered as an exact local route")
	}
	for _, path := range []string{"/api/users/me/generations/stream/", "/api/users/me/generations/stream/other"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if request.URL.Path == generationStreamRoute.path {
			t.Fatalf("test path unexpectedly matched exact route: %s", path)
		}
	}
}
