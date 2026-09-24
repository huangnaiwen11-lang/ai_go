package generation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"ai-business-service/internal/biz/creations"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/conf"
)

type stubCatalogStore struct {
	catalog *bizgeneration.PublishedMappingCatalog
	err     error
	calls   int
}

func (store *stubCatalogStore) PublishedCatalog(context.Context, string) (*bizgeneration.PublishedMappingCatalog, error) {
	store.calls++
	if store.err != nil {
		return nil, store.err
	}
	if store.catalog == nil {
		return nil, bizgeneration.ErrMappingCatalogUnavailable
	}
	return store.catalog, nil
}

func b2bGenerationConfig() *conf.Integrations_Generation {
	return &conf.Integrations_Generation{
		Provider:     conf.ProviderPolarStarB2BV2,
		PolarstarB2B: &conf.Integrations_PolarStarB2B{AccountRef: "acct-main"},
	}
}

// integrationsFor 把 generation 子块包成组合根实际提供的形状。
// 解析器刻意只接受整个 Integrations：多一层派生类型会让组合根绕开「选择器与
// 账号/目录必须同时就位」的校验。
func integrationsFor(g *conf.Integrations_Generation) *conf.Integrations {
	return &conf.Integrations{Generation: g}
}

func imageRecipe() *creations.B2BProductRecipe {
	return &creations.B2BProductRecipe{
		ProductKey: "image",
		Input:      json.RawMessage(`{"prompt":"海边的灯塔","aspectRatio":"1:1"}`),
	}
}

func TestNewAdmissionResolver选择本地时不需要目录(t *testing.T) {
	resolver, err := NewAdmissionResolver(integrationsFor(&conf.Integrations_Generation{}), nil)
	if err != nil {
		t.Fatalf("NewAdmissionResolver() error = %v", err)
	}
	admission, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1})
	if err != nil || admission.Route != creations.LocalExecutionRoute() {
		t.Fatalf("ResolveAdmission() = %#v, %v; want local route", admission, err)
	}
	// 本地选择器下携带公开配方说明调用方已按 B2B 编译，必须拒绝而不是丢弃配方。
	recipe := imageRecipe()
	if _, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: recipe}); !errors.Is(err, creations.ErrAdmissionConfigurationUnavailable) {
		t.Fatalf("ResolveAdmission() error = %v, want ErrAdmissionConfigurationUnavailable", err)
	}
}

// 选择 B2B 却没有账号或目录来源时，组合根必须拿不到解析器：否则新步骤会被冻成本地。
func TestNewAdmissionResolver选择B2B缺少来源时拒绝构造(t *testing.T) {
	catalogStore := &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}
	cases := map[string]struct {
		generation *conf.Integrations_Generation
		catalogs   bizgeneration.MappingCatalogStore
	}{
		"缺少配置":   {nil, catalogStore},
		"缺少B2B块": {&conf.Integrations_Generation{Provider: conf.ProviderPolarStarB2BV2}, catalogStore},
		"缺少账号引用": {&conf.Integrations_Generation{Provider: conf.ProviderPolarStarB2BV2, PolarstarB2B: &conf.Integrations_PolarStarB2B{}}, catalogStore},
		"缺少目录来源": {b2bGenerationConfig(), nil},
		"未知选择器":  {&conf.Integrations_Generation{Provider: "unknown_provider", PolarstarB2B: &conf.Integrations_PolarStarB2B{AccountRef: "acct-main"}}, catalogStore},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAdmissionResolver(integrationsFor(testCase.generation), testCase.catalogs); !errors.Is(err, ErrAdmissionConfig) {
				t.Fatalf("NewAdmissionResolver() error = %v, want ErrAdmissionConfig", err)
			}
		})
	}
}

func TestB2BAdmissionResolver冻结归属与目录版本(t *testing.T) {
	store := &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}
	resolver, err := NewAdmissionResolver(integrationsFor(b2bGenerationConfig()), store)
	if err != nil {
		t.Fatalf("NewAdmissionResolver() error = %v", err)
	}

	admission, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{
		UserID: "user-1", TemplateID: "template-1", TemplateVersion: 1,
		Atom: creations.AtomTextToImage, Sequence: 1, B2B: imageRecipe(),
	})
	if err != nil {
		t.Fatalf("ResolveAdmission() error = %v", err)
	}
	normalized, err := admission.Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	want := creations.ExecutionRoute{
		Provider:        creations.PolarStarB2BProvider,
		AccountRef:      "acct-main",
		ContractVersion: creations.B2BContractVersion,
		MappingVersion:  goldenMappingVersion,
	}
	if normalized.Route != want {
		t.Fatalf("route = %#v, want %#v", normalized.Route, want)
	}
	if normalized.B2B == nil || normalized.B2B.ProductKey != "image" {
		t.Fatalf("recipe = %#v", normalized.B2B)
	}
	// 冻结的 mapping_version 必须能投影出快照，否则提交时会拿不到映射。
	if _, err := MappingSnapshotFromCatalog(goldenCatalog(), normalized.Route.MappingVersion); err != nil {
		t.Fatalf("MappingSnapshotFromCatalog() error = %v", err)
	}
}

func TestB2BAdmissionResolver接受已知模板键(t *testing.T) {
	store := &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}
	resolver, err := NewAdmissionResolver(integrationsFor(b2bGenerationConfig()), store)
	if err != nil {
		t.Fatalf("NewAdmissionResolver() error = %v", err)
	}
	recipe := &creations.B2BProductRecipe{
		ProductKey:  "edit",
		TemplateKey: "portrait",
		Input:       json.RawMessage(`{"imageUrl":"https://cdn.example.com/source.png"}`),
	}
	admission, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{Atom: creations.AtomImageEdit, Sequence: 1, B2B: recipe})
	if err != nil {
		t.Fatalf("ResolveAdmission() error = %v", err)
	}
	if admission.Route.Provider != creations.PolarStarB2BProvider {
		t.Fatalf("route = %#v", admission.Route)
	}
}

func TestB2BAdmissionResolver接受未绑定首帧的第二阶段并冻结同一目录版本(t *testing.T) {
	catalog := goldenCatalog()
	catalog.Entries = append(catalog.Entries, bizgeneration.ModelMappingEntry{
		Capability: "image_to_video", ProductKey: "video", PublicModel: "ps-rush-v1",
		AllowedInputs: []string{"prompt", "durationSeconds", "aspectRatio"}, AspectRatios: []string{"9:16"}, Durations: []int{5, 10, 15}, Enabled: true,
	})
	store := &stubCatalogStore{catalog: catalogPtr(catalog)}
	resolver, err := NewAdmissionResolver(integrationsFor(b2bGenerationConfig()), store)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := creations.CompileDeferredB2BImageToVideo(creations.PublishedB2BProductRecipe{
		TemplateID: "video-template", TemplateVersion: 7, Atom: creations.AtomImageToVideo,
		ProductKey: "video", Input: json.RawMessage(`{"prompt":"cinematic motion","durationSeconds":10,"aspectRatio":"9:16"}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{
		TemplateID: "video-template", TemplateVersion: 7, Atom: creations.AtomImageToVideo, Sequence: 2,
		B2B: &deferred.Recipe, DeferredB2B: true,
	})
	if err != nil {
		t.Fatalf("ResolveAdmission() error = %v", err)
	}
	if admission.Route.Provider != creations.PolarStarB2BProvider || admission.Route.MappingVersion != goldenMappingVersion || admission.B2B == nil || len(admission.B2B.Assets) != 0 {
		t.Fatalf("admission = %#v", admission)
	}
	if _, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{
		TemplateID: "video-template", TemplateVersion: 7, Atom: creations.AtomImageToVideo, Sequence: 2, B2B: &deferred.Recipe,
	}); !errors.Is(err, creations.ErrB2BProductRecipeUnavailable) {
		t.Fatalf("regular admission without opening frame error = %v, want ErrB2BProductRecipeUnavailable", err)
	}
	prebound := deferred.Recipe
	prebound.Assets = []creations.B2BAsset{{Role: "opening_frame", URL: "https://assets.example.test/frame.png"}}
	if _, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{
		TemplateID: "video-template", TemplateVersion: 7, Atom: creations.AtomImageToVideo, Sequence: 2, B2B: &prebound, DeferredB2B: true,
	}); !errors.Is(err, creations.ErrB2BProductRecipeUnavailable) {
		t.Fatalf("prebound deferred admission error = %v, want ErrB2BProductRecipeUnavailable", err)
	}
}

// 创建预扣之前就必须按同一份公开映射校验输入。若只检查 productKey，模板发布
// 时拼错字段会在用户已预扣后才由提交 Worker 发现，导致无意义的失败与冲正。
func TestB2BAdmissionResolver在预扣前校验公开产品输入(t *testing.T) {
	store := &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}
	resolver, err := NewAdmissionResolver(integrationsFor(b2bGenerationConfig()), store)
	if err != nil {
		t.Fatalf("NewAdmissionResolver() error = %v", err)
	}
	for name, recipe := range map[string]*creations.B2BProductRecipe{
		"目录未允许的字段":  {ProductKey: "image", Input: json.RawMessage(`{"prompt":"灯塔","privateWorkflow":"never"}`)},
		"公开产品约束不成立": {ProductKey: "image", Input: json.RawMessage(`{"prompt":"灯塔","aspectRatio":"3:7"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			admission, err := resolver.ResolveAdmission(context.Background(), creations.AdmissionRequest{
				Atom: creations.AtomTextToImage, Sequence: 1, B2B: recipe,
			})
			if !errors.Is(err, creations.ErrB2BProductRecipeUnavailable) {
				t.Fatalf("ResolveAdmission() error = %v, want ErrB2BProductRecipeUnavailable", err)
			}
			if admission.Route != (creations.ExecutionRoute{}) || admission.B2B != nil {
				t.Fatalf("非法产品输入不得产生冻结归属：%#v", admission)
			}
		})
	}
}

// 每一项都必须是 fail closed：绝不能退回本地执行，也不能冻出没有配方的 B2B 步骤。
func TestB2BAdmissionResolverFailClosed(t *testing.T) {
	cases := map[string]struct {
		generation *conf.Integrations_Generation
		store      *stubCatalogStore
		request    creations.AdmissionRequest
		want       error
	}{
		"缺少公开配方": {b2bGenerationConfig(), &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1}, creations.ErrB2BProductRecipeUnavailable},
		"目录不可用":  {b2bGenerationConfig(), &stubCatalogStore{err: bizgeneration.ErrMappingCatalogUnavailable}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: imageRecipe()}, creations.ErrAdmissionDependencyUnavailable},
		"目录为空":   {b2bGenerationConfig(), &stubCatalogStore{}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: imageRecipe()}, creations.ErrAdmissionDependencyUnavailable},
		"目录结构损坏": {b2bGenerationConfig(), &stubCatalogStore{catalog: brokenCatalogPtr()}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: imageRecipe()}, creations.ErrAdmissionConfigurationUnavailable},
		"未知产品键":  {b2bGenerationConfig(), &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: &creations.B2BProductRecipe{ProductKey: "unknown", Input: json.RawMessage(`{"prompt":"x"}`)}}, creations.ErrB2BProductRecipeUnavailable},
		"产品键已停用": {b2bGenerationConfig(), &stubCatalogStore{catalog: catalogPtr(disabledCatalog())}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: imageRecipe()}, creations.ErrB2BProductRecipeUnavailable},
		"能力不匹配":  {b2bGenerationConfig(), &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}, creations.AdmissionRequest{Atom: creations.AtomImageEdit, Sequence: 1, B2B: imageRecipe()}, creations.ErrB2BProductRecipeUnavailable},
		"未知模板键":  {b2bGenerationConfig(), &stubCatalogStore{catalog: catalogPtr(goldenCatalog())}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: &creations.B2BProductRecipe{ProductKey: "image", TemplateKey: "unknown", Input: json.RawMessage(`{"prompt":"x"}`)}}, creations.ErrB2BProductRecipeUnavailable},
		// 目录版本要同时充当冻结的 mapping_version；不能作为执行身份的版本号必须在 admission 就失败。
		// "map/one" 能通过目录的结构校验（只要求无空白且有界），但含 "/" 不是合法执行身份。
		"目录版本不能作为执行身份": {b2bGenerationConfig(), &stubCatalogStore{catalog: catalogPtr(catalogWithVersion("map/one"))}, creations.AdmissionRequest{Atom: creations.AtomTextToImage, Sequence: 1, B2B: imageRecipe()}, creations.ErrAdmissionConfigurationUnavailable},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			resolver, err := NewAdmissionResolver(integrationsFor(testCase.generation), testCase.store)
			if err != nil {
				t.Fatalf("NewAdmissionResolver() error = %v", err)
			}
			admission, err := resolver.ResolveAdmission(context.Background(), testCase.request)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("ResolveAdmission() error = %v, want %v", err, testCase.want)
			}
			if admission.Route != (creations.ExecutionRoute{}) || admission.B2B != nil {
				t.Fatalf("失败时不得返回任何归属：%#v", admission)
			}
		})
	}
}

func catalogPtr(catalog bizgeneration.PublishedMappingCatalog) *bizgeneration.PublishedMappingCatalog {
	return &catalog
}

func disabledCatalog() bizgeneration.PublishedMappingCatalog {
	catalog := goldenCatalog()
	for index := range catalog.Entries {
		catalog.Entries[index].Enabled = false
	}
	return catalog
}

func brokenCatalogPtr() *bizgeneration.PublishedMappingCatalog {
	catalog := goldenCatalog()
	catalog.Entries[0].PublicModel = ""
	return &catalog
}

func catalogWithVersion(version string) bizgeneration.PublishedMappingCatalog {
	catalog := goldenCatalog()
	catalog.Version = version
	return catalog
}
