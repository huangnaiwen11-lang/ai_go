package worker

import (
	"context"
	"errors"
	"strings"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const defaultReconcileWorkerID = "local-provider-reconcile-worker"

var (
	// ErrReconcileWorkerDependenciesUnavailable 表示工作者未被完整装配。
	ErrReconcileWorkerDependenciesUnavailable = errors.New("provider reconcile worker dependencies are unavailable")
	// ErrInvalidReconcileEvent 表示对账工作项的标识无法还原出步骤身份。
	ErrInvalidReconcileEvent = errors.New("provider reconcile worker: invalid reconcile event")
)

// ProviderLookupParser 把一次状态查询归一化成统一终态观察值。
// 生产实现是 platform.B2BObservationParser。
type ProviderLookupParser interface {
	ParseLookup(*generation.ProviderSubmissionIntent, polarstarb2b.Job) (platform.B2BLookupOutcome, error)
}

// ProviderReconcileWorker 按冻结的幂等键核对一笔可能已经发出的 B2B 任务。
//
// 它是「终态只能靠投递到达」这一假设的唯一破局点：平台的有界 webhook 投递会
// 在 8 次后放弃，本地崩溃窗口也会整段吞掉投递。没有它，那些任务的创作永远停在
// submitted，用户预扣的钻石也永远不结算。
//
// 它绝不重新 POST，也绝不因为查不到就冲正：两者都会把「结果未知」当成「没有
// 结果」，而用户的钻石已经被预扣。
type ProviderReconcileWorker struct {
	outbox      outbox.Repository
	submissions generation.ProviderSubmissionStore
	terminals   generation.ProviderTerminalStore
	tx          shared.TxRunner
	providers   submissionRouter
	parser      ProviderLookupParser
	workerID    string
	now         func() time.Time
}

// NewProviderReconcileWorker 构造一次性对账工作者。
// 该构造器不领取事件、不写库、也不发起任何网络请求。
func NewProviderReconcileWorker(
	outboxRepository outbox.Repository,
	submissions generation.ProviderSubmissionStore,
	terminals generation.ProviderTerminalStore,
	tx shared.TxRunner,
	providers submissionRouter,
	parser ProviderLookupParser,
	workerID string,
	now func() time.Time,
) *ProviderReconcileWorker {
	if now == nil {
		now = time.Now
	}
	return &ProviderReconcileWorker{
		outbox: outboxRepository, submissions: submissions, terminals: terminals, tx: tx,
		providers: providers, parser: parser, workerID: workerID, now: now,
	}
}

// NewDefaultProviderReconcileWorker 提供组合根使用的默认装配。
//
// 形参取具体注册表类型而不是 submissionRouter 接口：Wire 不做结构化接口匹配，
// 声明接口会让组合图找不到提供者。
func NewDefaultProviderReconcileWorker(
	outboxRepository outbox.Repository,
	submissions generation.ProviderSubmissionStore,
	terminals generation.ProviderTerminalStore,
	tx shared.TxRunner,
	registry *platform.ProviderRegistry,
	parser ProviderLookupParser,
) *ProviderReconcileWorker {
	return NewProviderReconcileWorker(outboxRepository, submissions, terminals, tx, registry, parser, defaultReconcileWorkerID, time.Now)
}

// DeliverOnce 领取一个对账工作项并核对一次。
//
// 结案语义与投递消费一致：结论已落地（含幂等重放）算投递完成；供应商仍在处理、
// 外部任务号尚未绑定、传输失败或存储抖动都按退避重排；只有「事实互相矛盾」才
// 以失败结案。
func (worker *ProviderReconcileWorker) DeliverOnce(ctx context.Context, expectedEventID string) error {
	if err := worker.ready(); err != nil {
		return err
	}
	now := worker.nowUTC()
	eventType := outbox.EventType(generation.ProviderReconcileEventType)
	var event *outbox.Event
	var err error
	if expectedEventID == "" {
		event, err = worker.outbox.ClaimByType(ctx, worker.workerID, eventType, now, now.Add(defaultLeaseDuration))
	} else {
		event, err = worker.outbox.ClaimByIDAndType(ctx, worker.workerID, expectedEventID, eventType, now, now.Add(defaultLeaseDuration))
	}
	if err != nil || event == nil {
		return err
	}
	if event.EventType != eventType || (expectedEventID != "" && event.ID != expectedEventID) {
		return ErrUnexpectedClaimedEvent
	}
	stepID, err := reconcileStepID(event.ID)
	if err != nil {
		return worker.abandon(ctx, event)
	}
	intent, err := worker.submissions.ReadProviderSubmission(ctx, stepID)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	if intent == nil {
		return worker.abandon(ctx, event)
	}
	if intent.ExternalExecutionID == "" {
		// 授权已发出但外部任务号还没绑定：绑定与收敛由提交路径负责（它持有提交
		// 事件租约）。这里既不能按任务号核对，也不能冲正，只能重排等待。
		return worker.requeue(ctx, event, nil)
	}
	handle, err := worker.providers.ProviderForRoute(intent.Request.Route.Provider, intent.Request.Route.AccountRef)
	if err != nil {
		// 运行配置缺失或账号不符：这是可恢复的部署状态，不是「这笔任务做不了」。
		// 按永久失败结案会让一次配置回滚永久丢掉一笔在途任务。
		return worker.requeue(ctx, event, nil)
	}
	if !handle.IsB2B() || handle.B2B == nil {
		return worker.abandon(ctx, event)
	}
	frozen, err := polarstarb2b.RestoreRequest(b2bRoute(intent.Request.Route, intent.Request.StepID), intent.Request.Payload, intent.Request.Digest)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	job, err := handle.B2B.Lookup(ctx, polarstarb2b.LookupKey{
		AccountRef: handle.AccountRef, IdempotencyKey: frozen.IdempotencyKey(),
		ExternalID: frozen.ExternalID(), Capability: frozen.Capability(),
		// 带上已绑定的外部任务号：客户端会在解码时核对响应自报的任务号与它一致，
		// 从而挡住「查询落到另一笔任务上」。
		JobID: intent.ExternalExecutionID,
	})
	if err != nil {
		if errors.Is(err, polarstarb2b.ErrNotSent) {
			// 查询请求根本没发出去：本地构造的查询不合法，重排不会让它变合法。
			return worker.abandon(ctx, event)
		}
		// 未找到（404）与传输失败都无法证明中台没有这笔任务。按退避继续对账，
		// 既不重发也不冲正；平台给出 Retry-After 时以它为准。
		return worker.requeue(ctx, event, err)
	}
	outcome, err := worker.parser.ParseLookup(intent, job)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	if !outcome.Terminal {
		// 供应商仍在处理：等下一次对账，绝不把中间态写成终态。
		return worker.requeue(ctx, event, nil)
	}
	fact, err := generation.ProviderTerminalFactFromObservation(intent, outcome.Observation, intent.ExternalExecutionID, outcome.ResponseDigest, worker.nowUTC())
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	result, err := worker.applyTerminal(ctx, fact)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	if result == generation.ProviderTerminalQuarantined {
		// 冲突证据已经由仓储写入，重排不会改变结论。
		return worker.abandon(ctx, event)
	}
	// 终态 CAS 已在同一事务里把本步骤的对账事件标记为已投递并清掉租约；
	// 这里的结案因此多半会撞上租约冲突，由 settleOutboxError 视作正常交接。
	return worker.deliver(ctx, event)
}

func (worker *ProviderReconcileWorker) applyTerminal(ctx context.Context, fact generation.ProviderTerminalFact) (generation.ProviderTerminalApplyResult, error) {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	var result generation.ProviderTerminalApplyResult
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		applied, applyErr := worker.terminals.ApplyProviderTerminal(txCtx, fact)
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

func (worker *ProviderReconcileWorker) retryOrAbandon(ctx context.Context, event *outbox.Event, cause error) error {
	if providerTerminalPermanentFailure(cause) {
		return worker.abandon(ctx, event)
	}
	return worker.requeue(ctx, event, cause)
}

func (worker *ProviderReconcileWorker) deliver(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkDelivered(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderReconcileWorker) abandon(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkFailed(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderReconcileWorker) requeue(ctx context.Context, event *outbox.Event, cause error) error {
	now := worker.nowUTC()
	return settleOutboxError(worker.outbox.Requeue(ctx, event.ID, event.LeaseToken, nextSubmissionAttempt(now, event.AttemptCount, cause)))
}

func (worker *ProviderReconcileWorker) ready() error {
	if worker == nil || worker.outbox == nil || worker.submissions == nil || worker.terminals == nil ||
		worker.tx == nil || worker.providers == nil || worker.parser == nil || worker.workerID == "" {
		return ErrReconcileWorkerDependenciesUnavailable
	}
	return nil
}

// persistenceContext 与提交工作者同契约：保留调用链 values（例如 trace），
// 移除已经结束的外部请求取消信号，并以短超时约束数据库状态收敛。
func (worker *ProviderReconcileWorker) persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), submissionPersistenceTimeout)
}

func (worker *ProviderReconcileWorker) nowUTC() time.Time {
	if worker.now == nil {
		return time.Now().UTC()
	}
	return worker.now().UTC()
}

// reconcileStepID 从工作项标识还原步骤身份。
//
// 事件载荷有两种可能的生产者（提交意图与投递入库），字段布局不同，因此步骤
// 身份只从标识派生。往返校验保证后缀就是一个能被重新派生出同一标识的步骤 ID，
// 而不是任意字符串。
func reconcileStepID(eventID string) (string, error) {
	prefix := generation.ProviderReconcileEventType + ":"
	if !strings.HasPrefix(eventID, prefix) {
		return "", ErrInvalidReconcileEvent
	}
	stepID := strings.TrimPrefix(eventID, prefix)
	if generation.ProviderInboxRecoveryEventID(stepID) != eventID {
		return "", ErrInvalidReconcileEvent
	}
	return stepID, nil
}

var _ onceDeliverer = (*ProviderReconcileWorker)(nil)
