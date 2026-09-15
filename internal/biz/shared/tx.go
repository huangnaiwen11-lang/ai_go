package shared

import "context"

// TxRunner 是业务模块发起原子业务写入的唯一事务入口。
// 回调只接收 context，不能取得 MongoDB Client 或 Collection，从边界上避免业务层
// 直接读写存储实现。回调可能因数据库瞬时冲突被重试，因此必须保持幂等。
type TxRunner interface {
	WithinTx(context.Context, func(context.Context) error) error
}
