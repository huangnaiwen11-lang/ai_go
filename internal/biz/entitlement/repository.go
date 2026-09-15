package entitlement

import "context"

// SubscriptionReader 读取服务端自有订阅投影的不可变权益快照。
// 缺失投影返回 nil，nil，调用方按普通用户处理，绝不从用户命令接收订阅事实。
type SubscriptionReader interface {
	FindByUserID(context.Context, string) (*SubscriptionSnapshot, error)
}
