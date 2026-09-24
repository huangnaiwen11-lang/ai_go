package model

import "time"

// ProviderInboxDocument 保存已验签的 PolarStar B2B delivery 原文及其规范化摘要。
// 它与旧 callback_receipts 完全隔离，delivery 去重范围为 source/account/delivery。
type ProviderInboxDocument struct {
	ID             string    `bson:"_id"`
	Source         string    `bson:"source"`
	AccountRef     string    `bson:"account_ref"`
	DeliveryID     string    `bson:"delivery_id"`
	StepID         string    `bson:"step_id"`
	JobID          string    `bson:"job_id"`
	TerminalDigest string    `bson:"terminal_digest,omitempty"`
	PayloadDigest  string    `bson:"payload_digest"`
	Payload        []byte    `bson:"payload"`
	Status         string    `bson:"status"`
	Attempts       int       `bson:"attempts"`
	CreatedAt      time.Time `bson:"created_at"`
	UpdatedAt      time.Time `bson:"updated_at"`
}

// ProviderSubmissionIntentDocument is embedded in creation_steps. It is never
// removed to grant another POST; no supplier credential is persisted here.
//
// 唯一的例外是 Reauthorization：当对账**证明**平台从未受理这笔任务时
// （按确定派生的幂等键查询得到明确 404），它记录一次有审计的重发许可。
// 写入条件带 $exists:false，因此至多存在一份，字段本身即「额度已用完」的判据。
type ProviderSubmissionIntentDocument struct {
	Payload        []byte    `bson:"payload"`
	Digest         string    `bson:"digest"`
	IdempotencyKey string    `bson:"idempotency_key"`
	PreparedAt     time.Time `bson:"prepared_at"`
	Fence          int32     `bson:"fence"`
	// Reauthorization 非空即代表重新授权额度已经用完。
	Reauthorization *ProviderReauthorizationDocument `bson:"reauthorization,omitempty"`
}

// ProviderReauthorizationDocument 记录唯一一次「重新授权提交」的审计事实。
//
// 刻意不存「等待了多久」：它由 prepared_at 与 at 两个已持久化的事实相减得到，
// 再存一份副本只会多出一个可能漂移的数字。
type ProviderReauthorizationDocument struct {
	At     time.Time `bson:"at"`
	Reason string    `bson:"reason"`
	// Fence 是授权时的尝试计数，便于与发件箱记录对齐。
	Fence int32 `bson:"fence"`
}
