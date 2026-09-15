package gateway

import "net/http"

const (
	walletSummaryPath = "/api/wallet/summary"
	walletLedgerPath  = "/api/wallet/ledger"
)

// walletViewRoutes 是钱包读取域已经审查过的精确路由集合。
// 仅允许这两个 GET URL，不能按 /api/wallet 前缀接管旧 Node 的支付或其他钱包业务。
var walletViewRoutes = [...]exactRouteKey{
	{method: http.MethodGet, path: walletSummaryPath},
	{method: http.MethodGet, path: walletLedgerPath},
}

// matchWalletViewRoute 仅按精确 method/path 判断路由边界。
// 账本分页参数由本地 Handler 严格校验；这里不能拒绝 RawQuery，否则合法 cursor/skip
// 会错误回退到 Node。编码路径和方法变体仍一律不接管。
func matchWalletViewRoute(request *http.Request) (exactRouteKey, bool) {
	if request == nil || request.URL == nil || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path {
		return exactRouteKey{}, false
	}
	route := exactRouteKey{method: request.Method, path: request.URL.Path}
	for _, expected := range walletViewRoutes {
		if route == expected {
			return route, true
		}
	}
	return exactRouteKey{}, false
}
