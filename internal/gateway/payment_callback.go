package gateway

import "net/http"

const (
	paymentCallbackPathV1     = "/api/v1/internal/payment-confirmed"
	paymentCallbackPathLegacy = "/api/internal/payment-confirmed"
)

// matchesPaymentCallback 只识别两条未经编码、规范化或附加查询的支付确认路径。
func matchesPaymentCallback(request *http.Request) bool {
	return request != nil && request.Method == http.MethodPost && request.URL != nil &&
		!request.URL.ForceQuery && request.URL.RawQuery == "" && request.URL.EscapedPath() == request.URL.Path &&
		(request.URL.Path == paymentCallbackPathV1 || request.URL.Path == paymentCallbackPathLegacy)
}

func paymentCallbackRouteKey(path string) exactRouteKey {
	return exactRouteKey{method: http.MethodPost, path: path}
}
