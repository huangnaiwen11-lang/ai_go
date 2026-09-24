package generation

import (
	"context"
	"errors"

	"ai-business-service/internal/biz/creations"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/integrations/polarstarb2b"
)

// ErrAdmissionConfig 表示运行配置与 admission 需求不一致：选择了非本地 provider，
// 却没有可用的 provider 账号配置或已发布目录来源。组合根必须据此拒绝启动。
var ErrAdmissionConfig = errors.New("generation admission configuration unavailable")

// NewAdmissionResolver 按新任务选择器构造步骤归属解析器。
//
// 选择本地时返回只冻结本地归属的解析器，不需要目录；选择 B2B 时必须同时提供
// B2B 账号配置与已发布目录来源，否则返回 ErrAdmissionConfig，由组合根拒绝启动。
//
// 这是「selector 选 B2B 而新步骤仍被冻成本地」的唯一防线：没有它，用户请求的
// provider 与实际冻结的归属会不一致，而这种不一致不会在任何地方报错——步骤照常
// 创建、照常预扣，只是最终跑在了本地模拟器上。
//
// 形参取整个 Integrations 而不是 generation 子块：Wire 图里提供的是前者，
// 多一层派生类型只会让组合根绕开校验。
func NewAdmissionResolver(integrations *conf.Integrations, catalogs bizgeneration.MappingCatalogStore) (creations.AdmissionResolver, error) {
	if integrations == nil || integrations.GetGeneration() == nil {
		return nil, ErrAdmissionConfig
	}
	g := integrations.GetGeneration()
	switch conf.EffectiveGenerationProvider(g) {
	case conf.ProviderLocalExecutionV2:
		return creations.NewLocalAdmissionResolver(), nil
	case conf.ProviderPolarStarB2BV2:
	default:
		return nil, ErrAdmissionConfig
	}
	// B2B 账号是冻结归属的一部分；目录是 mapping_version 的唯一来源。
	// 二者缺一都不能安全地冻出 B2B 步骤，因此在这里就拒绝，而不是等到创建请求进来。
	b := g.GetPolarstarB2B()
	if b == nil || b.GetAccountRef() == "" || catalogs == nil {
		return nil, ErrAdmissionConfig
	}
	return &b2bAdmissionResolver{accountRef: b.GetAccountRef(), catalogs: catalogs}, nil
}

// NewAdmissionResolverWithStorageReadiness is the production composition
// factory. NewAdmissionResolver remains the pure routing/recipe constructor
// used by focused unit tests; every HTTP/GRPC composition root must call this
// wrapper so B2B cannot accept a billable task before result storage is ready.
func NewAdmissionResolverWithStorageReadiness(
	integrations *conf.Integrations,
	catalogs bizgeneration.MappingCatalogStore,
	readiness B2BAdmissionStorageReadiness,
) (creations.AdmissionResolver, error) {
	if integrations == nil || integrations.GetGeneration() == nil {
		return nil, ErrAdmissionConfig
	}
	if conf.EffectiveGenerationProvider(integrations.GetGeneration()) == conf.ProviderPolarStarB2BV2 {
		if err := readiness.RequireB2B(); err != nil {
			return nil, err
		}
	}
	return NewAdmissionResolver(integrations, catalogs)
}

// b2bAdmissionResolver 把服务端模板编译器编译出的公开产品配方解析为冻结的 B2B 归属。
//
// 目录版本取「当前最新已发布版本」，并随步骤一起冻结：已发布版本不可原地修改，
// 因此提交时用同一版本重放会得到同一份映射，不会因目录后续更新而漂移。
//
// 本类型不校验公开合同的字段语义（model 命名、字段与 capability 的对应、URL 可达性）。
// 那些仍由 polarstarb2b 适配器在映射时判定并保持唯一权威，避免两处规则漂移。
type b2bAdmissionResolver struct {
	accountRef string
	catalogs   bizgeneration.MappingCatalogStore
}

func (resolver *b2bAdmissionResolver) ResolveAdmission(ctx context.Context, request creations.AdmissionRequest) (creations.StepAdmission, error) {
	catalog, err := resolver.catalogs.PublishedCatalog(ctx, "")
	if err != nil {
		return creations.StepAdmission{}, creations.ErrAdmissionDependencyUnavailable
	}
	if catalog == nil {
		return creations.StepAdmission{}, creations.ErrAdmissionConfigurationUnavailable
	}
	published, err := catalog.Normalize()
	if err != nil {
		return creations.StepAdmission{}, creations.ErrAdmissionConfigurationUnavailable
	}
	return resolver.resolveAdmissionWithCatalog(request, published)
}

// ResolveAdmissions pins every step in one creation request to one catalog
// read. It matters specifically for B2B text-to-video: resolving its two
// product keys against separately read "latest" catalogs could otherwise
// freeze a mixed mapping generation during a publication race.
func (resolver *b2bAdmissionResolver) ResolveAdmissions(ctx context.Context, requests []creations.AdmissionRequest) ([]creations.StepAdmission, error) {
	if len(requests) == 0 {
		return nil, creations.ErrAdmissionConfigurationUnavailable
	}
	catalog, err := resolver.catalogs.PublishedCatalog(ctx, "")
	if err != nil {
		return nil, creations.ErrAdmissionDependencyUnavailable
	}
	if catalog == nil {
		return nil, creations.ErrAdmissionConfigurationUnavailable
	}
	published, err := catalog.Normalize()
	if err != nil {
		return nil, creations.ErrAdmissionConfigurationUnavailable
	}
	admissions := make([]creations.StepAdmission, 0, len(requests))
	for _, request := range requests {
		admission, err := resolver.resolveAdmissionWithCatalog(request, published)
		if err != nil {
			return nil, err
		}
		admissions = append(admissions, admission)
	}
	return admissions, nil
}

func (resolver *b2bAdmissionResolver) resolveAdmissionWithCatalog(request creations.AdmissionRequest, published bizgeneration.PublishedMappingCatalog) (creations.StepAdmission, error) {
	if request.B2B == nil {
		// 模板编译器还没有按公开合同编译这个步骤。选择 B2B 时绝不能退回
		// 本地执行，也不能冻出一个没有配方的 B2B 步骤。
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	recipe, err := request.B2B.Normalize()
	if err != nil {
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	entry, ok := published.Entry(recipe.ProductKey)
	if !ok || !entry.Enabled {
		// 未知或已停用的产品键都不参与新任务映射；停用条目只用于审计与历史重放。
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	// 步骤原子与目录 capability 必须一致：不一致说明模板编译器用错了产品键。
	if entry.Capability != string(request.Atom) {
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	if recipe.TemplateKey != "" {
		if _, ok := entry.Template(recipe.TemplateKey); !ok {
			return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
		}
	}
	// 预扣前复用最终 adapter 的公开合同校验。这里不能只认 productKey：若
	// 配方里有目录未允许的字段、比例或素材，等 Worker 再发现已经太晚。
	snapshot, err := MappingSnapshotFromCatalog(published, published.Version)
	if err != nil {
		return creations.StepAdmission{}, creations.ErrAdmissionConfigurationUnavailable
	}
	product, err := ProductInputFromRecipe(recipe, entry.Capability)
	if err != nil {
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	if request.DeferredB2B {
		if request.Atom != creations.AtomImageToVideo || request.Sequence != 2 {
			return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
		}
		if _, err := (creations.DeferredB2BImageToVideo{Recipe: recipe}).Normalize(); err != nil || polarstarb2b.ValidateDeferredImageToVideoProduct(snapshot, product) != nil {
			return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
		}
	} else if polarstarb2b.ValidateProduct(snapshot, product) != nil {
		return creations.StepAdmission{}, creations.ErrB2BProductRecipeUnavailable
	}
	route := creations.ExecutionRoute{
		Provider:        creations.PolarStarB2BProvider,
		AccountRef:      resolver.accountRef,
		ContractVersion: creations.B2BContractVersion,
		MappingVersion:  published.Version,
	}
	// 目录版本号要同时充当冻结的 mapping_version。若它不满足归属版本规则，
	// 说明发布侧写入了不能作为执行身份的版本号，这里立即失败而不是留到映射时。
	if _, err := creations.NormalizeExecutionRoute(route); err != nil {
		return creations.StepAdmission{}, creations.ErrAdmissionConfigurationUnavailable
	}
	return creations.StepAdmission{Route: route, B2B: &recipe}, nil
}

var _ creations.AdmissionResolver = (*b2bAdmissionResolver)(nil)
var _ creations.BatchAdmissionResolver = (*b2bAdmissionResolver)(nil)
