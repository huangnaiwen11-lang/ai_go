package gateway

import (
	"net/http"
	"strings"
)

const (
	worksListPath   = "/api/works"
	worksDetailPath = "/api/works/:id"
)

// worksRoutes 是作品历史已审查的最小 GET 路由集。
// 详情使用稳定占位 route key，而不是把任意作品 ID 写入路由开关文件。
var worksRoutes = [...]exactRouteKey{
	{method: http.MethodGet, path: worksListPath},
	{method: http.MethodGet, path: worksDetailPath},
}

// matchWorksRoute 严格匹配列表或单段详情路径。列表允许受控分页 query；详情不接受 query，
// 从而不会以模糊前缀意外接管旧 Node 的其他作品子资源。
func matchWorksRoute(request *http.Request) (exactRouteKey, bool) {
	if request == nil || request.URL == nil || request.Method != http.MethodGet || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path {
		return exactRouteKey{}, false
	}
	if request.URL.Path == worksListPath {
		return worksRoutes[0], true
	}
	if request.URL.RawQuery != "" || !strings.HasPrefix(request.URL.Path, worksListPath+"/") {
		return exactRouteKey{}, false
	}
	id := strings.TrimPrefix(request.URL.Path, worksListPath+"/")
	if id == "" || strings.Contains(id, "/") {
		return exactRouteKey{}, false
	}
	return worksRoutes[1], true
}
