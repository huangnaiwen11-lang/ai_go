// Package worker 只装配可显式调用的一次性后台工作单元，不在此包启动循环或注册传输路由。
package worker

import (
	"context"
	"errors"
	"time"

	"ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/biz/creations"
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
	defaultWorkerID      = "local-generation-submission-worker"
	// submissionPersistenceTimeout 限制外部调用结束后的状态收敛时间，避免写入无限阻塞。
	submissionPersistenceTimeout = 5 * time.Second
)

var (
	// ErrWorkerDependenciesUnavailable 表示工作者未被完整装配，不能安全处理任何事件。
	ErrWorkerDependenciesUnavailable = errors.New("generation submission worker dependencies are unavailable")
	// ErrUnexpectedClaimedEvent 表示领取到的事件不属于本次显式调用请求的稳定事件标识。
	ErrUnexpectedClaimedEvent = errors.New("generation submission worker claimed unexpected event")
	// ErrUnsupportedSubmissionRoute is returned before review, provider I/O,
	// reconciliation, or settlement when a frozen step is not local execution.
	ErrUnsupportedSubmissionRoute = errors.New("generation submission worker: unsupported submission route")
)

// submissionRouter 按步骤冻结的执行身份解析出站能力。
//
// 它刻意不接受「当前 selector」这类入参：历史步骤必须使用自己冻结的
// provider/account，否则切换配置就会把已受理的任务改派到另一个协议上。
// 生产实现是 internal/integrations/generation.ProviderRegistry。
type submissionRouter interface {
	ProviderForRoute(provider, accountRef string) (platform.ProviderHandle, error)
	// B2BSubmission 返回 B2B 提交分支所需的只读事实（目录来源与回调模式）。
	// ok 为 false 时调用方必须拒绝 B2B 提交，绝不能退回本地执行。
	B2BSubmission() (platform.B2BSubmissionConfig, bool)
}

// LocalClientRouter 把单个本地客户端包装成只解析本地路由的解析器。
//
// 它只适用于「运行配置只启用本地 provider」的装配与测试。生产装配必须传入
// ProviderRegistry：否则冻结为 B2B 的步骤会以 ErrUnsupportedSubmissionRoute
// 失败——这是有意的 fail closed，而不是缺省降级到本地模拟器。
type LocalClientRouter struct{ Local platform.LocalSubmitter }

func (router LocalClientRouter) ProviderForRoute(provider, accountRef string) (platform.ProviderHandle, error) {
	if router.Local == nil || provider != creations.LocalExecutionProvider || accountRef != creations.DefaultLocalAccount {
		return platform.ProviderHandle{}, platform.ErrProviderNotEnabled
	}
	return platform.ProviderHandle{ID: creations.LocalExecutionProvider, Local: router.Local, AccountRef: creations.DefaultLocalAccount}, nil
}

// B2BSubmission 始终报告未配置：该路由器只承载本地出站能力。
func (router LocalClientRouter) B2BSubmission() (platform.B2BSubmissionConfig, bool) {
	return platform.B2BSubmissionConfig{}, false
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
	providers  submissionRouter
	reviewer   contentreview.Reviewer
	workerID   string
	now        func() time.Time
	// 以下两项由 submissionStore 的能力断言得到，只在 B2B 提交路径上使用。
	// 本地提交路径不依赖它们，因此缺失只影响 B2B，不影响既有行为。
	providerSubmission generation.ProviderSubmissionStore
	providerJobs       generation.ProviderJobBinder
	// R4 门禁：平台幂等保留期与 lookup 语义确认前，运行构造器不接入重发能力。
	// 测试可显式注入以校验未来受控授权机制；404 本身不是重发授权。
	reauthorization generation.ProviderReauthorizationStore
}

// NewGenerationSubmissionWorker 为测试或受控装配构造一次性提交工作者。
// 该构造器不会领取事件、写库或发起任何 HTTP 请求。
func NewGenerationSubmissionWorker(
	outboxRepository outbox.Repository,
	submissionStore generation.SubmissionStore,
	settler reservationSettler,
	tx shared.TxRunner,
	providers submissionRouter,
	reviewer contentreview.Reviewer,
	workerID string,
	now func() time.Time,
) *GenerationSubmissionWorker {
	if now == nil {
		now = time.Now
	}
	worker := &GenerationSubmissionWorker{
		outbox: outboxRepository, submission: submissionStore, settler: settler,
		tx: tx, providers: providers, reviewer: reviewer, workerID: workerID, now: now,
	}
	// Mongo 仓储同时实现三个边界；测试替身可能只实现提交状态机本身。
	// 缺失时 B2B 提交会以 ErrB2BSubmissionUnavailable 失败，而不会误用本地路径。
	if store, ok := submissionStore.(generation.ProviderSubmissionStore); ok {
		worker.providerSubmission = store
	}
	if binder, ok := submissionStore.(generation.ProviderJobBinder); ok {
		worker.providerJobs = binder
	}
	return worker
}

// NewDefaultGenerationSubmissionWorker 提供仅用于 Wire 图校验的默认装配。
// main 不会调用 DeliverOnce，因此不会启动后台循环或产生网络请求。
//
// 形参取具体注册表类型而不是 submissionRouter 接口：Wire 不做结构化接口匹配，
// 声明接口会让组合图找不到提供者，从而在别处被静默替换成单个本地客户端。
func NewDefaultGenerationSubmissionWorker(
	outboxRepository outbox.Repository,
	submissionStore generation.SubmissionStore,
	ledgerUsecase *ledger.Usecase,
	tx shared.TxRunner,
	registry *platform.ProviderRegistry,
	reviewer contentreview.Reviewer,
) *GenerationSubmissionWorker {
	return NewGenerationSubmissionWorker(outboxRepository, submissionStore, ledgerUsecase, tx, registry, reviewer, defaultWorkerID, time.Now)
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
	// 冻结路由只用于解析出站能力，绝不参与「当前 selector」判断。
	route, err := creations.NormalizeExecutionRoute(record.Route)
	if err != nil {
		return ErrUnsupportedSubmissionRoute
	}
	record.Route = route
	handle, err := worker.providers.ProviderForRoute(route.Provider, route.AccountRef)
	if err != nil {
		// 冻结身份与运行配置不一致（未授权或账号不符），不是条件写冲突。
		// 必须原样返回：上游可能已经受理这笔任务，冲正或重排都会掩盖事实。
		return err
	}
	if !handle.IsLocal() && !handle.IsB2B() {
		return ErrUnsupportedSubmissionRoute
	}
	// B2B 的首次 POST 权限由持久化意图决定，领取次数不能证明已发出请求。
	// dispatching 重试可能仅经历了目录读取失败或 Prepare 前崩溃。
	// submitB2B 会先检查意图；已存在时仍只进入对账。
	if event.DeliveryStatus == outbox.DeliveryStatusReconciling || (!handle.IsB2B() && event.AttemptCount > 1) {
		return worker.reconcile(ctx, handle, record, now)
	}
	return worker.submit(ctx, handle, record, now)
}

func (worker *GenerationSubmissionWorker) submit(ctx context.Context, handle platform.ProviderHandle, record *generation.SubmissionRecord, now time.Time) error {
	if handle.IsB2B() {
		return worker.submitB2B(ctx, handle, record, now)
	}
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
	result, err := handle.Local.Submit(ctx, execution)
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
		// 退避按 Retry-After（若有）或尝试次数计算，绝不「立即」放回队列。
		return worker.markReconciling(ctx, record, now, err)
	}
}

func (worker *GenerationSubmissionWorker) reconcile(ctx context.Context, handle platform.ProviderHandle, record *generation.SubmissionRecord, now time.Time) error {
	if handle.IsB2B() {
		return worker.reconcileB2B(ctx, handle, record, now)
	}
	// 对账只传稳定步骤标识；客户端固定派生同一 externalRef 与 idempotencyKey。
	result, err := handle.Local.Lookup(ctx, record.StepID)
	switch {
	case err == nil:
		return worker.accept(ctx, record, result.JobID, now)
	case errors.Is(err, platform.ErrRejected):
		return worker.reject(ctx, record, now)
	default:
		return worker.requeue(ctx, record, nextSubmissionAttempt(now, record.Fence, err))
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

// markReconciling 把「提交结果未知」退回核对状态，并按 cause 决定再次可领取的时刻。
// cause 决定退避：平台给出 Retry-After 时以它为准，否则按尝试次数指数退避。
func (worker *GenerationSubmissionWorker) markReconciling(ctx context.Context, record *generation.SubmissionRecord, now time.Time, cause error) error {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	nextAttemptAt := nextSubmissionAttempt(now, record.Fence, cause)
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.submission.MarkReconciling(txCtx, generation.ReconcilingCommand{
			EventID: record.EventID, LeaseToken: record.LeaseToken, At: now, NextAttemptAt: nextAttemptAt,
		})
	})
	return worker.resolveConflict(persistenceCtx, record.EventID, err)
}

func (worker *GenerationSubmissionWorker) reject(ctx context.Context, record *generation.SubmissionRecord, now time.Time, causes ...generation.ProviderRejectionCause) error {
	// 该时刻在事务回调前冻结，MongoDB 自动重试回调时不得改变账本业务时间。
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	var cause generation.ProviderRejectionCause
	if len(causes) > 0 {
		cause = causes[0]
	}
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		if err := worker.submission.MarkRejected(txCtx, generation.RejectedCommand{
			EventID: record.EventID, LeaseToken: record.LeaseToken, At: now, ProviderRejectionCause: cause,
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
	if worker == nil || worker.outbox == nil || worker.submission == nil || worker.settler == nil || worker.tx == nil || worker.providers == nil || worker.workerID == "" {
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
var _ submissionRouter = (*platform.ProviderRegistry)(nil)
var _ submissionRouter = LocalClientRouter{}
