// Package payments 管理支付订单、支付事实和主站权益入账。
package payments

import "github.com/google/wire"

// ProviderSet 是支付模块的依赖注入入口。
// PayCores 与商店内购只提供外部支付事实，入账始终发生在本服务账本中。
var ProviderSet = wire.NewSet()
