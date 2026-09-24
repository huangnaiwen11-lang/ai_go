package generation

import (
	"errors"

	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/integrations/polarstarb2b"
)

// B2BCallbackPath 是 PolarStar B2B 终态回调（b2b.callback.v2）的挂载路径。
// 它同时是发给平台的 callbackUrl 的路径部分，由网关按精确路径接管。
const B2BCallbackPath = "/api/v1/internal/polarstar-callback"

var (
	// ErrB2BSubmissionConfig 表示 B2B 提交分支拿不到可用配置。
	ErrB2BSubmissionConfig = errors.New("generation b2b submission configuration unavailable")
)

// B2BSubmissionConfig 是 B2B 提交分支所需的只读运行事实。
//
// 它不含任何凭据：凭据只存在于注册表解析出的 ProviderHandle 里。目录来源与
// 回调模式必须来自同一份已校验配置，避免提交时用到与冻结版本不一致的目录。
type B2BSubmissionConfig struct {
	Catalogs bizgeneration.MappingCatalogStore
	Callback polarstarb2b.CallbackConfig
}

// Ready 报告该配置是否足以承载一次 B2B 提交。
// 未配置 B2B 时为零值，调用方必须据此拒绝提交，而不是退回本地执行。
func (config B2BSubmissionConfig) Ready() bool {
	return config.Catalogs != nil && config.Callback.Mode != ""
}

// newB2BSubmissionConfig 把已校验的 B2B 配置投影成提交分支需要的形状。
// b 为 nil（未配置 B2B 账号）时返回零值且不报错：本地 profile 合法。
func newB2BSubmissionConfig(b *conf.Integrations_PolarStarB2B, catalogs bizgeneration.MappingCatalogStore) (B2BSubmissionConfig, error) {
	if b == nil {
		return B2BSubmissionConfig{}, nil
	}
	if catalogs == nil {
		// 配置了 B2B 却没有目录来源：无法把冻结的 mapping_version 还原成映射，
		// 任何提交都只能失败。在构造期拒绝，而不是等创建请求进来。
		return B2BSubmissionConfig{}, ErrB2BSubmissionConfig
	}
	switch b.GetDeliveryMode() {
	case "lookup_only":
		return B2BSubmissionConfig{Catalogs: catalogs, Callback: polarstarb2b.CallbackConfig{Mode: "lookup_only"}}, nil
	case "webhook":
		// webhook 交付要求一个平台可达的回调地址。origin 由配置校验保证是公开
		// HTTPS 源（无路径、无查询串），挂载路径来自 B2BCallbackPath；两者拼出的
		// 地址必须就是网关实际接管的那个，否则我们会在请求里声明一个自己根本
		// 没有挂载的地址——平台投递 404、重试 8 次后放弃，创作永远停在 submitted。
		//
		// 这里用适配器自己的规则再验一次：配置校验对保留域（例如 .test/.invalid）
		// 比适配器宽松，只看 origin 非空会放过一类「启动正常、提交必失败」的地址。
		origin := b.GetCallbackOrigin()
		callbackURL := origin + B2BCallbackPath
		if origin == "" || !polarstarb2b.ValidCallbackURL(callbackURL) {
			return B2BSubmissionConfig{}, ErrB2BSubmissionConfig
		}
		return B2BSubmissionConfig{Catalogs: catalogs, Callback: polarstarb2b.CallbackConfig{
			Mode: "webhook", URL: callbackURL,
		}}, nil
	default:
		return B2BSubmissionConfig{}, ErrB2BSubmissionConfig
	}
}
