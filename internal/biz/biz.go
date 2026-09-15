// Package biz 汇总业务模块的依赖注入入口。
package biz

import (
	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"

	"github.com/google/wire"
)

// ProviderSet 装配已具备本地领域实现的业务模块。
// 身份模块不注册传输适配器，因此纳入此集合不会新增 HTTP 或 gRPC 业务路由。
var ProviderSet = wire.NewSet(NewModuleRegistry, identity.ProviderSet, catalog.ProviderSet, entitlement.ProviderSet, ledger.ProviderSet, generation.ProviderSet, outbox.NewWriter, creations.ProviderSet)
