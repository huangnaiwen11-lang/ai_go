package generation

import (
	"errors"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/conf"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestProviderRegistryDefaultsToLocalOnly(t *testing.T) {
	security, integrations := registryLocalConfig()
	r, err := newProviderRegistry(security, integrations, registryCatalogs())
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.NewTaskProvider()
	if err != nil || !h.IsLocal() {
		t.Fatalf("new task provider = %#v, %v", h, err)
	}
	if _, err := r.ProviderForRoute(conf.ProviderPolarStarB2BV2, "account-a"); !errors.Is(err, ErrProviderNotEnabled) {
		t.Fatalf("unconfigured b2b route must stay disabled, got %v", err)
	}
}

func TestProviderRegistrySeparatesSelectorFromHistoricalRoutes(t *testing.T) {
	security, integrations := registryLocalConfig()
	integrations.Generation.PolarstarB2B = registryB2BConfig()
	// Keep local selected: B2B is constructed only because its explicit block
	// is present, but it must not become the new-task provider.
	r, err := newProviderRegistry(security, integrations, registryCatalogs())
	if err != nil {
		t.Fatal(err)
	}
	newTask, err := r.NewTaskProvider()
	if err != nil || !newTask.IsLocal() {
		t.Fatalf("selector must remain local, got %#v, %v", newTask, err)
	}
	historical, err := r.ProviderForRoute(conf.ProviderPolarStarB2BV2, "account-a")
	if err != nil || !historical.IsB2B() || historical.TenantID != "tenant-a" || historical.DeliveryMode != "lookup_only" {
		t.Fatalf("historical b2b route = %#v, %v", historical, err)
	}
	if _, err := r.ProviderForRoute(conf.ProviderPolarStarB2BV2, "another-account"); !errors.Is(err, ErrProviderRouteMismatch) {
		t.Fatalf("wrong b2b account must be rejected, got %v", err)
	}
}

func TestProviderRegistryB2BSelectionRequiresExplicitConfig(t *testing.T) {
	security, integrations := registryLocalConfig()
	integrations.Generation.Provider = conf.ProviderPolarStarB2BV2
	integrations.Generation.BaseUrl = ""
	integrations.Generation.CallbackBaseUrl = ""
	integrations.Generation.ApiKey = ""
	security.GenerationRequestHmacKey = ""
	if _, err := newProviderRegistry(security, integrations, registryCatalogs()); !errors.Is(err, ErrProviderNotEnabled) {
		t.Fatalf("b2b selection without b2b block must fail closed, got %v", err)
	}

	integrations.Generation.PolarstarB2B = registryB2BConfig()
	r, err := newProviderRegistry(security, integrations, registryCatalogs())
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.NewTaskProvider()
	if err != nil || !h.IsB2B() || h.AccountRef != "account-a" {
		t.Fatalf("b2b new task provider = %#v, %v", h, err)
	}
}

func TestProviderRegistryDoesNotFallbackToB2BForLocalSelector(t *testing.T) {
	security, integrations := registryLocalConfig()
	integrations.Generation.BaseUrl = ""
	integrations.Generation.CallbackBaseUrl = ""
	integrations.Generation.ApiKey = ""
	security.GenerationRequestHmacKey = ""
	integrations.Generation.PolarstarB2B = registryB2BConfig()
	if _, err := newProviderRegistry(security, integrations, registryCatalogs()); !errors.Is(err, ErrProviderNotEnabled) {
		t.Fatalf("local selector without local provider must not fall back to b2b, got %v", err)
	}
}

// 组合根必须拿到可用的清理函数：B2B 空闲连接要在退出路径上被释放，
// 而不是靠进程结束顺手带走。
func TestProviderRegistryExposesIdempotentCleanup(t *testing.T) {
	security, integrations := registryLocalConfig()
	integrations.Generation.PolarstarB2B = registryB2BConfig()
	registry, closeRegistry, err := NewProviderRegistry(security, integrations, registryCatalogs())
	if err != nil {
		t.Fatal(err)
	}
	if registry == nil || closeRegistry == nil {
		t.Fatal("注册表与清理函数必须同时提供")
	}
	// 重复调用必须安全：shutdown 与错误分支可能各调用一次。
	closeRegistry()
	closeRegistry()
}

// 构造失败时不得返回半成品清理函数，否则组合根会对 nil 注册表调用 Close。
func TestProviderRegistryRejectsConfigWithoutCleanup(t *testing.T) {
	security, integrations := registryLocalConfig()
	integrations.Generation.Provider = conf.ProviderPolarStarB2BV2
	registry, closeRegistry, err := NewProviderRegistry(security, integrations, registryCatalogs())
	if !errors.Is(err, ErrProviderNotEnabled) || registry != nil {
		t.Fatalf("NewProviderRegistry() = %#v, %v; want nil registry and ErrProviderNotEnabled", registry, err)
	}
	if closeRegistry != nil {
		t.Fatal("构造失败时不得返回清理函数：组合根会把它当成功路径调用")
	}
}

// registryCatalogs 提供注册表构造所需的目录来源。
//
// 它必须是可用的已发布目录：配置了 B2B 账号却没有目录来源时，构造期就应当
// 失败（ErrB2BSubmissionConfig），因为任何提交都无法把冻结的 mapping_version
// 还原成映射。测试若传 nil，等于把「缺目录」当成合法 profile。
func registryCatalogs() *stubCatalogStore {
	return &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}
}

func registryLocalConfig() (*conf.Security, *conf.Integrations) {
	return &conf.Security{GenerationRequestHmacKey: strings.Repeat("r", 32)}, &conf.Integrations{Generation: &conf.Integrations_Generation{
		BaseUrl: "http://127.0.0.1:19081", CallbackBaseUrl: "http://127.0.0.1:18000", ApiKey: "test-only-local-key",
	}}
}

func registryB2BConfig() *conf.Integrations_PolarStarB2B {
	return &conf.Integrations_PolarStarB2B{
		AccountRef: "account-a", TenantId: "tenant-a", BaseUrl: "https://api.example.test", ApiKey: "test-only-b2b-key", DeliveryMode: "lookup_only",
		ResultHostAllowlist: []string{"cdn.example.test"}, HttpTimeout: durationpb.New(30 * time.Second), ConnectTimeout: durationpb.New(5 * time.Second), ResponseHeaderTimeout: durationpb.New(10 * time.Second),
		MaxConnectionsPerHost: 4, MaxResponseBytes: 1 << 20, MaxResultBytes: 8 << 20, MaxInFlight: 2,
	}
}
