// Package ledger 管理主站自有账本、预扣、确认、冲正与没收。
package ledger

import "github.com/google/wire"

// ProviderSet 是账本模块的依赖注入入口。
// 账本只记录 Go 主站拥有的资金事实，不读写现网 Node 钱包。
var ProviderSet = wire.NewSet(NewUsecase)
