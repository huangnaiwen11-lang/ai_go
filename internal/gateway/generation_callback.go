package gateway

import "net/http"

const (
	generationCallbackPathV1     = "/api/v1/internal/generation-callback"
	generationCallbackPathLegacy = "/api/internal/generation-callback"
)

// matchesGenerationCallback 只接受未经路径编码或规范化的两条内部回调路径，防止
// 相似请求绕过默认 Node 代理和既有准入边界。
func matchesGenerationCallback(r *http.Request) bool {
	return r != nil &&
		r.Method == http.MethodPost &&
		r.URL != nil &&
		!r.URL.ForceQuery &&
		r.URL.RawQuery == "" &&
		r.URL.EscapedPath() == r.URL.Path &&
		(r.URL.Path == generationCallbackPathV1 || r.URL.Path == generationCallbackPathLegacy)
}
