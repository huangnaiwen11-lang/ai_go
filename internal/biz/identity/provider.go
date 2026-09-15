// Package identity 管理用户、游客绑定、会话与账号状态。
package identity

import "github.com/google/wire"

// ProviderSet 是身份模块的依赖注入入口。
// 用例只依赖本包声明的仓储接口，由 data 层在应用装配时提供实现。
var ProviderSet = wire.NewSet(NewUsecase)
