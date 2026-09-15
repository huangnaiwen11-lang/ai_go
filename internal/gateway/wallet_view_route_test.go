package gateway

import (
	"net/http"
	"testing"
)

// 钱包读取只能由两个固定 GET 路径接管，不能用前缀匹配扩大 Go 的路由范围。
func TestWalletViewRoutes已登记为精确本地路由(t *testing.T) {
	for _, route := range []exactRouteKey{
		{method: http.MethodGet, path: "/api/wallet/summary"},
		{method: http.MethodGet, path: "/api/wallet/ledger"},
	} {
		if !isConfirmedExactRoute(route) {
			t.Fatalf("route %#v 未登记为精确本地路由", route)
		}
	}
}
