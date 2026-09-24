package generation

import (
	"context"
	"errors"
	"strings"
	"testing"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/integrations/polarstarb2b"
)

// submissionCatalogStore 只回答目录读取，不做任何校验：它要证明的是提交分支
// 把「目录来源」原样带进配置，而不是替仓储再验一遍。
type submissionCatalogStore struct {
	catalog bizgeneration.PublishedMappingCatalog
	err     error
}

func (store *submissionCatalogStore) PublishedCatalog(context.Context, string) (*bizgeneration.PublishedMappingCatalog, error) {
	if store.err != nil {
		return nil, store.err
	}
	catalog := store.catalog
	return &catalog, nil
}

var _ bizgeneration.MappingCatalogStore = (*submissionCatalogStore)(nil)

func submissionConfig(t *testing.T) *conf.Integrations_PolarStarB2B {
	t.Helper()
	return &conf.Integrations_PolarStarB2B{
		AccountRef: "account-a", TenantId: "tenant-1", BaseUrl: "https://api.example.test",
		ApiKey: strings.Repeat("a", 32), DeliveryMode: "lookup_only",
	}
}

func TestNewB2BSubmissionConfig未配置B2B时返回零值(t *testing.T) {
	config, err := newB2BSubmissionConfig(nil, &submissionCatalogStore{})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	// 本地 profile 合法：零值必须不可用，但也不能是个错误。
	if config.Ready() {
		t.Fatalf("零值配置报告可用: %#v", config)
	}
}

func TestNewB2BSubmissionConfig缺少目录来源时拒绝(t *testing.T) {
	if _, err := newB2BSubmissionConfig(submissionConfig(t), nil); !errors.Is(err, ErrB2BSubmissionConfig) {
		t.Fatalf("error = %v, want ErrB2BSubmissionConfig", err)
	}
}

func TestNewB2BSubmissionConfig未知交付模式时拒绝(t *testing.T) {
	for _, mode := range []string{"", "auto", "polling"} {
		b := submissionConfig(t)
		b.DeliveryMode = mode
		if _, err := newB2BSubmissionConfig(b, &submissionCatalogStore{}); !errors.Is(err, ErrB2BSubmissionConfig) {
			t.Fatalf("模式 %q 错误 = %v，期望 ErrB2BSubmissionConfig", mode, err)
		}
	}
}

func TestNewB2BSubmissionConfig查找模式禁用回调(t *testing.T) {
	catalogs := &submissionCatalogStore{}
	config, err := newB2BSubmissionConfig(submissionConfig(t), catalogs)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !config.Ready() || config.Catalogs != catalogs {
		t.Fatalf("配置 = %#v", config)
	}
	// 查找模式不得声明回调地址：声明了却没人接收，平台会按有界投递打满 8 次。
	if config.Callback.Mode != "lookup_only" || config.Callback.URL != "" {
		t.Fatalf("回调配置 = %#v", config.Callback)
	}
}

// webhook 交付要求一个平台可达的回调地址。缺少回调源时必须在构造期拒绝，
// 而不是把任务送出去再等平台投递到一个不存在的地址。
func TestNewB2BSubmissionConfigwebhook缺少回调源时拒绝(t *testing.T) {
	b := submissionConfig(t)
	b.DeliveryMode = "webhook"
	if _, err := newB2BSubmissionConfig(b, &submissionCatalogStore{}); !errors.Is(err, ErrB2BSubmissionConfig) {
		t.Fatalf("error = %v, want ErrB2BSubmissionConfig", err)
	}
}

// 配置层对保留域（.test/.invalid）比协议适配器宽松。这类地址永远不可能被平台
// 投递到，必须在构造期拒绝，而不是等第一笔任务进来才 ErrInvalidRequest。
func TestNewB2BSubmissionConfigwebhook拒绝保留域回调源(t *testing.T) {
	for _, origin := range []string{
		"https://callbacks.example.test",
		"https://callbacks.example.invalid",
		"http://callbacks.example.com",
		"https://callbacks.example.com:8443",
	} {
		b := submissionConfig(t)
		b.DeliveryMode, b.CallbackOrigin = "webhook", origin
		if _, err := newB2BSubmissionConfig(b, &submissionCatalogStore{}); !errors.Is(err, ErrB2BSubmissionConfig) {
			t.Fatalf("回调源 %q 错误 = %v，期望 ErrB2BSubmissionConfig", origin, err)
		}
	}
}

// 回调地址必须由「配置的公开源 + 网关实际接管的路径」拼出，并且要能被协议
// 适配器接受——适配器对回调地址的校验比配置校验更严，拼错就会在提交时
// ErrInvalidRequest，而不是在构造期被发现。
func TestNewB2BSubmissionConfigwebhook回调地址可被适配器接受(t *testing.T) {
	const origin = "https://callbacks.example.com"
	b := submissionConfig(t)
	b.DeliveryMode, b.CallbackOrigin = "webhook", origin

	config, err := newB2BSubmissionConfig(b, &submissionCatalogStore{})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !config.Ready() || config.Callback.Mode != "webhook" {
		t.Fatalf("配置 = %#v", config)
	}
	if want := origin + B2BCallbackPath; config.Callback.URL != want {
		t.Fatalf("回调地址 = %q，期望 %q", config.Callback.URL, want)
	}

	snapshot, err := MappingSnapshotFromCatalog(goldenCatalog(), goldenMappingVersion)
	if err != nil {
		t.Fatalf("MappingSnapshotFromCatalog() error = %v", err)
	}
	route := polarstarb2b.Route{
		StepID: "step-1", Provider: "polarstar_b2b_v2", AccountRef: "account-a",
		ContractVersion: "b2b.job.v2", MappingVersion: goldenMappingVersion,
	}
	request, err := polarstarb2b.MapRequest(snapshot, route, polarstarb2b.ProductInput{
		Capability: "text_to_image", ModelKey: "image",
		Input: polarstarb2b.Input{Prompt: "海边的灯塔", AspectRatio: "1:1"},
	}, config.Callback)
	if err != nil {
		t.Fatalf("MapRequest() error = %v", err)
	}
	payload := string(request.Payload())
	if !strings.Contains(payload, `"callbackUrl":"`+config.Callback.URL+`"`) ||
		!strings.Contains(payload, `"callbackPolicy":"bounded"`) {
		t.Fatalf("冻结请求未声明有界回调: %s", payload)
	}
	// 冻结字节必须能被适配器原样还原，否则对账路径无法用同一份字节重放。
	if _, err := polarstarb2b.RestoreRequest(route, request.Payload(), request.Digest()); err != nil {
		t.Fatalf("RestoreRequest() error = %v", err)
	}
}
