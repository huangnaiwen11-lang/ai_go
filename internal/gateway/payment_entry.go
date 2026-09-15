package gateway

import "net/http"

const (
	localCheckoutPath       = "/api/wallet/create-external-checkout"
	localVerifyPurchasePath = "/api/wallet/verify-purchase"
)

// matchesPaymentEntry 只识别已完成本地语义审查的两个既有钱包 POST URL。
// 查询参数、编码路径和别名一律不接管，避免影响 Node 的其余支付调用。
func matchesPaymentEntry(request *http.Request) bool {
	return request != nil && request.Method == http.MethodPost && request.URL != nil &&
		!request.URL.ForceQuery && request.URL.RawQuery == "" && request.URL.EscapedPath() == request.URL.Path &&
		(request.URL.Path == localCheckoutPath || request.URL.Path == localVerifyPurchasePath)
}

func paymentEntryRouteKey(path string) exactRouteKey {
	return exactRouteKey{method: http.MethodPost, path: path}
}
