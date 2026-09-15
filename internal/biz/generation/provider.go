// Package generation 管理产品任务、技术步骤与父子编排。
package generation

import "github.com/google/wire"

// ProviderSet 是生成编排模块的依赖注入入口。
// 它只编排三个中台原子，不把模板产品名称泄漏给生成中台。
var ProviderSet = wire.NewSet(NewCallbackUsecase)
