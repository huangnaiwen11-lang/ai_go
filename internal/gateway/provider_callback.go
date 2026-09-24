package gateway

import "net/http"

// providerCallbackPath 是 PolarStar B2B 终态回调（b2b.callback.v2）的唯一挂载路径。
//
// 它与 generationCallbackPathV1（execution.callback.v2，面向自有生成中台）是两条
// 不同合同的入口：签名域、去重键与投递重试策略都不同，因此不能共用一条路径或
// 一个处理器。这里与 internal/integrations/generation 的 B2BCallbackPath 保持
// 字面一致；两处都有各自的门禁测试钉住它。
const providerCallbackPath = "/api/v1/internal/polarstar-callback"

// matchesProviderCallback 只接受未经路径编码、规范化或附加查询的那一条回调路径。
// 与生成回调一样，它不接受路径别名：别名会让同一份投递在两个不同的 URL 上被
// 去重键之外的东西区分开，而投递去重只能依赖 X-Delivery-Id。
func matchesProviderCallback(r *http.Request) bool {
	return r != nil &&
		r.Method == http.MethodPost &&
		r.URL != nil &&
		!r.URL.ForceQuery &&
		r.URL.RawQuery == "" &&
		r.URL.EscapedPath() == r.URL.Path &&
		r.URL.Path == providerCallbackPath
}
