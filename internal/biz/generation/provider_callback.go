package generation

import (
	"context"
	"errors"
	"time"

	"ai-business-service/internal/biz/shared"
)

// ErrProviderCallbackDependenciesUnavailable 表示回调落库编排缺少必要依赖。
var ErrProviderCallbackDependenciesUnavailable = errors.New("generation: provider callback dependencies unavailable")

// ErrInvalidProviderDelivery 表示已验签的投递事实本身不完整。
// 它与 inbox 合并冲突（ErrProviderInboxConflict）是两回事：前者是调用方给了
// 一条无法持久化的事实，后者是本地已有另一份同键事实。
var ErrInvalidProviderDelivery = errors.New("generation: invalid provider delivery")

// ProviderDeliveryFact 是一条已验签的 b2b.callback.v2 投递事实。
//
// 它只承载「哪次投递、属于哪个冻结步骤、原始字节是什么」：终态语义、结果素材
// 与账本动作都不在这里——那些属于 inbox consumer，而不是 ACK 路径。
type ProviderDeliveryFact struct {
	AccountRef string
	DeliveryID string
	StepID     string
	JobID      string
	Attempt    int
	Payload    []byte
}

// ProviderCallbackUsecase 把一条已验签的 B2B 投递持久化进 inbox，并在同一事务
// 内唤醒「消费该投递」与「该步骤对账」两条恢复工作项。
//
// 这里刻意不做终态 CAS。平台要求 webhook 在 8 秒内返回 2xx，而终态 CAS 会因为
// 并发竞争进入重试；把它放进 ACK 路径，一次写冲突就会变成平台侧的一次重投，
// 而我们本来已经可以用 inbox 里的字节自己重放。切分点因此是：
// **ACK 只承诺「这条投递已被持久接受」，不承诺「终态已生效」**。
type ProviderCallbackUsecase struct {
	inbox ProviderInboxStore
	tx    shared.TxRunner
	clock func() time.Time
}

// NewProviderCallbackUsecase 创建回调落库用例。
func NewProviderCallbackUsecase(inbox ProviderInboxStore, tx shared.TxRunner) *ProviderCallbackUsecase {
	return NewProviderCallbackUsecaseWithClock(inbox, tx, time.Now)
}

// NewProviderCallbackUsecaseWithClock 创建使用受控时钟的回调落库用例。
func NewProviderCallbackUsecaseWithClock(inbox ProviderInboxStore, tx shared.TxRunner, clock func() time.Time) *ProviderCallbackUsecase {
	if clock == nil {
		clock = time.Now
	}
	return &ProviderCallbackUsecase{inbox: inbox, tx: tx, clock: clock}
}

// Handle 落库一条投递并返回合并结果。
//
// 业务时刻在进入可重试事务前冻结一次：Mongo 会重试整个事务回调，若在事务内取
// 当前时间，同一条投递的重试会写出不同的 CreatedAt，而 CreatedAt 是「首次见到
// 这条投递」的唯一证据。
func (usecase *ProviderCallbackUsecase) Handle(ctx context.Context, fact ProviderDeliveryFact) (ProviderInboxApplyResult, error) {
	if usecase == nil || usecase.inbox == nil || usecase.tx == nil || usecase.clock == nil {
		return "", ErrProviderCallbackDependenciesUnavailable
	}
	at := usecase.clock().UTC()
	if at.IsZero() {
		return "", ErrProviderCallbackDependenciesUnavailable
	}
	record, err := NewProviderInboxRecord(fact.AccountRef, fact.DeliveryID, fact.StepID, fact.JobID, "", fact.Payload, fact.Attempt, at)
	if err != nil {
		// inbox 记录的结构约束就是入站合同的一部分；在这里失败说明调用方
		// 把未校验的投递送进来了，不能退化成「稍后重试」。
		return "", ErrInvalidProviderDelivery
	}
	var result ProviderInboxApplyResult
	err = usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		applied, applyErr := usecase.inbox.ApplyAndSchedule(txCtx, record)
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
