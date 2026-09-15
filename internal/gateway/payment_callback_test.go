package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 支付回调只能在路由开关精确放行时由 Go 接管；未放行或路径形态不合法时必须回退 Node。
func TestHandler支付回调需要精确路由开关(t *testing.T) {
	upstream := newCallbackTestUpstream(t)
	defer upstream.Close()

	for _, testCase := range []struct {
		name    string
		target  string
		enabled bool
		wantGo  bool
	}{
		{name: "V1 已放行", target: "/api/v1/internal/payment-confirmed", enabled: true, wantGo: true},
		{name: "legacy 已放行", target: "/api/internal/payment-confirmed", enabled: true, wantGo: true},
		{name: "路由未放行", target: "/api/v1/internal/payment-confirmed", wantGo: false},
		{name: "含查询", target: "/api/v1/internal/payment-confirmed?retry=1", enabled: true, wantGo: false},
		{name: "编码路径", target: "/api/v1/internal%2Fpayment-confirmed", enabled: true, wantGo: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			gateway := New(Config{
				DefaultUpstream: mustURL(t, upstream.URL),
				RouteSwitch: paymentCallbackRouteSwitch{enabled: testCase.enabled},
				PaymentCallback: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					called = true
					writer.WriteHeader(http.StatusNoContent)
				}),
			})
			recorder := httptest.NewRecorder()
			gateway.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, testCase.target, nil))
			if called != testCase.wantGo {
				t.Fatalf("Go 支付回调被调用 = %t，期望 %t", called, testCase.wantGo)
			}
			if testCase.wantGo && recorder.Code != http.StatusNoContent {
				t.Fatalf("Go 回调响应 = %d，期望 %d", recorder.Code, http.StatusNoContent)
			}
			if !testCase.wantGo && (recorder.Code != http.StatusAccepted || recorder.Body.String() != "node") {
				t.Fatalf("未放行请求未回退 Node：%d %q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

type paymentCallbackRouteSwitch struct{ enabled bool }

func (switcher paymentCallbackRouteSwitch) Enabled(route exactRouteKey) bool {
	return switcher.enabled && (route == exactRouteKey{method: http.MethodPost, path: paymentCallbackPathV1} || route == exactRouteKey{method: http.MethodPost, path: paymentCallbackPathLegacy})
}
