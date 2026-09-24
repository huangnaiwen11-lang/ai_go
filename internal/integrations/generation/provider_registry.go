package generation

import (
	"context"
	"errors"
	"strings"
	"time"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/integrations/polarstarb2b"
	"github.com/google/wire"
	"google.golang.org/protobuf/types/known/durationpb"
)

// execution.v2 客户端必须持续满足注册表暴露的最小出站边界。
var _ LocalSubmitter = (*Client)(nil)

var (
	// ErrProviderRegistryConfig indicates that the composition root cannot
	// construct a safe provider from the already validated configuration.
	ErrProviderRegistryConfig = errors.New("generation provider registry configuration unavailable")
	// ErrProviderNotEnabled is deliberately distinct from a provider protocol
	// error.  Callers must not silently fall back to local execution on this
	// error, especially in production.
	ErrProviderNotEnabled = errors.New("generation provider is not enabled")
	// ErrProviderRouteMismatch prevents a historical step from being sent via
	// the currently selected new-task provider or a different B2B account.
	ErrProviderRouteMismatch = errors.New("generation provider route does not match configured account")
)

// LocalSubmitter 是 execution.v2 的最小出站边界，也是注册表句柄暴露本地
// 能力的唯一形状。它只接受 execution.v2 技术请求，不含用户、权益、账本或
// 支付字段；调用方依赖该接口而不是具体客户端类型，因此注册表不绑定实现，
// 也不会因客户端构造方式变化而改动下游。
type LocalSubmitter interface {
	Submit(context.Context, Execution) (SubmissionResult, error)
	Lookup(context.Context, string) (LookupResult, error)
}

// ProviderHandle is a capability-scoped provider reference returned by the
// registry.  A handle carries exactly one provider capability; callers must not
// infer a provider from the active selector for a persisted historical step.
// The B2B policy values are copied from configuration for workers that need a
// bounded semaphore or result URL validation. Secrets are intentionally absent.
type ProviderHandle struct {
	ID                  string
	Local               LocalSubmitter
	B2B                 *polarstarb2b.Client
	AccountRef          string
	TenantID            string
	DeliveryMode        string
	MaxInFlight         int32
	MaxResultBytes      int64
	ResultHostAllowlist []string
}

func (h ProviderHandle) IsLocal() bool {
	return h.ID == conf.ProviderLocalExecutionV2 && h.Local != nil && h.B2B == nil
}
func (h ProviderHandle) IsB2B() bool {
	return h.ID == conf.ProviderPolarStarB2BV2 && h.B2B != nil && h.Local == nil
}

// ProviderRegistry keeps independent local and B2B clients. NewTaskProvider
// only follows the selector frozen in current configuration. ForRoute instead
// resolves a persisted provider identity, so switching the selector cannot
// redirect an already accepted task to another protocol.
type ProviderRegistry struct {
	newProvider string
	local       *Client
	b2b         *polarstarb2b.Client
	b2bAccount  string
	b2bTenant   string
	b2bMode     string
	maxInFlight int32
	maxResult   int64
	allowlist   []string
	b2bConfig   B2BSubmissionConfig
}

// RegistryProviderSet is kept separate from the legacy execution.v2
// ProviderSet. The composition root can opt into the registry without making
// the existing local worker accidentally receive a B2B client.
var RegistryProviderSet = wire.NewSet(NewProviderRegistry)

// NewProviderRegistry is the generation composition root. An absent B2B
// block is the explicit default local profile: no B2B client is constructed.
// Supplying the B2B block is the authorization/feature gate for B2B routes;
// selecting local only controls new tasks and does not disable already-frozen
// B2B routes when that block is present.
//
// 返回值中的清理函数用于在退出时释放 B2B 空闲连接。按 Wire 约定返回 func()
// 是让组合图在 shutdown 路径上真正调用 Close 的唯一方式——手工在生成代码里
// 插入 Close 会与 `wire diff` 不一致，而那种不一致会在下次重新生成时被抹掉。
func NewProviderRegistry(security *conf.Security, integrations *conf.Integrations, catalogs bizgeneration.MappingCatalogStore) (*ProviderRegistry, func(), error) {
	registry, err := newProviderRegistry(security, integrations, catalogs)
	if err != nil {
		return nil, nil, err
	}
	return registry, registry.Close, nil
}

func newProviderRegistry(security *conf.Security, integrations *conf.Integrations, catalogs bizgeneration.MappingCatalogStore) (*ProviderRegistry, error) {
	if security == nil || integrations == nil || integrations.GetGeneration() == nil {
		return nil, ErrProviderRegistryConfig
	}
	g := integrations.GetGeneration()
	selected := conf.EffectiveGenerationProvider(g)
	if selected != conf.ProviderLocalExecutionV2 && selected != conf.ProviderPolarStarB2BV2 {
		return nil, ErrProviderRegistryConfig
	}
	r := &ProviderRegistry{newProvider: selected}

	// Local credentials are optional when B2B is the only configured profile;
	// build local only when a complete local target/key is present. This avoids
	// making B2B startup depend on stale local drain credentials.
	localConfigured := strings.TrimSpace(g.GetBaseUrl()) != "" || strings.TrimSpace(g.GetCallbackBaseUrl()) != "" || strings.TrimSpace(g.GetApiKey()) != "" || strings.TrimSpace(security.GetGenerationRequestHmacKey()) != "" || strings.TrimSpace(security.GetGenerationCallbackHmacKey()) != ""
	if localConfigured {
		client, err := NewConfiguredLocalClient(security, integrations)
		if err != nil {
			return nil, err
		}
		r.local = client
	}

	if b := g.GetPolarstarB2B(); b != nil {
		// polarstarb2b.NewClient performs independent URL, identity, timeout and
		// transport-bound checks. Global ValidateGeneration additionally checks
		// delivery mode, callback and result host policy before this constructor.
		client, err := polarstarb2b.NewClient(polarstarb2b.ClientOptions{
			BaseURL: b.GetBaseUrl(), APIKey: b.GetApiKey(), AccountRef: b.GetAccountRef(), ExpectedTenantID: b.GetTenantId(),
			Timeout: durationValue(b.GetHttpTimeout()), ConnectTimeout: durationValue(b.GetConnectTimeout()),
			ResponseHeaderTimeout: durationValue(b.GetResponseHeaderTimeout()), MaxConnectionsPerHost: int(b.GetMaxConnectionsPerHost()),
			MaxResponseBytes: b.GetMaxResponseBytes(),
		})
		if err != nil {
			return nil, err
		}
		config, err := newB2BSubmissionConfig(b, catalogs)
		if err != nil {
			return nil, err
		}
		r.b2b, r.b2bAccount, r.b2bTenant, r.b2bMode = client, b.GetAccountRef(), b.GetTenantId(), b.GetDeliveryMode()
		r.maxInFlight, r.maxResult = b.GetMaxInFlight(), b.GetMaxResultBytes()
		r.allowlist = append([]string(nil), b.GetResultHostAllowlist()...)
		r.b2bConfig = config
	}

	if selected == conf.ProviderLocalExecutionV2 && r.local == nil {
		return nil, ErrProviderNotEnabled
	}
	if selected == conf.ProviderPolarStarB2BV2 && r.b2b == nil {
		return nil, ErrProviderNotEnabled
	}
	return r, nil
}

// NewTaskProvider returns the provider for a newly admitted task. It never
// consults a historical step route and never falls back when disabled.
func (r *ProviderRegistry) NewTaskProvider() (ProviderHandle, error) {
	if r == nil {
		return ProviderHandle{}, ErrProviderNotEnabled
	}
	return r.handle(r.newProvider)
}

// Close releases idle B2B transport connections during application shutdown.
// It is intentionally idempotent and does not cancel in-flight requests.
func (r *ProviderRegistry) Close() {
	if r != nil && r.b2b != nil {
		r.b2b.CloseIdleConnections()
	}
}

// ProviderForRoute resolves a persisted route provider and account. The
// account check is part of the registry boundary so a rotated/misconfigured
// B2B credential cannot claim another tenant's step.
func (r *ProviderRegistry) ProviderForRoute(provider, accountRef string) (ProviderHandle, error) {
	if r == nil {
		return ProviderHandle{}, ErrProviderNotEnabled
	}
	h, err := r.handle(provider)
	if err != nil {
		return ProviderHandle{}, err
	}
	if strings.TrimSpace(accountRef) == "" || h.AccountRef != accountRef {
		return ProviderHandle{}, ErrProviderRouteMismatch
	}
	return h, nil
}

// B2BSubmission 返回 B2B 提交分支所需的只读事实。
// ok 为 false 表示运行配置没有授权 B2B（或 B2B 提交尚不可用）；调用方必须
// 据此拒绝提交，绝不能退回本地执行。
func (r *ProviderRegistry) B2BSubmission() (B2BSubmissionConfig, bool) {
	if r == nil || r.b2b == nil || !r.b2bConfig.Ready() {
		return B2BSubmissionConfig{}, false
	}
	return r.b2bConfig, true
}

func (r *ProviderRegistry) handle(provider string) (ProviderHandle, error) {
	switch provider {
	case conf.ProviderLocalExecutionV2:
		if r.local == nil {
			return ProviderHandle{}, ErrProviderNotEnabled
		}
		return ProviderHandle{ID: provider, Local: r.local, AccountRef: "local-default"}, nil
	case conf.ProviderPolarStarB2BV2:
		if r.b2b == nil {
			return ProviderHandle{}, ErrProviderNotEnabled
		}
		return ProviderHandle{ID: provider, B2B: r.b2b, AccountRef: r.b2bAccount, TenantID: r.b2bTenant, DeliveryMode: r.b2bMode, MaxInFlight: r.maxInFlight, MaxResultBytes: r.maxResult, ResultHostAllowlist: append([]string(nil), r.allowlist...)}, nil
	default:
		return ProviderHandle{}, ErrProviderNotEnabled
	}
}

func durationValue(value *durationpb.Duration) time.Duration {
	if value == nil {
		return 0
	}
	return value.AsDuration()
}
