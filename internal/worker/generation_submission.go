// Package worker 只装配可显式调用的一次性后台工作单元，不在此包启动循环或注册传输路由。
package worker

import (
	"context"
	"errors"
	"time"

	"ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	platform "ai-business-service/internal/integrations/generation"

	"github.com/google/wire"
)

const (
	defaultLeaseDuration = time.Minute
	defaultRetryBackoff  = 30 * time.Second
	defaultWorkerID      = "local-generation-submission-worker"
	// submissionPersistenceTimeout 限制外部调用结束后的状态收敛时间，避免写入无限阻塞。
	submissionPersistenceTimeout = 5 * time.Second
)

var (
	// ErrWorkerDependenciesUnavailable 表示工作者未被完整装配，不能安全处理任何事件。
	ErrWorkerDependenciesUnavailable = errors.New("generation submission worker dependencies are unavailable")
	// ErrUnexpectedClaimedEvent 表示领取到的事件不属于本次显式调用请求的稳定事件标识。
	ErrUnexpectedClaimedEvent = errors.New("generation submission worker claimed unexpected event")
)

// submissionClient 是工作者调用生成中台所需的最小技术边界。
// 它只接受 execution.v2 技术请求，不包含用户、权益、账本或支付字段。
type submissionClient interface {
	Submit(context.Context, platform.Execution) (platform.SubmissionResult, error)
	Lookup(context.Context, string) (platform.LookupResult, error)
}

// reservationSettler 是工作者收敛明确失败或审核没收的最小账本边界。
// 账本模块仍独占实际余额、日免和分录的读写。
type reservationSettler interface {
	ReverseInTx(context.Context, string, ledger.ReversalReason, time.Time) (*ledger.Reservation, error)
	ConfiscateInTx(context.Context, string, string, time.Time) (*ledger.Reservation, error)
}

// settledSubmissionReader 是冲突后可选的既有事实读取边界。
// 读取只用于确认其他工作者或未来回调已经收敛；它绝不触发再次冲正。
type settledSubmissionReader interface {
	ExistingSubmission(context.Context, string) error
}

// GenerationSubmissionWorker 负责一次领取、提交或对账，不持有轮询循环。
type GenerationSubmissionWorker struct {
	outbox     outbox.Repository
	submission generation.SubmissionStore
	settler    reservationSettler
	tx         shared.TxRunner
	client     submissionClient
	reviewer   contentreview.Reviewer
	workerID   string
	now        func() time.Time
}

// NewGenerationSubmissionWorker 为测试或受控装配构造一次性提交工作者。
// 该构造器不会领取事件、写库或发起任何 HTTP 请求。
func NewGenerationSubmissionWorker(
	outboxRepository outbox.Repository,
	submissionStore generation.SubmissionStore,
	settler reservationSettler,
	tx shared.TxRunner,
	client submissionClient,
	reviewer contentreview.Reviewer,
	workerID string,
	now func() time.Time,
) *GenerationSubmissionWorker {
	if now == nil {
		now = time.Now
	}
	return &GenerationSubmissionWorker{
		outbox: outboxRepository, submission: submissionStore, settler: settler,
		tx: tx, client: client, reviewer: reviewer, workerID: workerID, now: now,
	}
}

// NewDefaultGenerationSubmissionWorker 提供仅用于 Wire 图校验的本地默认装配。
// main 不会调用 DeliverOnce，因此不会启动后台循环或产生网络请求。
func NewDefaultGenerationSubmissionWorker(
	outboxRepository outbox.Repository,
	submissionStore generation.SubmissionStore,
	ledgerUsecase *ledger.Usecase,
	tx shared.TxRunner,
	client *platform.Client,
	reviewer contentreview.Reviewer,
) *GenerationSubmissionWorker {
	return NewGenerationSubmissionWorker(outboxRepository, submissionStore, ledgerUsecase, tx, client, reviewer, defaultWorkerID, time.Now)
}

// DeliverOnce 领取一个事件并处理一次首次生成提交。
// expectedEventID 仅用于调用方确认领取结果，空字符串表示接受任一可领取生成事件。
func (worker *GenerationSubmissionWorker) DeliverOnce(ctx context.Context, expectedEventID string) error {
	if err := worker.ready(); err != nil {
		return err
	}
	now := worker.nowUTC()
	var event *outbox.Event
	var err error
	if expectedEventID == "" {
		event, err = worker.outbox.ClaimByType(ctx, worker.workerID, outbox.EventTypeGenerationSubmission, now, now.Add(defaultLeaseDuration))
	} else {
		// 指定投递必须在数据库条件更新中锁定目标 ID，禁止先租约其他事件再归还。
		event, err = worker.outbox.ClaimByIDAndType(ctx, worker.workerID, expectedEventID, outbox.EventTypeGenerationSubmission, now, now.Add(defaultLeaseDuration))
	}
	if err != nil || event == nil {
		return err
	}
	if event.EventType != outbox.EventTypeGenerationSubmission {
		return ErrUnexpectedClaimedEvent
	}
	if expectedEventID != "" && event.ID != expectedEventID {
		return ErrUnexpectedClaimedEvent
	}

	record, err := worker.submission.ClaimedSubmission(ctx, event.ID)
	if err != nil {
		return worker.resolveConflict(ctx, event.ID, err)
	}
	if record == nil || record.LeaseToken != event.LeaseToken || record.EventID != event.ID {
		return worker.resolveConflict(ctx, event.ID, generation.ErrSubmissionConflict)
	}

	if event.DeliveryStatus != outbox.DeliveryStatusDispatching && event.DeliveryStatus != outbox.DeliveryStatusReconciling {
		return worker.resolveConflict(ctx, event.ID, generation.ErrSubmissionConflict)
	}
	if event.DeliveryStatus == outbox.DeliveryStatusReconciling || event.AttemptCount > 1 {
		return worker.reconcile(ctx, record, now)
	}
	return worker.submit(ctx, record, now)
}

func (worker *GenerationSubmissionWorker) submit(ctx context.Context, record *generation.SubmissionRecord, now time.Time) error {
	execution, err := platform.ExecutionFromSubmissionPayload(record.StepID, record.ExecutionPayload)
	if err != nil {
		return worker.reject(ctx, record, now)
	}
	if record.ContentAccess == identity.ContentAccessReviewRestricted {
		if worker.reviewer == nil {
			// 审核配置缺失属于依赖不可用，不能被误判为内容拒绝。
			return worker.reject(ctx, record, now)
		}
		request, err := contentreview.NewRequest(record.CreationID, record.StepID, execution.Input.Prompt, execution.Input.NegativePrompt)
		if err != nil {
			return worker.reject(ctx, record, now)
		}
		decision, err := worker.reviewer.Review(ctx, request)
		if err != nil {
			// 审核网络、鉴权或响应异常均不能没收，必须立即冲正。
			return worker.reject(ctx, record, now)
		}
		if err := decision.Validate(); err != nil {
			return worker.reject(ctx, record, now)
		}
		if decision.Outcome == contentreview.OutcomeRejected {
			return worker.confiscate(ctx, record, now)
		}
	}
	result, err := worker.client.Submit(ctx, execution)
	switch {
	case err == nil:
		return worker.accept(ctx, record, result.JobID, now)
	case errors.Is(err, platform.ErrRejected),
		errors.Is(err, platform.ErrInvalidExecution),
		errors.Is(err, platform.ErrInvalidParameters),
		errors.Is(err, platform.ErrInvalidRequest),
		errors.Is(err, platform.ErrLocalSubmission):
		// 本地合同、参数或目标校验失败时尚未形成可信提交结果，必须立即冲正。
		return worker.reject(ctx, record, now)
	default:
		// 超时、5xx 或不完整响应无法证明中台未受理，必须先进入对账状态。
		return worker.markReconciling(ctx, record, now)
	}
}

func (worker *GenerationSubmissionWorker) reconcile(ctx context.Context, record *generation.SubmissionRecord, now time.Time) error {
	// 对账只传稳定步骤标识；客户端固定派生同一 externalRef 与 idempotencyKey。
	result, err := worker.client.Lookup(ctx, record.StepID)
	switch {
	case err == nil:
		return worker.accept(ctx, record, result.JobID, now)
	case errors.Is(err, platform.ErrRejected):
		return worker.reject(ctx, record, now)
	default:
		return worker.requeue(ctx, record, now.Add(defaultRetryBackoff))
	}
}

func (worker *GenerationSubmissionWorker) accept(ctx context.Context, record *generation.SubmissionRecord, jobID string, now time.Time) error {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.submission.MarkSubmitted(txCtx, generation.SubmittedCommand{
			EventID: record.EventID, LeaseToken: record.LeaseToken, JobID: jobID, At: now,
		})
	})
	return worker.resolveConflict(persistenceCtx, record.EventID, err)
}

func (worker *GenerationSubmissionWorker) markReconciling(ctx context.Context, record *generation.SubmissionRecord, now time.Time) error {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.submission.MarkReconciling(txCtx, generation.ReconcilingCommand{
			EventID: record.EventID, LeaseToken: record.LeaseToken, At: now,
		})
	})
	return worker.resolveConflict(persistenceCtx, record.EventID, err)
}

func (worker *GenerationSubmissionWorker) reject(ctx context.Context, record *generation.SubmissionRecord, now time.Time) error {
	// 该时刻在事务回调前冻结，MongoDB 自动重试回调时不得改变账本业务时间。
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		if err := worker.submission.MarkRejected(txCtx, generation.RejectedCommand{
			EventID: record.EventID, LeaseToken: record.LeaseToken, At: now,
		}); err != nil {
			return err
		}
		if _, err := worker.settler.ReverseInTx(txCtx, record.CreationID, ledger.ReversalReasonSubmissionRejected, now); err != nil {
			return err
		}
		return worker.outbox.MarkFailed(txCtx, record.EventID, record.LeaseToken, now)
	})
	return worker.resolveConflict(persistenceCtx, record.EventID, err)
}

// confiscate 在同一事务中收敛审核拒绝的创作、预留与发件箱，任何一步失败都会整体回滚。
func (worker *GenerationSubmissionWorker) confiscate(ctx context.Context, record *generation.SubmissionRecord, now time.Time) error {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		if err := worker.submission.MarkConfiscated(txCtx, generation.ConfiscatedCommand{
			EventID: record.EventID, LeaseToken: record.LeaseToken, At: now,
		}); err != nil {
			return err
		}
		if _, err := worker.settler.ConfiscateInTx(txCtx, record.CreationID, "audit_rejected", now); err != nil {
			return err
		}
		return worker.outbox.MarkFailed(txCtx, record.EventID, record.LeaseToken, now)
	})
	return worker.resolveConflict(persistenceCtx, record.EventID, err)
}

func (worker *GenerationSubmissionWorker) requeue(ctx context.Context, record *generation.SubmissionRecord, nextAttemptAt time.Time) error {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.outbox.Requeue(txCtx, record.EventID, record.LeaseToken, nextAttemptAt)
	})
	return worker.resolveConflict(persistenceCtx, record.EventID, err)
}

func (worker *GenerationSubmissionWorker) resolveConflict(ctx context.Context, eventID string, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, generation.ErrSubmissionConflict) && !errors.Is(err, outbox.ErrLeaseConflict) {
		return err
	}
	if reader, ok := worker.submission.(settledSubmissionReader); ok {
		if readErr := reader.ExistingSubmission(ctx, eventID); readErr != nil {
			return readErr
		}
	}
	// 条件更新冲突说明既有流程已收敛或租约已失效；这里绝不再次进入冲正路径。
	return nil
}

func (worker *GenerationSubmissionWorker) ready() error {
	if worker == nil || worker.outbox == nil || worker.submission == nil || worker.settler == nil || worker.tx == nil || worker.client == nil || worker.workerID == "" {
		return ErrWorkerDependenciesUnavailable
	}
	return nil
}

func (worker *GenerationSubmissionWorker) nowUTC() time.Time {
	if worker.now == nil {
		return time.Now().UTC()
	}
	return worker.now().UTC()
}

// persistenceContext 保留调用链 values（例如 trace），移除已经结束的外部请求取消信号，
// 并以短超时约束数据库状态收敛。
func (worker *GenerationSubmissionWorker) persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), submissionPersistenceTimeout)
}

// ProviderSet 只装配工作者依赖，禁止在 Provider 中启动轮询、定时器或网络投递。
var ProviderSet = wire.NewSet(NewDefaultGenerationSubmissionWorker)

var _ reservationSettler = (*ledger.Usecase)(nil)
var _ submissionClient = (*platform.Client)(nil)
