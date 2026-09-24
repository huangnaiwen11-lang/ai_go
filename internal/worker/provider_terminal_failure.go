package worker

import (
	"errors"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

// providerTerminalPermanentFailure 判定「重排不会改变结论」的失败。
//
// 回调消费与对账查询共用同一份判定：两条路径最终都会撞上同一批完整性冲突，
// 各写一份白名单必然漂移，而漂移的代价是「一条路径重排到天荒地老、另一条
// 路径把可恢复的抖动当永久失败放弃」。
//
// 只认那些表示**磁盘上的事实互相矛盾**的错误：
//   - 投递不存在 / 记录损坏 / 投递已被隔离 / 投递与冻结意图身份不符；
//   - 已持久化字节或查询响应不再满足合同；
//   - 冻结提交意图缺失或本身非法；
//   - 组装出的终态事实本身非法。
//
// 这些状态再读一万次也是同一个结论。刻意**不含**任何存储或事务错误（含
// Mongo 写冲突、陈旧 fence/version、传输失败、404）：那些是瞬时状态，重排
// 即可收敛。把瞬时故障误判为永久会让一笔本可收敛的任务被永久放弃，而反过来
// 只是多轮几次退避——代价不对称，所以判定一律往「重试」一侧偏。
func providerTerminalPermanentFailure(err error) bool {
	return errors.Is(err, generation.ErrInvalidProviderInboxKey) ||
		errors.Is(err, generation.ErrInvalidProviderInboxRecord) ||
		errors.Is(err, generation.ErrProviderInboxConflict) ||
		errors.Is(err, generation.ErrProviderInboxStepMismatch) ||
		errors.Is(err, generation.ErrInvalidProviderTerminalObservation) ||
		errors.Is(err, generation.ErrInvalidProviderTerminal) ||
		errors.Is(err, generation.ErrSubmissionConflict) ||
		errors.Is(err, generation.ErrInvalidSubmissionCommand) ||
		errors.Is(err, polarstarb2b.ErrInvalidCallback) ||
		errors.Is(err, polarstarb2b.ErrInvalidRequest)
}
