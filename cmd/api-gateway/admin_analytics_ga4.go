package main

import (
	"log/slog"

	bizga4 "ai-business-service/internal/biz/adminanalyticsga4"
	integrationsga4 "ai-business-service/internal/integrations/ga4"
)

// newGa4Reporter 装配 GA4 出站客户端。
//
// 未配置、或凭据存在但不可解析时返回 nil，而不是让整个 Gateway 启动失败：
// GA4 是后台的分析外围能力，缺它不该阻断业务网关。
// 返回 nil 的后果是精确且可观测的 —— /status 仍然能回答「配没配」，
// 报表则返回 503 GA4_CLIENT_UNAVAILABLE，绝不会伪装成一个全空仪表盘。
//
// 返回值故意声明成接口类型：只有非类型化的 nil 才能让下游的 `reporter == nil`
// 判定成立，`(*ga4.Client)(nil)` 塞进接口会让那一步失效。
func newGa4Reporter(environment integrationsga4.Environment) bizga4.Reporter {
	if !environment.Configured() {
		return nil
	}
	credentials, err := integrationsga4.LoadCredentials(environment.CredentialSource)
	if err != nil {
		slog.Warn("GA4 credential could not be loaded; reports will answer GA4_CLIENT_UNAVAILABLE", "error", err)
		return nil
	}
	client, err := integrationsga4.New(integrationsga4.Options{
		PropertyID:  environment.PropertyID,
		Credentials: credentials,
	})
	if err != nil {
		slog.Warn("GA4 client could not be built; reports will answer GA4_CLIENT_UNAVAILABLE", "error", err)
		return nil
	}
	return client
}
