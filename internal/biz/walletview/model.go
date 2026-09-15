// Package walletview 定义用户本人可读取的钱包投影。
//
// 本包只承载读取模型和查询用例：不创建任务、不预扣钻石、不调用支付，也不依赖 Mongo 或 HTTP。
package walletview

import "time"

// VIP 表示读取时刻的 VIP 有效性及到期时间。
type VIP struct {
	Active    bool
	ExpiresAt time.Time
}

// DailyQuota 是用户固定时区内某个本地日期的单类日额度投影。
// Remaining 由持久化读取层提供，不能在查询层按 limit 和 used 重新推算，
// 以避免读取与并发占用之间出现不一致。
type DailyQuota struct {
	Limit     int32
	Used      int32
	Remaining int32
	LocalDate string
}

// Snapshot 是钱包页需要的只读事实快照。
// Timezone 必须是用户首次绑定时固化的 IANA 时区，不使用部署机器本地时区。
type Snapshot struct {
	UserID         string
	DiamondBalance int64
	VIP            VIP
	Timezone       string
	DailyImage     DailyQuota
	DailyVideo     DailyQuota
}

// LedgerEntry 是展示用的不可变账本分录。
// 这里的 ID 是账本分录标识；CreationID 仅用于关联同一创作任务。
type LedgerEntry struct {
	ID            string
	CreationID    string
	DeltaDiamonds int64
	Reason        string
	CreatedAt     time.Time
}

// LedgerPageQuery 定义账本倒序分页条件。
// Cursor 非空时优先级高于 Skip，Usecase 会在调用仓储前清除 Skip。
type LedgerPageQuery struct {
	UserID string
	Limit  int
	Skip   int
	Cursor string
}

// LedgerPage 是账本倒序分页的结果。
type LedgerPage struct {
	Entries    []LedgerEntry
	NextCursor string
}
