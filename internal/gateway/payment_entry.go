package gateway

import (
	"net/http"
	"regexp"
)

const (
	localCheckoutPath       = "/api/wallet/create-external-checkout"
	localVerifyPurchasePath = "/api/wallet/verify-purchase"
	localProductsPath       = "/api/wallet/products"
	localOrderStatusRoute   = "/api/payments/order-status/:orderId"
)

var localOrderStatusPath = regexp.MustCompile(`^/api/payments/order-status/[A-Za-z0-9_-]{1,128}$`)

// matchesPaymentEntry 只识别审查过的支付读写 URL；订单标识限定为单个安全路径段。
// 查询参数、编码路径和别名一律不接管，避免影响 Node 的其余支付调用。
func matchesPaymentEntry(request *http.Request) bool {
	if request == nil || request.URL == nil || request.URL.ForceQuery || request.URL.RawQuery != "" || request.URL.EscapedPath() != request.URL.Path {
		return false
	}
	if request.Method == http.MethodGet {
		return request.URL.Path == localProductsPath || localOrderStatusPath.MatchString(request.URL.Path)
	}
	return request.Method == http.MethodPost && (request.URL.Path == localCheckoutPath || request.URL.Path == localVerifyPurchasePath)
}

func paymentEntryRouteKey(path string) exactRouteKey {
	if path == localProductsPath {
		return exactRouteKey{method: http.MethodGet, path: localProductsPath}
	}
	if localOrderStatusPath.MatchString(path) {
		return exactRouteKey{method: http.MethodGet, path: localOrderStatusRoute}
	}
	return exactRouteKey{method: http.MethodPost, path: path}
}
