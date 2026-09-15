package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"ai-business-service/internal/gateway"
)

const (
	defaultGatewayAddr       = ":18000"
	defaultGatewayConfigPath = "./configs/config.yaml"
)

func main() {
	listenAddr := gatewayListenAddr()
	upstreamRaw := getenv("BACKEND_UPSTREAM", "http://127.0.0.1:4000")
	upstream, err := parseBackendUpstream(upstreamRaw)
	if err != nil {
		slog.Error("invalid BACKEND_UPSTREAM", "error", err)
		os.Exit(2)
	}
	if err := rejectDeprecatedPrefixUpstreams(os.Getenv("GATEWAY_PREFIX_UPSTREAMS")); err != nil {
		slog.Error("unsupported GATEWAY_PREFIX_UPSTREAMS", "error", err)
		os.Exit(2)
	}
	admissionTimeout, err := parseOptionalAdmissionTimeout(os.Getenv("GATEWAY_ADMISSION_TIMEOUT"))
	if err != nil {
		slog.Error("invalid GATEWAY_ADMISSION_TIMEOUT", "error", err)
		os.Exit(2)
	}
	generationCallback, cleanupCallback, err := newOptionalGenerationCallback(
		generationCallbackEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
	)
	if err != nil {
		slog.Error("initialize local generation callback handler", "error", err)
		os.Exit(2)
	}
	paymentCallback, cleanupPaymentCallback, err := newOptionalPaymentCallback(
		localPaymentCallbackEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
	)
	if err != nil {
		combineCleanups(cleanupCallback)()
		slog.Error("initialize local payment callback handler", "error", err)
		os.Exit(2)
	}
	sessionAuthenticator, cleanupSessionAuth, err := newOptionalSessionAuthenticator(
		goSessionAuthEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback)()
		slog.Error("initialize local Go session authenticator", "error", err)
		os.Exit(2)
	}
	generationStreamHandler, cleanupGenerationStream := newOptionalGenerationStreamHandler(
		goSessionAuthEnabled() && localGenerationStreamEnabled(), sessionAuthenticator,
	)
	paymentEntryEnabled := goSessionAuthEnabled() && localPaymentEntryEnabled() && localIAPTestVerifierEnabled()
	var paymentEntry http.Handler
	var cleanupPaymentEntry func()
	if localPayCoresMockEnabled() {
		paymentEntry, cleanupPaymentEntry, err = newOptionalPayCoresPaymentEntryHandler(paymentEntryEnabled, getenv("GATEWAY_CONFIG", defaultGatewayConfigPath), sessionAuthenticator)
	} else {
		paymentEntry, cleanupPaymentEntry, err = newOptionalPaymentEntryHandler(paymentEntryEnabled, getenv("GATEWAY_CONFIG", defaultGatewayConfigPath), sessionAuthenticator)
	}
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth)()
		slog.Error("initialize local payment entry handler", "error", err)
		os.Exit(2)
	}
	publicT2IHandler, cleanupPublicT2I, err := newOptionalPublicT2IHandler(
		goSessionAuthEnabled() && publicT2IEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry)()
		slog.Error("initialize local public T2I handler", "error", err)
		os.Exit(2)
	}
	localMediaHandler, cleanupLocalMedia, err := newOptionalLocalMediaHandler(
		goSessionAuthEnabled() && localMediaEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry, cleanupPublicT2I)()
		slog.Error("initialize local media handler", "error", err)
		os.Exit(2)
	}
	publicVideoHandler, cleanupPublicVideo, err := newOptionalPublicVideoHandler(
		publicVideoLocalEnabled(goSessionAuthEnabled(), publicVideoEnabled(), generationCallback),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry, cleanupPublicT2I, cleanupLocalMedia)()
		slog.Error("initialize local public video handler", "error", err)
		os.Exit(2)
	}
	authEntryHandler, cleanupAuthEntry, err := newOptionalAuthEntryHandler(
		goSessionAuthEnabled() && localAuthEntryEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry, cleanupPublicT2I, cleanupLocalMedia, cleanupPublicVideo)()
		slog.Error("initialize local auth entry handler", "error", err)
		os.Exit(2)
	}
	walletViewHandler, cleanupWalletView, err := newOptionalWalletViewHandler(
		goSessionAuthEnabled() && localWalletViewEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry, cleanupPublicT2I, cleanupLocalMedia, cleanupPublicVideo, cleanupAuthEntry)()
		slog.Error("initialize local wallet view handler", "error", err)
		os.Exit(2)
	}
	worksViewHandler, cleanupWorksView, err := newOptionalWorksViewHandler(
		goSessionAuthEnabled() && localWorksViewEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry, cleanupPublicT2I, cleanupLocalMedia, cleanupPublicVideo, cleanupAuthEntry, cleanupWalletView)()
		slog.Error("initialize local works view handler", "error", err)
		os.Exit(2)
	}
	feedbackHandler, cleanupFeedback, err := newOptionalFeedbackHandler(
		goSessionAuthEnabled() && localFeedbackEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry, cleanupPublicT2I, cleanupLocalMedia, cleanupPublicVideo, cleanupAuthEntry, cleanupWalletView, cleanupWorksView)()
		slog.Error("initialize local feedback handler", "error", err)
		os.Exit(2)
	}
	notificationHandler, cleanupNotification, err := newOptionalNotificationHandler(
		goSessionAuthEnabled() && localNotificationsEnabled(),
		getenv("GATEWAY_CONFIG", defaultGatewayConfigPath),
		sessionAuthenticator,
	)
	if err != nil {
		combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupPaymentEntry, cleanupPublicT2I, cleanupLocalMedia, cleanupPublicVideo, cleanupAuthEntry, cleanupWalletView, cleanupWorksView, cleanupFeedback)()
		slog.Error("initialize local notification handler", "error", err)
		os.Exit(2)
	}
	cleanupDependencies := combineCleanups(cleanupCallback, cleanupPaymentCallback, cleanupSessionAuth, cleanupGenerationStream, cleanupPaymentEntry, cleanupPublicT2I, cleanupLocalMedia, cleanupPublicVideo, cleanupAuthEntry, cleanupWalletView, cleanupWorksView, cleanupFeedback, cleanupNotification)
	g := newGatewayWithPaymentEntry(
		upstream,
		gateway.NewFileRouteSwitch(os.Getenv("GATEWAY_EXACT_ROUTE_SWITCH_FILE")),
		admissionTimeout,
		generationCallback,
		publicT2IHandler,
		publicVideoHandler,
		paymentCallback,
		paymentEntry,
		authEntryHandler,
		localMediaHandler,
		walletViewHandler,
		worksViewHandler,
		feedbackHandler,
		notificationHandler,
		generationStreamHandler,
	)
	slog.Info("api gateway listening", "addr", listenAddr, "upstream", upstream.Redacted())
	if err := runGateway(listenAddr, g, cleanupDependencies, http.ListenAndServe); err != nil {
		slog.Error("api gateway stopped", "error", err)
		os.Exit(1)
	}
}

// runGateway 将监听生命周期和本地回调依赖的清理绑定在一起。即使监听立即失败，
// 也会在错误返回 main 之前释放 MongoDB 连接；main 随后再决定退出码。
func runGateway(listenAddr string, instance *gateway.Gateway, cleanup func(), listen func(string, http.Handler) error) error {
	if cleanup != nil {
		defer cleanup()
	}
	return listen(listenAddr, instance.Handler())
}

// newGateway 只负责把已受控构造的本地回调处理器传入 Gateway；监听与默认 Node
// 代理仍由 main 保持原有流程管理。
func newGateway(upstream *url.URL, routeSwitch gateway.RouteSwitch, admissionTimeout time.Duration, generationCallback, publicT2IHandler, publicVideoHandler http.Handler, handlers ...http.Handler) *gateway.Gateway {
	var paymentCallback http.Handler
	var authEntryHandler http.Handler
	if len(handlers) == 1 {
		authEntryHandler = handlers[0]
	}
	if len(handlers) == 2 {
		paymentCallback = handlers[0]
		authEntryHandler = handlers[1]
	}
	return gateway.New(gateway.Config{
		DefaultUpstream:    upstream,
		RouteSwitch:        routeSwitch,
		AdmissionTimeout:   admissionTimeout,
		GenerationCallback: generationCallback,
		PaymentCallback:    paymentCallback,
		T2IHandler:         publicT2IHandler,
		VideoHandler:       publicVideoHandler,
		AuthEntryHandler:   authEntryHandler,
	})
}

// newGatewayWithPaymentEntry 为 main 提供带本地支付入口的显式装配，保留旧测试辅助函数
// 的可读参数形态，避免可选 Handler 的位置被不透明的可变参数误解。
func newGatewayWithPaymentEntry(upstream *url.URL, routeSwitch gateway.RouteSwitch, admissionTimeout time.Duration, generationCallback, publicT2IHandler, publicVideoHandler, paymentCallback, paymentEntry, authEntryHandler http.Handler, localHandlers ...http.Handler) *gateway.Gateway {
	var localMediaHandler http.Handler
	var walletViewHandler http.Handler
	var worksViewHandler http.Handler
	if len(localHandlers) > 0 {
		localMediaHandler = localHandlers[0]
	}
	if len(localHandlers) > 1 {
		walletViewHandler = localHandlers[1]
	}
	if len(localHandlers) > 2 {
		worksViewHandler = localHandlers[2]
	}
	var feedbackHandler http.Handler
	if len(localHandlers) > 3 {
		feedbackHandler = localHandlers[3]
	}
	var notificationHandler http.Handler
	if len(localHandlers) > 4 {
		notificationHandler = localHandlers[4]
	}
	var generationStreamHandler http.Handler
	if len(localHandlers) > 5 {
		generationStreamHandler = localHandlers[5]
	}
	return gateway.New(gateway.Config{
		DefaultUpstream:     upstream,
		RouteSwitch:         routeSwitch,
		AdmissionTimeout:    admissionTimeout,
		GenerationCallback:  generationCallback,
		PaymentCallback:     paymentCallback,
		PaymentEntryHandler: paymentEntry,
		T2IHandler:          publicT2IHandler,
		VideoHandler:        publicVideoHandler,
		AuthEntryHandler:    authEntryHandler,
		MediaHandler:        localMediaHandler,
		WalletViewHandler:   walletViewHandler,
		WorksHandler:        worksViewHandler,
		FeedbackHandler:     feedbackHandler,
		NotificationHandler: notificationHandler,
		GenerationStreamHandler: generationStreamHandler,
	})
}

// gatewayListenAddr 默认使用本地回调 Origin 对应端口；显式环境变量仍拥有最高优先级。
func gatewayListenAddr() string {
	return getenv("GATEWAY_ADDR", defaultGatewayAddr)
}

// generationCallbackEnabled 只允许显式 true 接管两条内部回调路径，默认维持
// Gateway 对 Node 的透明代理，不能借用 routeSwitch 控制该安全边界。
func generationCallbackEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_GENERATION_CALLBACK_ENABLED")), "true")
}

// localPaymentCallbackEnabled 是本地 Go 支付回调的独立门禁；默认不得接管 Node 收款路径。
func localPaymentCallbackEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_PAYMENT_CALLBACK_ENABLED")), "true")
}

// localPaymentEntryEnabled 是本地钱包入口的独立开关，单独为 true 仍不足以接管 Node。
func localPaymentEntryEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_PAYMENT_ENTRY_ENABLED")), "true")
}

// localIAPTestVerifierEnabled 明确承认当前仅允许受控本地回执，杜绝误接真实商店凭据。
func localIAPTestVerifierEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_IAP_TEST_VERIFIER_ENABLED")), "true")
}

// localPayCoresMockEnabled 是本地 mock 收银台的第四道门禁；未显式开启时保持 local_only，
// 因而不会因为支付入口接管就请求任何 PayCores 地址。
func localPayCoresMockEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_PAYCORES_MOCK_ENABLED")), "true")
}

// localFeedbackEnabled 仅允许本地显式接管反馈写入，默认保持 Node 透明代理。
func localFeedbackEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_FEEDBACK_ENABLED")), "true")
}

// localNotificationsEnabled 仅控制通知读写是否由 Go 本地接管，默认保持 Node 兼容代理。
func localNotificationsEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_NOTIFICATIONS_ENABLED")), "true")
}

// localGenerationStreamEnabled 是本地内存 SSE 的独立开关，不代表 Redis 兼容或生产放行。
func localGenerationStreamEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_GENERATION_STREAM_ENABLED")), "true")
}

// goSessionAuthEnabled 仅允许显式 true 装配 Go 自有会话依赖；它不是公开路由开关。
func goSessionAuthEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_GO_SESSION_AUTH_ENABLED")), "true")
}

// publicT2IEnabled 是公开纯文生图的第二道显式开关；仅会话开关开启不足以接管 Node 路由。
func publicT2IEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_PUBLIC_T2I_ENABLED")), "true")
}

// localMediaEnabled 是素材域独立的第二道显式开关。它不能被 T2I 开关隐式打开，
// 这样未完成素材迁移的环境不会意外接管 Node 上传路径。
func localMediaEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_MEDIA_ENABLED")), "true")
}

// publicVideoEnabled 是公开模板视频的独立开关；必须与 Go 会话开关同时开启才会装配本地依赖。
func publicVideoEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_PUBLIC_VIDEO_ENABLED")), "true")
}

// localAuthEntryEnabled 是账号入口独立的第二道显式开关；仅启用 Go 会话认证不足以
// 接管 Node 的注册和登录路径。
func localAuthEntryEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_AUTH_ENTRY_ENABLED")), "true")
}

// localWalletViewEnabled 是钱包读取的独立第二道门禁；它只接管两个 GET 路径，
// 不能由支付、创作或会话开关隐式打开。
func localWalletViewEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_WALLET_VIEW_ENABLED")), "true")
}

// localWorksViewEnabled 是作品历史读取的独立第二道门禁；只有与 Go 会话及 route-switch
// 同时启用，Gateway 才能接管两条已审查的作品 GET 路径。
func localWorksViewEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GATEWAY_LOCAL_WORKS_VIEW_ENABLED")), "true")
}

// publicVideoLocalEnabled 把创建预扣与可信回调视为同一条原子链路。
// 回调未装配时绝不能接管视频创建，否则 Go 账本和创作无法由本地回调收敛。
func publicVideoLocalEnabled(sessionEnabled, videoEnabled bool, callback http.Handler) bool {
	return sessionEnabled && videoEnabled && callback != nil
}

// combineCleanups 将多个可选本地依赖的释放函数收敛为一次调用，并按逆序释放。
func combineCleanups(cleanups ...func()) func() {
	return func() {
		for index := len(cleanups) - 1; index >= 0; index-- {
			if cleanups[index] != nil {
				cleanups[index]()
			}
		}
	}
}

// parseBackendUpstream 将所有上游输入错误收敛为固定消息，避免日志回显 userinfo
// 或其他未经验证的 URL 内容。
func parseBackendUpstream(raw string) (*url.URL, error) {
	upstream, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return nil, errors.New("invalid backend upstream")
	}
	return upstream, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// parseOptionalAdmissionTimeout 只接受正数 Go duration。未设置表示保持既有的本地
// 行为；真实灰度前必须由受控运行基线明确给出数值，不能凭此默认生产配置。
func parseOptionalAdmissionTimeout(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}

	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, errors.New("admission timeout must be a positive Go duration")
	}
	return timeout, nil
}

// rejectDeprecatedPrefixUpstreams prevents unaudited path-prefix cutovers.
// Exact local routes must come from the audited migration matrix instead.
func rejectDeprecatedPrefixUpstreams(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return errors.New("GATEWAY_PREFIX_UPSTREAMS is unsupported; use an audited exact local route")
}
