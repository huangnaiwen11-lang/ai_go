package model

import (
	"time"
)

// OutboxEventDocument 表示发件箱事件持久化对象。
type OutboxEventDocument struct {
	ID             string    `bson:"_id"`
	AggregateID    string    `bson:"aggregate_id"`
	EventType      string    `bson:"event_type"`
	Payload        []byte    `bson:"payload"`
	DeliveryStatus string    `bson:"delivery_status"`
	AttemptCount   int32     `bson:"attempt_count"`
	NextAttemptAt  time.Time `bson:"next_attempt_at"`
	LeaseToken     string    `bson:"lease_token,omitempty"`
	LeaseUntil     time.Time `bson:"lease_until,omitempty"`
	LeaseOwner     string    `bson:"lease_owner,omitempty"`
	JobID          string    `bson:"job_id,omitempty"`
	LastError      string    `bson:"last_error,omitempty"`
	// AttentionReason 记录事件转入人工关注的固定安全原因，只在 delivery_status
	// 为 needs_attention 时写入。与 last_error 分开：Requeue 每次都会覆盖
	// last_error，关注原因必须能在重新入队之后存活。
	AttentionReason string `bson:"attention_reason,omitempty"`
	// RedriveStartedAt 与 RedriveAttemptBase 记录人工重驱开启的自动重试窗口，
	// RedriveCount 单调递增。三者都只被 RedriveAttention 改写；AttemptCount 与
	// CreatedAt 保持各自的事实语义，不因重驱而改变。
	RedriveStartedAt   time.Time `bson:"redrive_started_at,omitempty"`
	RedriveAttemptBase int32     `bson:"redrive_attempt_base,omitempty"`
	RedriveCount       int32     `bson:"redrive_count,omitempty"`
	// TerminalVersion 记录「该事件被哪个终态版本终结」。B2B 对账事件被终态
	// 写入同事务终结时填 1；旧 generation.submission 事件不写该字段。
	TerminalVersion int64     `bson:"terminal_version,omitempty"`
	CreatedAt       time.Time `bson:"created_at"`
	UpdatedAt       time.Time `bson:"updated_at"`
}
