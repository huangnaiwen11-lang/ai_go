package generation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"ai-business-service/internal/biz/shared"
)

var (
	// ErrInvalidProviderInboxKey 表示 receipt 身份不满足持久化约束。
	ErrInvalidProviderInboxKey = errors.New("generation: invalid provider inbox key")
	// ErrInvalidProviderTerminalObservation 表示重新解析出的终态语义不完整。
	ErrInvalidProviderTerminalObservation = errors.New("generation: invalid provider terminal observation")
	// ErrProviderInboxConsumerDependenciesUnavailable 表示消费编排缺少必要依赖。
	ErrProviderInboxConsumerDependenciesUnavailable = errors.New("generation: provider inbox consumer dependencies unavailable")
	// ErrProviderInboxStepMismatch 表示投递与它声称的冻结步骤身份不一致。
	// 这与 inbox 合并冲突不同：后者是同一 delivery 键上的两份字节，这里是
	// 两份本来就不该被关联在一起的业务事实。
	ErrProviderInboxStepMismatch = errors.New("generation: provider inbox step mismatch")
)

// ProviderInboxKey 是一条已持久化投递的稳定身份。
//
// 它与 inbox 文档 ID 同源（都由 source/account/delivery 派生），因此消费侧
// 不需要知道文档 ID 的构造方式，也不会因为 ID 拼写改动而读不到记录。
type ProviderInboxKey struct {
	Source     string
	AccountRef string
	DeliveryID string
}

// Validate 复用入站身份约束，避免消费侧接受一条永远不会被 ACK 过的键。
func (key ProviderInboxKey) Validate() error {
	if key.Source != ProviderInboxSource || !providerInboxStableIdentity(key.AccountRef) ||
		!providerInboxStableIdentity(key.DeliveryID) {
		return ErrInvalidProviderInboxKey
	}
	return nil
}

// ProviderInboxConsumerStore 是 inbox consumer 侧的最小持久化边界。
//
// 它与 ProviderInboxStore（ACK 侧）刻意分开：ACK 只写入待消费事实，消费侧
// 只读回事实并推进本地生命周期。合并两套方法会让任何一个替身实现都必须
// 同时满足写入与消费两侧的语义，从而失去接口隔离。
type ProviderInboxConsumerStore interface {
	// ReadProviderInbox 按 receipt 身份读回已持久化的投递事实。
	// 未找到返回 (nil, nil)：调用方据此判定「投递不存在」而不是存储故障。
	ReadProviderInbox(context.Context, ProviderInboxKey) (*ProviderInboxRecord, error)
	// MarkProviderInboxApplied 把 pending 记录单向推进为 applied，并写入终态摘要。
	// 已 applied 的记录必须保持不变（幂等），不得被第二次消费改写摘要。
	MarkProviderInboxApplied(context.Context, ProviderInboxKey, string, time.Time) error
}

// ProviderTerminalObservation 是从**已持久化的原始投递字节**重新解析出的
// 终态语义。它只含归一化后的结论，不含原始报文、不含凭据。
//
// 重新解析而不是在 ACK 时把结论存进 inbox：ACK 路径必须极短，而结论的
// 语义一旦在写入时定型，后续就无法在不改历史数据的前提下修正解析规则。
type ProviderTerminalObservation struct {
	Capability string
	Status     ProviderTerminalStatus
	// ResultRef 只有完成态才允许非空；它是供应商给出的结果地址原文，
	// 不构成下载授权——素材发布阶段仍须独立做 host/DNS/内容校验。
	ResultRef string
}

// Validate 约束观察值：能力必须属于冻结的三种原子之一，状态必须是终态，
// 且非完成态不得携带结果地址（「失败但带结果」会让下游只能靠猜来选一个）。
func (observation ProviderTerminalObservation) Validate() error {
	if !isFrozenCapability(observation.Capability) || !validProviderTerminalStatus(observation.Status) {
		return ErrInvalidProviderTerminalObservation
	}
	if observation.Status == ProviderTerminalCompleted {
		if observation.ResultRef == "" {
			return ErrInvalidProviderTerminalObservation
		}
		return nil
	}
	if observation.ResultRef != "" {
		return ErrInvalidProviderTerminalObservation
	}
	return nil
}

// isFrozenCapability 是冻结的三原子集合，与 mapper 的 supportedCapability
// 必须一致；两处一旦漂移，消费侧就会接受一个永远不会被提交过的能力。
func isFrozenCapability(capability string) bool {
	return capability == "text_to_image" || capability == "image_edit" || capability == "image_to_video"
}

// ProviderTerminalSummaryDigest 是「结论 + 语义摘要」的规范摘要，供 callback、
// lookup 与 inbox consumer 三条路径共用。
//
// 它刻意**不含**原始报文字节：raw transport digest 参与比较会让同一结论经
// lookup 与 callback 两条路到达时被判成冲突。前导域分隔串是版本化的，改动
// 摘要口径必须换版本号，否则历史槽位的摘要会被新算法判成不一致。
func ProviderTerminalSummaryDigest(status ProviderTerminalStatus, resultRef string) string {
	sum := sha256.Sum256([]byte("b2b.terminal.v2\x00" + string(status) + "\x00" + resultRef))
	return hex.EncodeToString(sum[:])
}

// ProviderInboxConsumerUsecase 把一条已 ACK 的投递推进为步骤终态。
//
// 它是 ACK 与终态之间的唯一桥梁：没有它，投递只会停在 pending，创作永远停在
// submitted，用户预扣的钻石也永远不结算。
//
// 事务边界是「读投递 + 读冻结意图 + 写终态 + 标记已消费」整体。任何一步失败
// 都必须整体回滚，使调用方可以安全重排同一事件——半途提交会让投递被标成已
// 消费而终态并未落地。
type ProviderInboxConsumerUsecase struct {
	inbox       ProviderInboxConsumerStore
	submissions ProviderSubmissionStore
	terminals   ProviderTerminalStore
	tx          shared.TxRunner
	clock       func() time.Time
}

// NewProviderInboxConsumerUsecase 创建消费用例。
func NewProviderInboxConsumerUsecase(
	inbox ProviderInboxConsumerStore,
	submissions ProviderSubmissionStore,
	terminals ProviderTerminalStore,
	tx shared.TxRunner,
) *ProviderInboxConsumerUsecase {
	return NewProviderInboxConsumerUsecaseWithClock(inbox, submissions, terminals, tx, time.Now)
}

// NewProviderInboxConsumerUsecaseWithClock 创建使用受控时钟的消费用例。
func NewProviderInboxConsumerUsecaseWithClock(
	inbox ProviderInboxConsumerStore,
	submissions ProviderSubmissionStore,
	terminals ProviderTerminalStore,
	tx shared.TxRunner,
	clock func() time.Time,
) *ProviderInboxConsumerUsecase {
	if clock == nil {
		clock = time.Now
	}
	return &ProviderInboxConsumerUsecase{inbox: inbox, submissions: submissions, terminals: terminals, tx: tx, clock: clock}
}

// Consume 消费一条已 ACK 的投递并返回终态 CAS 结果。
//
// 返回值语义（直接决定 Worker 的重排策略）：
//   - ProviderTerminalApplied：本次写入终态并标记投递已消费；
//   - ProviderTerminalNoop：同一结论此前已落地，投递也已消费，无需重排；
//   - 非 nil error：整体回滚，调用方按退避重排同一事件。
//
// 返回 error 时不得假定投递未被消费：只有事务提交失败才回滚，调用方重排后
// 会重新读到同一份事实并按上面的幂等规则收敛。
func (usecase *ProviderInboxConsumerUsecase) Consume(ctx context.Context, key ProviderInboxKey, observation ProviderTerminalObservation) (ProviderTerminalApplyResult, error) {
	if usecase == nil || usecase.inbox == nil || usecase.submissions == nil || usecase.terminals == nil ||
		usecase.tx == nil || usecase.clock == nil {
		return "", ErrProviderInboxConsumerDependenciesUnavailable
	}
	if err := key.Validate(); err != nil {
		return "", err
	}
	if err := observation.Validate(); err != nil {
		return "", err
	}
	// 业务时刻在进入可重试事务前冻结一次：Mongo 会重试整个事务回调，
	// 在事务内取当前时间会让同一次消费的重试写出不同的 ConfirmedAt。
	at := usecase.clock().UTC()
	if at.IsZero() {
		return "", ErrProviderInboxConsumerDependenciesUnavailable
	}

	var result ProviderTerminalApplyResult
	err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		applied, applyErr := usecase.consumeInTx(txCtx, key, observation, at)
		if applyErr != nil {
			return applyErr
		}
		result = applied
		return nil
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

func (usecase *ProviderInboxConsumerUsecase) consumeInTx(ctx context.Context, key ProviderInboxKey, observation ProviderTerminalObservation, at time.Time) (ProviderTerminalApplyResult, error) {
	record, err := usecase.inbox.ReadProviderInbox(ctx, key)
	if err != nil {
		return "", err
	}
	if record == nil {
		// 工作项存在但投递不存在：说明事件与事实被拆开了（例如手工清理）。
		// 重排不会让它出现，必须显式失败而不是静默成功。
		return "", ErrInvalidProviderInboxKey
	}
	if err := record.Validate(); err != nil {
		return "", err
	}
	switch record.Status {
	case ProviderInboxApplied:
		// 已经消费过：结论已落地，摘要不应被第二次消费改写。
		return ProviderTerminalNoop, nil
	case ProviderInboxQuarantined:
		// 入库时已判定字节冲突，重排不会改变结论。
		return "", ErrProviderInboxConflict
	}

	intent, err := usecase.submissions.ReadProviderSubmission(ctx, record.StepID)
	if err != nil {
		return "", err
	}
	if intent == nil {
		return "", ErrProviderInboxStepMismatch
	}
	if err := validateInboxAgainstIntent(record, intent, observation); err != nil {
		return "", err
	}

	digest := ProviderTerminalSummaryDigest(observation.Status, observation.ResultRef)
	fact, err := ProviderTerminalFactFromObservation(intent, observation, record.JobID, record.PayloadDigest, at)
	if err != nil {
		return "", err
	}
	applyResult, err := usecase.terminals.ApplyProviderTerminal(ctx, fact)
	if err != nil {
		// 瞬时竞态（陈旧 fence/version）在这里原样上抛，让整笔事务回滚并重排。
		return "", err
	}
	if applyResult != ProviderTerminalApplied && applyResult != ProviderTerminalNoop {
		// 隔离事实不能标记为正常消费。相同终态已由其他 delivery/lookup
		// 落地时，本条合法投递仍需要在同一事务内推进为 applied。
		return applyResult, nil
	}
	if err := usecase.inbox.MarkProviderInboxApplied(ctx, key, digest, at); err != nil {
		return "", err
	}
	return applyResult, nil
}

// validateInboxAgainstIntent 交叉核对投递、冻结意图与重新解析出的结论。
//
// 三份事实来自三个不同的写入时刻（提交意图 / 回调入库 / 消费），任何不一致
// 都说明其中一份被伪造或串号。此时只能拒绝，不能挑一个信：挑错了会把一笔
// 任务的终态写到另一笔任务上，而这种错误在账本上不可逆。
func validateInboxAgainstIntent(record *ProviderInboxRecord, intent *ProviderSubmissionIntent, observation ProviderTerminalObservation) error {
	// 冻结请求的内部一致性由领域再验一次，不依赖适配器替我们验过：适配器实现
	// 可能有多个（Mongo、内存替身、未来的其他存储），把完整性寄托在它们身上
	// 等于让每新增一个实现就多一次漏检机会。
	if intent.Request.Validate() != nil {
		return ErrProviderInboxStepMismatch
	}
	if intent.Request.StepID != record.StepID ||
		intent.Request.Capability != observation.Capability ||
		intent.Request.Route.AccountRef != record.AccountRef {
		return ErrProviderInboxStepMismatch
	}
	if !isFrozenCapability(intent.Request.Capability) || intent.Fence <= 0 {
		return ErrProviderInboxStepMismatch
	}
	return nil
}
