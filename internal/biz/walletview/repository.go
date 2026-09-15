package walletview

import "context"

// Repository 是钱包只读投影的反转依赖。
// 实现层负责从自有库聚合余额、VIP、日额度和账本；领域层不感知任何存储细节。
type Repository interface {
	// FindSnapshot 缺失余额或整体钱包投影时可返回 nil, nil；用例会投影为该用户的 0 钻石快照。
	FindSnapshot(context.Context, string) (*Snapshot, error)
	// ListLedgerEntries 必须按创建时间倒序返回，并以传入的 cursor 或 skip 进行分页。
	ListLedgerEntries(context.Context, LedgerPageQuery) (*LedgerPage, error)
}
