// Package adminmux 把已迁移的 /api/admin/ 只读投影组合成单一入口。
//
// 网关对 /api/admin/ 是 fail-closed：命中该前缀的请求全部由 Go 处理，
// 未实现的路径返回 501，不回退 Node。因此本包只做路径分派，
// 不改变任何一个投影自身的授权与语义。
package adminmux

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Options 是各管理投影的装配点。Fallback 必须是 adminview 主投影，
// 由它处理已迁移的通用管理接口，并对剩余路径返回 501。
type Options struct {
	Apps         http.Handler
	UTM          http.Handler
	Funnel       http.Handler
	Subscription http.Handler
	Blog         http.Handler
	Menu         http.Handler
	GA4          http.Handler
	Pricing      http.Handler
	PayCores     http.Handler
	ImageReview  http.Handler
	Images       http.Handler
	VideoReview  http.Handler
	Fallback     http.Handler
}

type handler struct {
	apps         http.Handler
	utm          http.Handler
	funnel       http.Handler
	subscription http.Handler
	blog         http.Handler
	menu         http.Handler
	ga4          http.Handler
	pricing      http.Handler
	payCores     http.Handler
	imageReview  http.Handler
	images       http.Handler
	videoReview  http.Handler
	fallback     http.Handler
}

// New 返回组合后的管理入口。
func New(options Options) http.Handler {
	return &handler{
		apps:         options.Apps,
		utm:          options.UTM,
		funnel:       options.Funnel,
		subscription: options.Subscription,
		blog:         options.Blog,
		menu:         options.Menu,
		ga4:          options.GA4,
		pricing:      options.Pricing,
		payCores:     options.PayCores,
		imageReview:  options.ImageReview,
		images:       options.Images,
		videoReview:  options.VideoReview,
		fallback:     options.Fallback,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.fallback == nil {
		failure(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Admin handler unavailable")
		return
	}
	path := r.URL.Path
	switch {
	case h.apps != nil && matches(path, "/api/admin/apps"):
		h.apps.ServeHTTP(w, r)
	case h.apps != nil && matches(path, "/api/admin/config/platforms"):
		h.apps.ServeHTTP(w, r)
	case h.utm != nil && matches(path, "/api/admin/utm-links"):
		h.utm.ServeHTTP(w, r)
	case h.funnel != nil && matches(path, "/api/admin/analytics/utm-funnel"):
		h.funnel.ServeHTTP(w, r)
	case h.blog != nil && matches(path, "/api/admin/blog"):
		h.blog.ServeHTTP(w, r)
	case h.subscription != nil && matches(path, "/api/admin/analytics/subscription"):
		h.subscription.ServeHTTP(w, r)
	case h.ga4 != nil && matches(path, "/api/admin/analytics/ga4"):
		h.ga4.ServeHTTP(w, r)
	case h.menu != nil && matches(path, "/api/admin/system/menu-visibility"):
		h.menu.ServeHTTP(w, r)
	case h.pricing != nil && (matches(path, "/api/admin/wallet/packages") || matches(path, "/api/admin/wallet/vip-config") || matches(path, "/api/admin/wallet/subscription-strategy") || matches(path, "/api/admin/config/system") || matches(path, "/api/admin/config/pricing")):
		h.pricing.ServeHTTP(w, r)
	case h.payCores != nil && matches(path, "/api/admin/paycores"):
		h.payCores.ServeHTTP(w, r)
	case h.imageReview != nil && matches(path, "/api/admin/image-review"):
		h.imageReview.ServeHTTP(w, r)
	case h.images != nil && matches(path, "/api/admin/images"):
		h.images.ServeHTTP(w, r)
	case h.videoReview != nil && (matches(path, "/api/admin/video-review") || matches(path, "/api/admin/homepage/review-template/video")):
		h.videoReview.ServeHTTP(w, r)
	default:
		h.fallback.ServeHTTP(w, r)
	}
}

// matches 只接受精确路径或以其为父目录的路径，避免 /api/admin/appsfoo 被误接管。
func matches(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func failure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}
