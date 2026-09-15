package gateway

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Config 配置兼容网关使用的上游服务。
// 未列入 confirmedLocalRoutes 的请求都转发至 DefaultUpstream。
type Config struct {
	DefaultUpstream *url.URL
	RouteSwitch     RouteSwitch
	// GenerationCallback 仅处理两条精确的内部生成回调路径；未配置时仍由 Node 代理。
	GenerationCallback http.Handler
	// PaymentCallback 仅在独立开关和精确路由开关均放行后处理本地 PayCores 回调。
	PaymentCallback http.Handler
	// PaymentEntryHandler 仅在既有支付开关启用时处理已审查的支付读写入口。
	PaymentEntryHandler http.Handler
	// T2IHandler 只处理已通过精确路由和纯 T2I 候选分类的 Go 请求；nil 时全部代理 Node。
	T2IHandler http.Handler
	// VideoHandler 只处理已选模板的单图或文本首帧视频候选；nil 时全部代理 Node。
	VideoHandler http.Handler
	// AuthEntryHandler 仅处理四条精确的 Go 自有账号入口；nil 时全部继续代理 Node。
	AuthEntryHandler http.Handler
	// MediaHandler 仅处理两条用户自有图片素材路径；nil 时所有素材请求都继续代理 Node。
	MediaHandler http.Handler
	// WalletViewHandler 仅处理两个已审核的钱包只读 GET 路径；nil 时继续代理 Node。
	WalletViewHandler http.Handler
	// WorksHandler 仅处理本人作品列表与单件详情；nil 时继续代理 Node。
	WorksHandler        http.Handler
	FeedbackHandler     http.Handler
	NotificationHandler http.Handler
	// GenerationStreamHandler 仅处理本地生成状态 SSE；未配置时继续代理 Node。
	GenerationStreamHandler http.Handler
	// AdmissionTimeout 仅约束本地金丝雀向 Node 发起的准入请求。零值表示不额外
	// 缩短调用方上下文，保留现有本地行为；真实切流前必须按受控基线显式配置。
	AdmissionTimeout time.Duration
}

// Gateway 是透明的 /api 兼容代理，不访问业务数据库，也不修改响应信封。
type Gateway struct {
	defaultProxy            *httputil.ReverseProxy
	defaultUpstream         *url.URL
	routeSwitch             RouteSwitch
	admissionClient         *http.Client
	generationCallback      http.Handler
	paymentCallback         http.Handler
	paymentEntryHandler     http.Handler
	t2iHandler              http.Handler
	videoHandler            http.Handler
	authEntryHandler        http.Handler
	mediaHandler            http.Handler
	walletViewHandler       http.Handler
	worksHandler            http.Handler
	feedbackHandler         http.Handler
	notificationHandler     http.Handler
	generationStreamHandler http.Handler
}

type exactLocalRoute struct {
	method  string
	path    string
	handler http.HandlerFunc
}

// confirmedLocalRoutes 根据阶段 0 迁移矩阵编译进网关。这里仅允许已确认的精确
// method/path 组合，不能使用前缀、/api/v1 别名或编码后的路径变体。
var confirmedLocalRoutes = [...]exactLocalRoute{
	{
		method:  http.MethodGet,
		path:    growthPingPath,
		handler: serveGrowthPingCanary,
	},
}

// authEntryRoutes 是已经完成账号语义审查的精确方法与路径组合。不能以路径前缀、
// /api/v1 别名或 URL 编码的路径替代，避免 OAuth 等 Node 专属认证流程误入 Go。
var authEntryRoutes = [...]exactRouteKey{
	{method: http.MethodPost, path: "/api/auth/register"},
	{method: http.MethodPost, path: "/api/auth/login"},
	{method: http.MethodPost, path: "/api/auth/guest"},
	{method: http.MethodPost, path: "/api/auth/bind"},
	{method: http.MethodGet, path: "/api/auth/me"},
	{method: http.MethodPatch, path: "/api/auth/me/profile"},
	{method: http.MethodDelete, path: "/api/auth/me/sessions"},
	{method: http.MethodPost, path: "/api/auth/me/password"},
	{method: http.MethodDelete, path: "/api/auth/me"},
}

var feedbackRoutes = [...]exactRouteKey{{method: http.MethodPost, path: "/api/feedback"}}

var notificationRoutes = [...]exactRouteKey{
	{method: http.MethodGet, path: "/api/notifications"},
	{method: http.MethodDelete, path: "/api/notifications"},
	{method: http.MethodGet, path: "/api/notifications/unread-count"},
	{method: http.MethodGet, path: "/api/notifications/preferences"},
	{method: http.MethodPatch, path: "/api/notifications/preferences"},
	{method: http.MethodPost, path: "/api/notifications/read-all"},
	{method: http.MethodPost, path: "/api/notifications/:id/read"},
	{method: http.MethodDelete, path: "/api/notifications/:id"},
}

var generationStreamRoute = exactRouteKey{method: http.MethodGet, path: "/api/users/me/generations/stream"}

func isConfirmedExactRoute(route exactRouteKey) bool {
	for _, localRoute := range []exactRouteKey{
		{method: http.MethodPost, path: paymentCallbackPathV1},
		{method: http.MethodPost, path: paymentCallbackPathLegacy},
		{method: http.MethodPost, path: localCheckoutPath},
		{method: http.MethodPost, path: localVerifyPurchasePath},
		{method: http.MethodGet, path: localProductsPath},
		{method: http.MethodGet, path: localOrderStatusRoute},
	} {
		if route == localRoute {
			return true
		}
	}
	if route == generationStreamRoute {
		return true
	}
	for _, localRoute := range []t2iRoute{t2iRouteCreate, t2iRouteStatuses, t2iRouteDetail} {
		if route == localRoute.routeKey() {
			return true
		}
	}
	for _, localRoute := range []mediaRoute{mediaRouteUpload, mediaRouteRead} {
		if route == localRoute.routeKey() {
			return true
		}
	}
	for _, localRoute := range []videoRoute{videoRouteCreate, videoRouteStatuses, videoRouteDetail} {
		if route == localRoute.routeKey() {
			return true
		}
	}
	for _, authRoute := range authEntryRoutes {
		if route == authRoute {
			return true
		}
	}
	for _, walletRoute := range walletViewRoutes {
		if route == walletRoute {
			return true
		}
	}
	for _, worksRoute := range worksRoutes {
		if route == worksRoute {
			return true
		}
	}
	for _, feedbackRoute := range feedbackRoutes {
		if route == feedbackRoute {
			return true
		}
	}
	for _, notificationRoute := range notificationRoutes {
		if route == notificationRoute {
			return true
		}
	}
	for _, confirmedRoute := range confirmedLocalRoutes {
		if route.method == confirmedRoute.method && route.path == confirmedRoute.path {
			return true
		}
	}
	return false
}

// New 构造网关。缺少默认上游属于启动配置错误，应在进程启动时立即暴露。
func New(cfg Config) *Gateway {
	if cfg.DefaultUpstream == nil {
		panic("gateway: default upstream is required")
	}
	routeSwitch := cfg.RouteSwitch
	if routeSwitch == nil {
		routeSwitch = disabledRouteSwitch{}
	}
	return &Gateway{
		defaultProxy:            newProxy(cfg.DefaultUpstream),
		defaultUpstream:         cloneURL(cfg.DefaultUpstream),
		routeSwitch:             routeSwitch,
		admissionClient:         newAdmissionClient(cfg.AdmissionTimeout),
		generationCallback:      cfg.GenerationCallback,
		paymentCallback:         cfg.PaymentCallback,
		paymentEntryHandler:     cfg.PaymentEntryHandler,
		t2iHandler:              cfg.T2IHandler,
		videoHandler:            cfg.VideoHandler,
		authEntryHandler:        cfg.AuthEntryHandler,
		mediaHandler:            cfg.MediaHandler,
		walletViewHandler:       cfg.WalletViewHandler,
		worksHandler:            cfg.WorksHandler,
		feedbackHandler:         cfg.FeedbackHandler,
		notificationHandler:     cfg.NotificationHandler,
		generationStreamHandler: cfg.GenerationStreamHandler,
	}
}

// Handler 只服务已确认的精确本地路由，其余请求全部代理至 Node。
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = newRequestID()
			r.Header.Set("X-Request-Id", requestID)
		}
		if g.generationCallback != nil && matchesGenerationCallback(r) {
			g.generationCallback.ServeHTTP(w, r)
			return
		}
		if g.generationStreamHandler != nil && r.Method == http.MethodGet && r.URL.Path == generationStreamRoute.path && g.routeSwitch.Enabled(generationStreamRoute) {
			g.generationStreamHandler.ServeHTTP(w, r)
			return
		}
		if g.paymentCallback != nil && matchesPaymentCallback(r) && g.routeSwitch.Enabled(paymentCallbackRouteKey(r.URL.Path)) {
			g.paymentCallback.ServeHTTP(w, r)
			return
		}
		if g.paymentEntryHandler != nil && matchesPaymentEntry(r) && g.routeSwitch.Enabled(paymentEntryRouteKey(r.URL.Path)) {
			// 支付入口一旦接管不得再回退 Node，否则一次客户端购买可能冻结两张订单或重复入账。
			g.paymentEntryHandler.ServeHTTP(w, r)
			return
		}
		// 本机 I2I 目录与图片 Handler 同期开关。目录不包含技术配方，且只有完整 fixture 的
		// Vite 精确代理会将请求定向到这里；其余环境保持 Node 目录语义。
		if g.t2iHandler != nil && matchesLocalImageTemplateCatalog(r) {
			g.t2iHandler.ServeHTTP(w, r)
			return
		}
		if g.videoHandler != nil && matchesLocalVideoTemplateCatalog(r) {
			g.videoHandler.ServeHTTP(w, r)
			return
		}
		if route, ok := matchWorksRoute(r); ok {
			if g.worksHandler != nil && g.routeSwitch.Enabled(route) {
				// 作品读取一旦由 Go 接管，必须使用同一 Go 会话和自有创作事实；
				// 开关关闭时继续由 Node 透明处理，避免混用两个作品域。
				g.worksHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		if route, ok := matchWalletViewRoute(r); ok {
			if g.walletViewHandler != nil && g.routeSwitch.Enabled(route) {
				// 钱包读取一旦由 Go 接管，必须使用 Go 自有会话和账本事实，不能回退 Node
				// 混用两个账户域；开关未开启时则完整保持 Node 的既有读取语义。
				g.walletViewHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		if route, ok := matchAuthEntryRoute(r); ok {
			if g.authEntryHandler != nil && g.routeSwitch.Enabled(route) {
				// 账号入口一旦被明确接管，响应必须来自同一 Go 用户域，绝不能在失败时
				// 重放 Node，否则会出现两个账户、两个会话或重复注册奖励的风险。
				g.authEntryHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		if route, ok := matchFeedbackRoute(r); ok {
			if g.feedbackHandler != nil && g.routeSwitch.Enabled(route) {
				g.feedbackHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		if route, ok := matchNotificationRoute(r); ok {
			if g.notificationHandler != nil && g.routeSwitch.Enabled(route) {
				g.notificationHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		if route := matchLocalMediaRoute(r); route != mediaRouteNone {
			if g.mediaHandler != nil && g.routeSwitch.Enabled(route.routeKey()) {
				// 上传和读取一旦接管均不得转交 Node，素材 ID、归属校验和存储均属于 Go 自有域。
				g.mediaHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		if route := matchT2ILocalRoute(r); route != t2iRouteNone {
			if g.t2iHandler != nil && g.routeSwitch.Enabled(route.routeKey()) && isT2ILocalCandidate(route, r) {
				// 本地处理器一旦取得控制权，不能再重放给 Node，避免创建请求双预扣。
				g.t2iHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		if route := matchVideoLocalRoute(r); route != videoRouteNone {
			if g.videoHandler != nil && g.routeSwitch.Enabled(route.routeKey()) && isVideoLocalCandidate(route, r) {
				// 接管后绝不回退 Node，防止创建预扣在两个服务各执行一次。
				g.videoHandler.ServeHTTP(w, r)
				return
			}
			g.defaultProxy.ServeHTTP(w, r)
			return
		}
		for _, route := range confirmedLocalRoutes {
			routeKey := exactRouteKey{method: route.method, path: route.path}
			if r.Method != routeKey.method || r.URL.Path != routeKey.path || r.URL.EscapedPath() != routeKey.path {
				continue
			}
			if !g.routeSwitch.Enabled(routeKey) {
				break
			}
			g.serveAfterNodeAdmission(w, r, route, requestID)
			return
		}

		g.defaultProxy.ServeHTTP(w, r)
	})
}

func matchAuthEntryRoute(request *http.Request) (exactRouteKey, bool) {
	if request == nil || request.URL == nil || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path {
		return exactRouteKey{}, false
	}
	route := exactRouteKey{method: request.Method, path: request.URL.Path}
	for _, expected := range authEntryRoutes {
		if route == expected {
			return route, true
		}
	}
	return exactRouteKey{}, false
}

func matchFeedbackRoute(request *http.Request) (exactRouteKey, bool) {
	if request == nil || request.URL == nil || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path {
		return exactRouteKey{}, false
	}
	route := exactRouteKey{method: request.Method, path: request.URL.Path}
	for _, expected := range feedbackRoutes {
		if route == expected {
			return route, true
		}
	}
	return exactRouteKey{}, false
}

func matchNotificationRoute(request *http.Request) (exactRouteKey, bool) {
	if request == nil || request.URL == nil || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path {
		return exactRouteKey{}, false
	}
	route := exactRouteKey{method: request.Method, path: request.URL.Path}
	for _, expected := range notificationRoutes {
		if route == expected {
			// 列表允许 limit/unreadOnly；未读数和全部已读不接受额外参数。
			if route.path != "/api/notifications" && request.URL.RawQuery != "" {
				return exactRouteKey{}, false
			}
			return route, true
		}
	}
	if request.URL.RawQuery != "" {
		return exactRouteKey{}, false
	}
	if request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/notifications/") && strings.HasSuffix(request.URL.Path, "/read") {
		id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/notifications/"), "/read")
		if id != "" && !strings.Contains(id, "/") {
			return exactRouteKey{method: http.MethodPost, path: "/api/notifications/:id/read"}, true
		}
	}
	if request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/notifications/") {
		id := strings.TrimPrefix(request.URL.Path, "/api/notifications/")
		if id != "" && !strings.Contains(id, "/") {
			return exactRouteKey{method: http.MethodDelete, path: "/api/notifications/:id"}, true
		}
	}
	return exactRouteKey{}, false
}

func matchesLocalImageTemplateCatalog(request *http.Request) bool {
	return request != nil && request.URL != nil && request.Method == http.MethodGet &&
		request.URL.Path == "/api/homepage/image-templates" && request.URL.RawQuery == "" &&
		!request.URL.ForceQuery && request.URL.EscapedPath() == request.URL.Path
}

func matchesLocalVideoTemplateCatalog(request *http.Request) bool {
	return request != nil && request.URL != nil && request.Method == http.MethodGet &&
		request.URL.Path == "/api/homepage/video-templates" && request.URL.RawQuery == "" &&
		!request.URL.ForceQuery && request.URL.EscapedPath() == request.URL.Path
}

func cloneURL(source *url.URL) *url.URL {
	cloned := *source
	return &cloned
}
