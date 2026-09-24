package outbox

import (
	"context"
	"time"
)

// Writer 定义创作事务写入待投递事件所需的最小边界。
// 调用方传入的 context 已携带事务，写入实现不得自行开启事务。
type Writer interface {
	Enqueue(context.Context, *Event) error
}

// Repository 定义发件箱事件的原子持久化操作。
// 事务由调用方通过 context 传入，仓储不得自行开启事务。
type Repository interface {
	Writer
	Claim(context.Context, string, time.Time, time.Time) (*Event, error)
	// ClaimByType 只领取指定事件类型，避免不同工作者占用不属于自己的租约。
	ClaimByType(context.Context, string, EventType, time.Time, time.Time) (*Event, error)
	// ClaimByIDAndType 原子领取指定事件，供受控单次投递使用。
	// 实现必须在同一条件更新中同时限制 ID、类型和可领取租约，不能先领取其他事件再归还。
	ClaimByIDAndType(context.Context, string, string, EventType, time.Time, time.Time) (*Event, error)
	// RenewLease 延展一个仍未过期、仍归当前 token 所有的已领取事件。
	// 任一条件不符都必须返回 ErrLeaseConflict；它是长 I/O Worker 的取消
	// 信号，续租失败后不得继续下载、上传或发布结果。
	RenewLease(context.Context, string, string, time.Time, time.Time) error
	Requeue(context.Context, string, string, time.Time) error
	MarkDelivered(context.Context, string, string, time.Time) error
	MarkFailed(context.Context, string, string, time.Time) error
	// MarkNeedsAttention 把已领取事件转成人工关注终态。它与 MarkFailed 的区别是
	// 「事实仍然有效，只是自动路径放弃」，因此实现不得触发退款或允许发布，
	// 并且必须让该事件不再出现在任何可领取集合里。
	MarkNeedsAttention(context.Context, string, string, AttentionReason, time.Time) error
	// RedriveAttention 是唯一能让事件离开 needs_attention 的操作：它开启一个新的
	// 自动重试窗口并返回更新后的事件。
	//
	// 实现必须满足三条不变量：
	//  1. 条件更新同时限制 _id、delivery_status == needs_attention 与
	//     redrive_count == ExpectedRedriveCount；未命中一律 ErrRedriveConflict，
	//     不得先读后无脑覆盖。
	//  2. 可写字段只有 delivery_status / next_attempt_at / 窗口三字段 / updated_at，
	//     并清空 attention_reason。**payload 不得出现在更新语句里** ——
	//     provider、账号、路由与幂等键都冻结在其中，重驱不能碰。
	//  3. redrive_count 单调递增，且不得超过 MaxRedriveCount。
	RedriveAttention(context.Context, RedriveCommand) (*Event, error)
}

// NewWriter 将完整发件箱仓储收窄为创作模块所需的写入边界。
func NewWriter(repository Repository) Writer {
	return repository
}
