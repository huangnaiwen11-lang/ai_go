package gateway

import (
	"net/http"
	"testing"
)

func TestWorksRoutes已登记为精确本地路由(t *testing.T) {
	for _, route := range []exactRouteKey{
		{method: http.MethodGet, path: "/api/works"},
		{method: http.MethodGet, path: "/api/works/:id"},
	} {
		if !isConfirmedExactRoute(route) {
			t.Fatalf("route %#v 未登记为精确本地路由", route)
		}
	}
}
