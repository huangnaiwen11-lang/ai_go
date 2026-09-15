package walletcontract

import "errors"

var (
	// ErrMigrationNotAllowed 表示静态合同被错误标记为可迁移，或未经证据许可打开了
	// Go 分流。该错误只用于本地审计，不得映射到支付或用户接口。
	ErrMigrationNotAllowed = errors.New("wallet contract: migration is not allowed")

	// ErrInvalidStaticContract 表示合同偏离已冻结的钱包、账本或支付边界。错误保持
	// 通用，避免账单、订单、签名或用户标识泄露到审计输出。
	ErrInvalidStaticContract = errors.New("wallet contract: static contract is invalid")
)

const (
	// ClassificationPending 表示资料只能证明静态源码结构。
	ClassificationPending = "待核实"
	// MigrationNotGranted 是静态合同唯一允许的迁移许可状态。
	MigrationNotGranted = "未授予"
	// SourceScopeStaticAudit 防止受控运行证据与静态资料被混用。
	SourceScopeStaticAudit = "仅静态源码审计"
)

// Route 描述一个公开 Wallet 接口及其 Node 兼容别名。它不保存请求参数、余额、订单、
// receipt、Authorization 或任何真实用户资料。
type Route struct {
	Method  string
	Path    string
	Aliases []string
}

// BalanceBaseline 保存余额接口的静态事实。MayCreateWallet 特别指出该 GET 请求不是
// 无副作用读取：当前 Node 在用户没有 Wallet 时会创建余额为零的钱包。
type BalanceBaseline struct {
	Route             Route
	MayCreateWallet   bool
	CacheControl      string
	VaryAuthorization bool
	CacheStatus       bool
	MayExposeStale    bool
}

// LedgerBaseline 保存账本读取的分页语义。CursorPrecedesSkip 为 true 表示提供 cursor
// 后不再叠加 skip，避免 Go 实现把两个分页机制错误组合。
type LedgerBaseline struct {
	Route              Route
	DefaultLimit       int
	MinimumLimit       int
	MaximumSkip        int
	CursorPrecedesSkip bool
}

// MutationBaseline 记录钱包余额变更必须维持的账务不变量。具体余额、金额、重试次数和
// 事务实现均不放在此静态合同中，避免用本地资料伪造运行时正确性。
type MutationBaseline struct {
	EntryPoint         string
	ProductionTxn      bool
	PreventsNegative   bool
	IdempotentReplay   bool
	PublishAfterCommit bool
}

// PaymentBoundary 记录支付渠道与 Webhook 的归属。主站仍需等待 PayCores 完成渠道侧
// 状态后再收敛权益，paid 且 backendReady 为 false 不能被当作最终到账。
type PaymentBoundary struct {
	Owner                      string
	WebhookOwner               string
	WaitForBackendReady        bool
	ProxyRoutes                []Route
	Callback                   Route
	CallbackRequiresV2HMAC     bool
	CallbackRejectsNonceReplay bool
}

// Manifest 是 Wallet/Billing 静态合同的最小可验证表示。GoRouteEnabled 只用于显式
// 拦截错误配置；合法的静态合同必须保持 false。
type Manifest struct {
	Classification      string
	MigrationPermission string
	SourceScope         string
	GoRouteEnabled      bool
	Balance             BalanceBaseline
	Ledger              LedgerBaseline
	Mutation            MutationBaseline
	Payment             PaymentBoundary
}

const (
	balancePath       = "/api/wallet/balance"
	balanceAlias      = "/api/v1/wallet/balance"
	ledgerPath        = "/api/wallet/ledger"
	ledgerAlias       = "/api/v1/wallet/ledger"
	balanceCacheValue = "private, no-store, max-age=0, must-revalidate"
	paymentOwner      = "PayCores"
)

// expectedPaymentProxyRoutes 是主站只可代理给 PayCores 的精确公开路径。清单不能
// 用前缀或通配符代替，避免把尚未审计的支付入口误归为可迁移范围。
func expectedPaymentProxyRoutes() []Route {
	return []Route{
		{Method: "GET", Path: "/api/wallet/external-payment-methods", Aliases: []string{"/api/v1/wallet/external-payment-methods"}},
		{Method: "POST", Path: "/api/wallet/create-external-checkout", Aliases: []string{"/api/v1/wallet/create-external-checkout"}},
		{Method: "POST", Path: "/api/wallet/external-checkout-billing-prefill", Aliases: []string{"/api/v1/wallet/external-checkout-billing-prefill"}},
		{Method: "POST", Path: "/api/wallet/crypto/create-payment", Aliases: []string{"/api/v1/wallet/crypto/create-payment"}},
		{Method: "POST", Path: "/api/wallet/crypto/refresh-payment", Aliases: []string{"/api/v1/wallet/crypto/refresh-payment"}},
		{Method: "GET", Path: "/api/wallet/crypto/status/:orderId", Aliases: []string{"/api/v1/wallet/crypto/status/:orderId"}},
	}
}

func expectedPaymentCallback() Route {
	return Route{
		Method:  "POST",
		Path:    "/api/internal/payment-confirmed",
		Aliases: []string{"/api/v1/internal/payment-confirmed"},
	}
}

// ValidateManifest 校验当前静态合同没有遗漏关键副作用、账务不变量或支付归属。通过只
// 说明本地材料与 Node 静态基线一致，不表示余额、账本、退款、支付或 Gateway 分流已获准。
func ValidateManifest(manifest Manifest) error {
	if manifest.MigrationPermission != MigrationNotGranted || manifest.GoRouteEnabled {
		return ErrMigrationNotAllowed
	}
	if manifest.Classification != ClassificationPending || manifest.SourceScope != SourceScopeStaticAudit {
		return ErrInvalidStaticContract
	}
	if !isRoute(manifest.Balance.Route, "GET", balancePath, balanceAlias) ||
		!manifest.Balance.MayCreateWallet ||
		manifest.Balance.CacheControl != balanceCacheValue ||
		!manifest.Balance.VaryAuthorization ||
		!manifest.Balance.CacheStatus ||
		!manifest.Balance.MayExposeStale {
		return ErrInvalidStaticContract
	}
	if !isRoute(manifest.Ledger.Route, "GET", ledgerPath, ledgerAlias) ||
		manifest.Ledger.DefaultLimit != 50 ||
		manifest.Ledger.MinimumLimit != 1 ||
		manifest.Ledger.MaximumSkip != 100000 ||
		!manifest.Ledger.CursorPrecedesSkip {
		return ErrInvalidStaticContract
	}
	if manifest.Mutation.EntryPoint != "applyMutation" ||
		!manifest.Mutation.ProductionTxn ||
		!manifest.Mutation.PreventsNegative ||
		!manifest.Mutation.IdempotentReplay ||
		!manifest.Mutation.PublishAfterCommit {
		return ErrInvalidStaticContract
	}
	if manifest.Payment.Owner != paymentOwner ||
		manifest.Payment.WebhookOwner != paymentOwner ||
		!manifest.Payment.WaitForBackendReady ||
		!sameRoutes(manifest.Payment.ProxyRoutes, expectedPaymentProxyRoutes()) ||
		!sameRoute(manifest.Payment.Callback, expectedPaymentCallback()) ||
		!manifest.Payment.CallbackRequiresV2HMAC ||
		!manifest.Payment.CallbackRejectsNonceReplay {
		return ErrInvalidStaticContract
	}

	return nil
}

func isRoute(route Route, method, path, alias string) bool {
	return route.Method == method && route.Path == path && len(route.Aliases) == 1 && route.Aliases[0] == alias
}

func sameRoute(actual, expected Route) bool {
	if actual.Method != expected.Method || actual.Path != expected.Path || len(actual.Aliases) != len(expected.Aliases) {
		return false
	}
	for index, alias := range expected.Aliases {
		if actual.Aliases[index] != alias {
			return false
		}
	}
	return true
}

func sameRoutes(actual, expected []Route) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index, expectedRoute := range expected {
		if !sameRoute(actual[index], expectedRoute) {
			return false
		}
	}
	return true
}
