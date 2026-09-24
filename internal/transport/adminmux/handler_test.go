package adminmux

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func stub(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stub", name)
		w.WriteHeader(http.StatusOK)
	})
}

func TestAdminMux按路径分派到各自投影(t *testing.T) {
	gateway := New(Options{
		Apps:         stub("apps"),
		UTM:          stub("utm"),
		Funnel:       stub("funnel"),
		Subscription: stub("subscription"),
		Blog:         stub("blog"),
		GA4:          stub("ga4"),
		Pricing:      stub("pricing"),
		PayCores:     stub("paycores"),
		ImageReview:  stub("image-review"),
		Images:       stub("images"),
		VideoReview:  stub("video-review"),
		Fallback:     stub("fallback"),
	})
	cases := []struct{ path, want string }{
		{"/api/admin/apps", "apps"},
		{"/api/admin/apps/abc", "apps"},
		{"/api/admin/apps/overview", "apps"},
		{"/api/admin/utm-links", "utm"},
		{"/api/admin/utm-links/sources", "utm"},
		{"/api/admin/analytics/utm-funnel", "funnel"},
		{"/api/admin/analytics/utm-funnel/trend", "funnel"},
		{"/api/admin/blog", "blog"},
		{"/api/admin/blog/stats", "blog"},
		{"/api/admin/blog/abc/publish", "blog"},
		{"/api/admin/analytics/subscription/overview", "subscription"},
		{"/api/admin/analytics/subscription/subscribers", "subscription"},
		{"/api/admin/analytics/ga4/status", "ga4"},
		{"/api/admin/analytics/ga4/pwa/summary", "ga4"},
		{"/api/admin/wallet/packages", "pricing"},
		{"/api/admin/wallet/vip-config", "pricing"},
		{"/api/admin/wallet/subscription-strategy", "pricing"},
		{"/api/admin/config/system/wallet.coinPackagesOverrides", "pricing"},
		{"/api/admin/config/pricing", "pricing"},
		{"/api/admin/config/pricing/text-to-image", "pricing"},
		{"/api/admin/paycores/channels-overview", "paycores"},
		{"/api/admin/paycores/orders", "paycores"},
		{"/api/admin/image-review", "image-review"},
		{"/api/admin/image-review/pending", "image-review"},
		{"/api/admin/image-review/stats", "image-review"},
		{"/api/admin/images", "images"},
		{"/api/admin/images/template-options", "images"},
		{"/api/admin/images/provider-status", "images"},
		{"/api/admin/images/today-by-gpu", "images"},
		{"/api/admin/images/stats", "images"},
		{"/api/admin/images/batch-hide", "images"},
		{"/api/admin/images/507f1f77bcf86cd799439011/hide", "images"},
		{"/api/admin/video-review", "video-review"},
		{"/api/admin/video-review/pending", "video-review"},
		{"/api/admin/video-review/stats", "video-review"},
		{"/api/admin/video-review/gpu-status", "video-review"},
		{"/api/admin/homepage/review-template/video/video-1", "video-review"},
		// 以下都必须落到 adminview 主投影，不能被前缀误接管。
		{"/api/admin/users", "fallback"},
		{"/api/admin/appsfoo", "fallback"},
		{"/api/admin/blogroll", "fallback"},
		{"/api/admin/utm-links-x", "fallback"},
		{"/api/admin/analytics/utm-funnelx", "fallback"},
		{"/api/admin/config/pricingx", "fallback"},
		{"/api/admin/analytics/subscription", "subscription"},
		{"/api/admin/analytics/overview", "fallback"},
		{"/api/admin/analytics/subscriptions", "fallback"},
		{"/api/admin/analytics/ga4extra", "fallback"},
		{"/api/admin/paycores-extra", "fallback"},
		{"/api/admin/image-review-extra", "fallback"},
		{"/api/admin/images-extra", "fallback"},
		{"/api/admin/video-review-extra", "fallback"},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.path, nil))
		if got := recorder.Header().Get("X-Stub"); got != testCase.want {
			t.Errorf("path %q 分派到 %q，期望 %q", testCase.path, got, testCase.want)
		}
	}
}

func TestAdminMux缺少主投影时拒绝服务(t *testing.T) {
	gateway := New(Options{Apps: stub("apps")})
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/users", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", recorder.Code)
	}
	var body struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body.Success || body.Code != "SERVICE_UNAVAILABLE" {
		t.Fatalf("响应 = %#v，期望失败且带 SERVICE_UNAVAILABLE", body)
	}
}

func TestAdminMux未装配的投影回落到主投影(t *testing.T) {
	gateway := New(Options{Fallback: stub("fallback")})
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil))
	if got := recorder.Header().Get("X-Stub"); got != "fallback" {
		t.Fatalf("分派到 %q，期望 fallback", got)
	}
}
