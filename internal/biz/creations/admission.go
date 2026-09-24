package creations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"ai-business-service/internal/biz/entitlement"
)

var (
	// ErrAdmissionDependencyUnavailable 表示目录、映射或其它外部依赖读取/解析失败。
	ErrAdmissionDependencyUnavailable = &admissionError{message: "creation admission dependency unavailable", code: "SERVICE_UNAVAILABLE"}
	// ErrAdmissionConfigurationUnavailable 表示发布配置或执行身份不一致，不能安全冻结归属。
	ErrAdmissionConfigurationUnavailable = &admissionError{message: "creation admission configuration unavailable", code: "GENERATION_CONFIGURATION_UNAVAILABLE"}
	// ErrInvalidAdmission 表示解析结果自身不合法：归属与公开配方互相矛盾，或新步骤
	// 给出了空归属。它与「不可用」区分开，便于运维区分授权缺失与实现缺陷。
	ErrInvalidAdmission = errors.New("creation admission invalid")
)

// admissionError 同时是领域哨兵和最小 HTTP 合同。这样 direct transport 与 Kratos
// error encoder 都能得到同一稳定 code，而不需要让 server 反向依赖具体 resolver。
type admissionError struct {
	message string
	code    string
}

func (e *admissionError) Error() string   { return e.message }
func (e *admissionError) StatusCode() int { return http.StatusServiceUnavailable }
func (e *admissionError) Code() string    { return e.code }
func (e *admissionError) Message() string { return e.message }
func (e *admissionError) Details() any    { return nil }

const (
	// maxAdmissionKeyLength 与已发布目录的 product key 规则保持一致。
	// admission 的校验不得比目录更严，否则目录里合法的条目会变成无法下单的死条目。
	maxAdmissionKeyLength = 512
	// maxAdmissionRecipeBytes 限制冻结配方的原始字节，避免把任意大输入写进创建事务。
	maxAdmissionRecipeBytes = 64 << 10
	// maxAdmissionAssets 与 B2B 适配器的素材角色上限一致；超出即拒绝，不截断。
	maxAdmissionAssets = 7
	// maxAdmissionAssetRoleLength 限制角色名长度，角色语义仍由适配器判定。
	maxAdmissionAssetRoleLength = 64
	// maxAdmissionAssetURLLength 只做长度护栏；公开可达性由适配器与发布方证明。
	maxAdmissionAssetURLLength = 8192
)

// B2BAsset 是上游已授权的素材引用及其产品角色。
// 它只是引用与角色，不含凭证，也不是长期作品地址。
type B2BAsset struct {
	Role string `json:"role"`
	URL  string `json:"url"`
}

// B2BProductRecipe 是服务端模板编译器为单个步骤编译出的公开产品配方。
//
// 它只承载 b2b.job.v2 的公开字段：execution.v2 的私有执行参数没有对应表示，
// 两种合同不得互相透传。账号、租户、回调地址与任何凭证都不在此类型中。
//
// 配方在创建事务内随发件箱载荷冻结，提交时只按冻结字节还原，不重新编译。
type B2BProductRecipe struct {
	ProductKey  string          `json:"productKey"`
	TemplateKey string          `json:"templateKey,omitempty"`
	Input       json.RawMessage `json:"input"`
	Assets      []B2BAsset      `json:"assets,omitempty"`
}

// StepAdmission 是一个步骤在创建时冻结的执行归属。
type StepAdmission struct {
	Route ExecutionRoute
	// B2B 只允许与 B2B 归属同时出现；本地步骤必须为 nil。
	// 这样「选了 B2B 却冻成本地」不会以丢弃配方的方式静默降级，而是直接报错。
	B2B *B2BProductRecipe
}

// AdmissionRequest 是解析单个步骤归属所需的最小事实。
// 它不携带余额、权益或账本字段；这些仍由创建事务自行读取。
type AdmissionRequest struct {
	UserID          string
	TemplateID      string
	TemplateVersion int64
	Product         entitlement.GenerationRequest
	Atom            StepAtom
	Sequence        int32
	// B2B 由服务端模板编译器填写，只对首个步骤存在；本地编译路径为 nil。
	B2B *B2BProductRecipe
	// DeferredB2B is true only for the blocked second step of a B2B
	// text-to-video plan. Its recipe has been structurally frozen but has no
	// opening frame yet, so the adapter performs the corresponding preflight
	// instead of accepting it as a complete provider request.
	DeferredB2B bool
}

// AdmissionResolver 决定新步骤冻结到哪个 provider 身份。
//
// 实现必须把「未授权」「目录未发布」表达为错误，绝不能退化成另一个 provider：
// 返回本地归属就等于把 B2B 请求静默跑成本地模拟器。
type AdmissionResolver interface {
	ResolveAdmission(context.Context, AdmissionRequest) (StepAdmission, error)
}

// BatchAdmissionResolver is an optional consistency extension. A B2B
// two-step plan must resolve both routes against one published catalog snapshot;
// callers fall back to AdmissionResolver for local and existing test adapters.
type BatchAdmissionResolver interface {
	AdmissionResolver
	ResolveAdmissions(context.Context, []AdmissionRequest) ([]StepAdmission, error)
}

// Normalize 校验一个解析结果。
//
// 新步骤不接受空归属：历史文档的四项全空由 NormalizeExecutionRoute 兼容为本地，
// 但新任务必须显式选择 provider，不能靠零值默认。
func (admission StepAdmission) Normalize() (StepAdmission, error) {
	if admission.Route == (ExecutionRoute{}) {
		return StepAdmission{}, ErrInvalidAdmission
	}
	route, err := NormalizeExecutionRoute(admission.Route)
	if err != nil {
		return StepAdmission{}, ErrInvalidAdmission
	}
	admission.Route = route
	switch route.Provider {
	case LocalExecutionProvider:
		if admission.B2B != nil {
			return StepAdmission{}, ErrInvalidAdmission
		}
		return admission, nil
	case PolarStarB2BProvider:
		if admission.B2B == nil {
			return StepAdmission{}, ErrInvalidAdmission
		}
		recipe, err := admission.B2B.Normalize()
		if err != nil {
			return StepAdmission{}, err
		}
		admission.B2B = &recipe
		return admission, nil
	default:
		return StepAdmission{}, ErrInvalidAdmission
	}
}

// Normalize 校验并复制一份公开产品配方。
//
// 它只做结构校验：公开合同的合法性（model 命名、字段与 capability 的对应、
// URL 可达性）仍由 B2B 适配器在映射时判定并保持唯一权威。
func (recipe B2BProductRecipe) Normalize() (B2BProductRecipe, error) {
	if !validAdmissionKey(recipe.ProductKey) {
		return B2BProductRecipe{}, ErrInvalidAdmission
	}
	if recipe.TemplateKey != "" && !validAdmissionKey(recipe.TemplateKey) {
		return B2BProductRecipe{}, ErrInvalidAdmission
	}
	if len(recipe.Input) == 0 || len(recipe.Input) > maxAdmissionRecipeBytes || !isJSONObject(recipe.Input) {
		return B2BProductRecipe{}, ErrInvalidAdmission
	}
	if len(recipe.Assets) > maxAdmissionAssets {
		return B2BProductRecipe{}, ErrInvalidAdmission
	}
	assets := make([]B2BAsset, 0, len(recipe.Assets))
	for _, asset := range recipe.Assets {
		if !validAdmissionAssetField(asset.Role, maxAdmissionAssetRoleLength) || !validAdmissionAssetField(asset.URL, maxAdmissionAssetURLLength) {
			return B2BProductRecipe{}, ErrInvalidAdmission
		}
		assets = append(assets, asset)
	}
	recipe.Input = append(json.RawMessage(nil), recipe.Input...)
	recipe.Assets = assets
	return recipe, nil
}

// Marshal 返回冻结配方的稳定字节表示。
// 同一份配方重复编码必须得到相同字节，否则发件箱载荷与指纹会随调用顺序漂移。
func (recipe B2BProductRecipe) Marshal() ([]byte, error) {
	normalized, err := recipe.Normalize()
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil || len(encoded) == 0 || len(encoded) > maxAdmissionRecipeBytes {
		return nil, ErrInvalidAdmission
	}
	return encoded, nil
}

// Digest 返回冻结配方的 SHA-256 摘要，用于幂等请求指纹。
// 它不是最终 wire 请求的摘要：后者由 B2B 适配器在映射后计算并单独持久化。
func (recipe B2BProductRecipe) Digest() (string, error) {
	encoded, err := recipe.Marshal()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ParseB2BProductRecipe 从冻结字节还原公开产品配方。
// 它拒绝未知字段与非法结构，避免把损坏或篡改的载荷当作可信配方使用。
func ParseB2BProductRecipe(payload []byte) (B2BProductRecipe, error) {
	var recipe B2BProductRecipe
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&recipe) != nil {
		return B2BProductRecipe{}, ErrInvalidAdmission
	}
	return recipe.Normalize()
}

// localAdmissionResolver 是未装配 B2B admission 时的唯一默认实现。
//
// 它只冻结本地执行身份，并拒绝任何携带公开产品配方的请求：那说明调用方已经按
// B2B 合同编译，而运行配置没有授权 B2B，此时继续执行就是静默降级。
type localAdmissionResolver struct{}

func (localAdmissionResolver) ResolveAdmission(_ context.Context, request AdmissionRequest) (StepAdmission, error) {
	if request.B2B != nil {
		return StepAdmission{}, ErrAdmissionConfigurationUnavailable
	}
	return StepAdmission{Route: LocalExecutionRoute()}, nil
}

// NewLocalAdmissionResolver 返回只冻结本地归属的解析器，供组合根在运行配置
// 选择本地 provider 时显式装配。它不会因为任何理由退回本地以外的 provider。
func NewLocalAdmissionResolver() AdmissionResolver {
	return localAdmissionResolver{}
}

// admissionResolverOrDefault 把 nil 解释为纯本地解析器。
// nil 是「尚未装配 B2B admission」的唯一表达，组合根必须显式依赖它。
func admissionResolverOrDefault(resolver AdmissionResolver) AdmissionResolver {
	if resolver == nil {
		return localAdmissionResolver{}
	}
	return resolver
}

// validAdmissionKey 与已发布目录的 product key 规则保持同等宽松：
// 只要求非空、无首尾空白、不含空白字符且长度有界。
func validAdmissionKey(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= maxAdmissionKeyLength
}

func validAdmissionAssetField(value string, limit int) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= limit && !strings.ContainsAny(value, "\r\n")
}

func isJSONObject(raw json.RawMessage) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(raw, &object) == nil && object != nil
}
