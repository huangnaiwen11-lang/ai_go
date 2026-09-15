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
	Requeue(context.Context, string, string, time.Time) error
	MarkDelivered(context.Context, string, string, time.Time) error
	MarkFailed(context.Context, string, string, time.Time) error
}

// NewWriter 将完整发件箱仓储收窄为创作模块所需的写入边界。
func NewWriter(repository Repository) Writer {
	return repository
}
