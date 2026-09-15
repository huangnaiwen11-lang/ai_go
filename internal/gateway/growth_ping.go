package gateway

import "net/http"

const (
	growthPingPath     = "/api/growth/ping"
	growthPingResponse = "{\"success\":true,\"data\":{\"ok\":true}}"
)

// serveGrowthPingCanary 输出与 Node 兼容且无副作用的响应。它不拥有业务状态，
// 因而网关可以提供此可用性检查，不把数据库或领域服务依赖带入迁移边界。
func serveGrowthPingCanary(response http.ResponseWriter, _ *http.Request) {
	if response.Header().Get("Content-Type") == "" {
		response.Header().Set("Content-Type", "application/json")
	}
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(growthPingResponse))
}
