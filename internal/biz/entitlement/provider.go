// Package entitlement 管理价格、VIP、每日额度和用户时区。
package entitlement

import "github.com/google/wire"

// ProviderSet 是权益模块的依赖注入入口。
// 免费额度与钻石余额必须通过原子预留协作，不能被其他模块绕过。
var ProviderSet = wire.NewSet(NewUsecase)
