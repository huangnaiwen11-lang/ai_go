package worker

import (
	"context"
	"errors"
	"net/http"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

var (
	// ErrB2BSubmissionUnavailable 表示 B2B 提交所需的冻结事实或运行配置缺失。
	// 它与条件写冲突区分开：调用方不得据此冲正、重排或改写路由。
	ErrB2BSubmissionUnavailable = errors.New("generation submission worker: b2b submission unavailable")
)

// reauthorizationGracePeriod 是「平台明确查不到」到「允许重新授权提交」之间的最短等待。
//
// 它吸收平台侧的读后写延迟：刚受理的任务可能短暂查不到。重发在平台侧虽然幂等，
// 但白跑一次外呼，而且会把真正的读后写延迟掩盖成「任务不存在」。
//
// 10 分钟不是拍脑袋：重排退避从 10s 起步、5 分钟封顶，等待期满时已经跨过
// 至少一个完整的 5 分钟周期，尝试计数也早已远超首次。
const reauthorizationGracePeriod = 10 * time.Minute

// submitB2B 提交一个冻结为 PolarStar B2B 的步骤。
//
// 顺序固定为：读既有提交意图 → 映射冻结请求 → 事务内领取提交授权 → 提交 →
// 事务内绑定外部任务。任何一步失败都不允许退回本地执行。
func (worker *GenerationSubmissionWorker) submitB2B(ctx context.Context, handle platform.ProviderHandle, record *generation.SubmissionRecord, now time.Time) error {
	config, ok := worker.providers.B2BSubmission()
	if !ok || handle.B2B == nil || worker.providerSubmission == nil || worker.providerJobs == nil {
		return ErrB2BSubmissionUnavailable
	}
	// 已存在冻结提交意图说明这笔任务可能已经发出去过。绝不能在这里再次 POST：
	// 授权只发放一次，之后只能按同一幂等键对账。
	//
	// 唯一的例外在 reconcileB2B：当对账**证明**平台从未受理这笔任务时，
	// 它可以消费一次有审计的重新授权。这里没有那份证据，因此只能对账。
	intent, err := worker.providerSubmission.ReadProviderSubmission(ctx, record.StepID)
	switch {
	case err == nil && intent != nil:
		return worker.reconcileB2B(ctx, handle, record, now)
	case err != nil:
		return err
	case record.CreationStatus == creations.CreationStatusCancelling:
		// A cancelling creation must never obtain a first POST permission.  The
		// cancellation request is accepted only after a durable intent exists,
		// but this explicit fence also protects repaired/corrupt documents from
		// turning a user cancellation into a newly created provider job.
		return ErrB2BSubmissionUnavailable
	}
	request, err := mapB2BRequest(ctx, config, record)
	switch {
	case err == nil:
	case b2bMappingDeterministicFailure(err):
		// 冻结配方与冻结目录确实不兼容，或目录里确实没有这个版本。
		// 重试不会改变结果，且尚未发出任何请求，因此可以安全冲正。
		return worker.reject(ctx, record, now)
	default:
		// 读取目录本身失败（数据库抖动、超时等）：请求同样尚未发出，
		// 但这属于基础设施问题而不是「这笔任务做不了」。
		// 冲正会把用户已经预扣的钻石退掉并宣告创作失败，代价远大于重排一次。
		return worker.requeue(ctx, record, nextSubmissionAttempt(now, record.Fence, err))
	}
	if err := worker.prepareB2BSubmission(ctx, record, request, now); err != nil {
		if errors.Is(err, generation.ErrSubmissionConflict) {
			// 并发工作者已拿到授权，或既有意图不可用。两种情况都只能对账：
			// 这里无法区分「还没发出去」与「已经发出去」，而重发是不可接受的。
			return worker.reconcileB2B(ctx, handle, record, now)
		}
		return err
	}
	// 提交前用协议适配器重新校验冻结字节，确保发送的就是持久化的那一份。
	frozen, err := polarstarb2b.RestoreRequest(b2bRoute(record.Route, record.StepID), request.Payload(), request.Digest())
	if err != nil {
		return ErrB2BSubmissionUnavailable
	}
	job, err := handle.B2B.Submit(ctx, frozen)
	if decision := b2bDeterministicRejection(err); decision.Reject {
		// 确定性 4xx（含余额不足）重试不会改变结果，立即冲正。只有
		// PAYMENT_REQUIRED 才把内部 provider_payment_required 写入步骤事实。
		return worker.reject(ctx, record, now, decision.Cause)
	}
	switch {
	case err == nil:
		return worker.bindB2BJob(ctx, record, job, now)
	case errors.Is(err, polarstarb2b.ErrNotSent):
		// 本地校验失败：请求从未离开进程，可以安全冲正。
		return worker.reject(ctx, record, now)
	case errors.Is(err, polarstarb2b.ErrConflict):
		// 幂等冲突说明同一 key 已有任务；必须对账，既不能重发也不能冲正。
		return worker.reconcileB2B(ctx, handle, record, now)
	default:
		// 429/5xx/传输失败都无法证明中台未受理，只能进入对账。
		// 平台给出 Retry-After 时按它退避，否则按尝试次数指数退避。
		return worker.markReconciling(ctx, record, now, err)
	}
}

// reconcileB2B 按冻结的幂等键核对一笔可能已经发出的 B2B 任务。
//
// 它绝不重新 POST，也绝不因为查不到就冲正：两者都会把「结果未知」当成
// 「没有结果」，而用户的钻石已经被预扣。
func (worker *GenerationSubmissionWorker) reconcileB2B(ctx context.Context, handle platform.ProviderHandle, record *generation.SubmissionRecord, now time.Time) error {
	if worker.providerSubmission == nil {
		return ErrB2BSubmissionUnavailable
	}
	intent, err := worker.providerSubmission.ReadProviderSubmission(ctx, record.StepID)
	if err != nil || intent == nil {
		return ErrB2BSubmissionUnavailable
	}
	frozen, err := polarstarb2b.RestoreRequest(b2bRoute(record.Route, record.StepID), intent.Request.Payload, intent.Request.Digest)
	if err != nil {
		return ErrB2BSubmissionUnavailable
	}
	job, err := handle.B2B.Lookup(ctx, polarstarb2b.LookupKey{
		AccountRef:     handle.AccountRef,
		IdempotencyKey: frozen.IdempotencyKey(),
		ExternalID:     frozen.ExternalID(),
		Capability:     frozen.Capability(),
	})
	switch {
	case err == nil:
	case errors.Is(err, polarstarb2b.ErrNotSent):
		return ErrB2BSubmissionUnavailable
	case b2bLookupNotFound(err):
		// 404 可能来自短暂不可见或保留期到期，不能证明从未受理。
		// 运行构造器未授权重发；下层会按退避继续查询。
		return worker.reauthorizeB2B(ctx, handle, record, intent, frozen, now, err)
	default:
		// 传输失败、429 与 5xx 都无法证明中台没有这笔任务。
		// 按退避继续对账，既不重发也不冲正；平台给出 Retry-After 时以它为准。
		return worker.requeue(ctx, record, nextSubmissionAttempt(now, record.Fence, err))
	}
	// 先绑定再判终态：否则「提交后立刻完成」的任务会丢掉外部任务号。
	if err := worker.bindB2BJob(ctx, record, job, now); err != nil {
		return err
	}
	// 终态与非终态在这里都不需要额外动作，因此刻意不区分。
	//
	// BindProviderJob 已在同一事务里把提交事件结清（delivery_status=delivered）
	// 并唤醒 generation.reconcile 事件，终态由对账 Worker 走统一 CAS 落库
	// （ApplyProviderTerminal）。本函数对「任务已经跑完」没有别的事可做。
	//
	// 这里曾经在终态时返回 ErrB2BTerminalUnavailable，后果是 Runner.Run 上抛、
	// 组合根 panic：一笔正常完成的任务换来一次进程崩溃，并打断同进程内其他
	// 在途任务。终态是每笔任务最终都会走到的正常终局，绝不能走 fail-fast 通道。
	return nil
}

// b2bLookupNotFound 判定「平台按确定派生的幂等键确实查不到这笔任务」。
//
// 只分类 HTTP 404，不赋予其“从未受理”的含义。传输失败、429 与 5xx
// 走独立退避分支；任何一种结果均不能单独授权第二次 POST。
func b2bLookupNotFound(err error) bool {
	var clientErr *polarstarb2b.ClientError
	return errors.As(err, &clientErr) && clientErr.HTTPStatus == http.StatusNotFound
}

// reauthorizeB2B 保留受控重新授权的机制，但运行构造器不会注入该能力。
// 开放前必须核实平台幂等保留期、查询语义并补齐显式授权策略；十分钟等待
// 不是安全证明。未授权时只对账、不重发、不退款。
func (worker *GenerationSubmissionWorker) reauthorizeB2B(
	ctx context.Context,
	handle platform.ProviderHandle,
	record *generation.SubmissionRecord,
	intent *generation.ProviderSubmissionIntent,
	frozen polarstarb2b.Request,
	now time.Time,
	cause error,
) error {
	// 等待期未满：平台可能只是还没把刚受理的任务暴露出来。
	if now.Sub(intent.PreparedAt) < reauthorizationGracePeriod {
		return worker.requeue(ctx, record, nextSubmissionAttempt(now, record.Fence, cause))
	}
	if worker.reauthorization == nil {
		// 仓储不支持重新授权：保持既有语义，只对账不重发。
		return worker.requeue(ctx, record, nextSubmissionAttempt(now, record.Fence, cause))
	}
	command := generation.ReauthorizeProviderSubmissionCommand{
		EventID: record.EventID, CreationID: record.CreationID, StepID: record.StepID,
		LeaseToken: record.LeaseToken, LeaseOwner: record.LeaseOwner, Fence: record.Fence, At: now.UTC(),
		Reason: generation.ReauthorizationReasonLookupNotFound,
	}
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.reauthorization.ReauthorizeProviderSubmission(txCtx, command)
	})
	switch {
	case err == nil:
	case errors.Is(err, generation.ErrReauthorizationExhausted):
		// 额度已用完，或持久化事实不再满足授权条件。
		// 不凭空造终态，继续按退避对账。
		return worker.requeue(ctx, record, nextSubmissionAttempt(now, record.Fence, cause))
	default:
		return err
	}
	// 发送的必须是持久化的那一份：frozen 由 RestoreRequest 从冻结字节还原。
	job, err := handle.B2B.Submit(ctx, frozen)
	if err != nil {
		// 成功之外的一切结果都回到对账：
		//   - ErrConflict 说明同键已有任务，下一次 Lookup 就会找到它；
		//   - 429/5xx/传输失败仍然无法证明结果；
		//   - 确定性拒绝也不足以推翻「第一次提交结果未知」这个事实。
		return worker.requeue(ctx, record, nextSubmissionAttempt(now, record.Fence, err))
	}
	return worker.bindB2BJob(ctx, record, job, now)
}

// mapB2BRequest 把冻结配方按冻结目录版本映射成确定的公开 wire 请求。
//
// 目录版本来自步骤冻结的 route.MappingVersion，已发布版本不可原地修改，
// 因此同一步骤在任何时刻重放都会得到同一份字节。
func mapB2BRequest(ctx context.Context, config platform.B2BSubmissionConfig, record *generation.SubmissionRecord) (polarstarb2b.Request, error) {
	if record == nil {
		return polarstarb2b.Request{}, ErrB2BSubmissionUnavailable
	}
	recipe, err := creations.ParseB2BProductRecipe(record.ExecutionPayload)
	if err != nil {
		return polarstarb2b.Request{}, err
	}
	return mapB2BRecipe(ctx, config, record.Route, record.StepID, recipe)
}

// mapB2BRecipe validates a frozen public product against its frozen mapping
// version and returns the exact request the submission worker would later
// persist and POST. The materializer uses the same helper after binding an R2
// opening_frame, so a two-step video cannot make its second stage ready with a
// product that normal B2B submission would reject.
func mapB2BRecipe(ctx context.Context, config platform.B2BSubmissionConfig, route creations.ExecutionRoute, stepID string, recipe creations.B2BProductRecipe) (polarstarb2b.Request, error) {
	if !config.Ready() || stepID == "" {
		return polarstarb2b.Request{}, ErrB2BSubmissionUnavailable
	}
	normalizedRoute, err := creations.NormalizeExecutionRoute(route)
	if err != nil || normalizedRoute.Provider != creations.PolarStarB2BProvider {
		return polarstarb2b.Request{}, ErrB2BSubmissionUnavailable
	}
	catalog, err := config.Catalogs.PublishedCatalog(ctx, normalizedRoute.MappingVersion)
	if err != nil {
		// 原样返回：调用方要区分「这个版本确实没有」与「这次没读到」，
		// 前者是确定性失败，后者只应重排。
		return polarstarb2b.Request{}, err
	}
	if catalog == nil {
		return polarstarb2b.Request{}, platform.ErrProviderMappingSnapshot
	}
	snapshot, err := platform.MappingSnapshotFromCatalog(*catalog, normalizedRoute.MappingVersion)
	if err != nil {
		return polarstarb2b.Request{}, err
	}
	mapping, ok := snapshot.Models[recipe.ProductKey]
	if !ok {
		return polarstarb2b.Request{}, platform.ErrProviderMappingSnapshot
	}
	// capability 取自目录条目：admission 已经确认它与步骤原子一致，适配器会再核一遍。
	product, err := platform.ProductInputFromRecipe(recipe, mapping.Capability)
	if err != nil {
		return polarstarb2b.Request{}, err
	}
	return polarstarb2b.MapRequest(snapshot, b2bRoute(normalizedRoute, stepID), product, config.Callback)
}

// prepareB2BSubmission 在调用方事务中领取一次性提交授权。
// 它不做任何 HTTP 调用：授权与网络请求分开，才能让「授权已发但结果未知」
// 收敛到只读的对账路径。
func (worker *GenerationSubmissionWorker) prepareB2BSubmission(ctx context.Context, record *generation.SubmissionRecord, request polarstarb2b.Request, now time.Time) error {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	command := generation.PrepareProviderSubmissionCommand{
		EventID: record.EventID, CreationID: record.CreationID,
		LeaseToken: record.LeaseToken, LeaseOwner: record.LeaseOwner,
		Fence: record.Fence, At: now,
		Request: generation.FrozenProviderRequest{
			Route: record.Route, StepID: record.StepID, Capability: request.Capability(),
			IdempotencyKey: request.IdempotencyKey(), Digest: request.Digest(), Payload: request.Payload(),
		},
	}
	return worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.providerSubmission.PrepareProviderSubmission(txCtx, command)
	})
}

// bindB2BJob 在调用方事务中把外部任务号绑定到步骤与提交事件。
// 绑定同时唤醒按步骤去重的对账事件，使恢复不依赖原租约超时。
func (worker *GenerationSubmissionWorker) bindB2BJob(ctx context.Context, record *generation.SubmissionRecord, job polarstarb2b.Job, now time.Time) error {
	persistenceCtx, cancel := worker.persistenceContext(ctx)
	defer cancel()
	binding := generation.ProviderJobBinding{
		EventID: record.EventID, CreationID: record.CreationID, StepID: record.StepID,
		LeaseToken: record.LeaseToken, LeaseOwner: record.LeaseOwner, Fence: record.Fence,
		Route: record.Route, ExternalID: job.JobID, Capability: job.Capability, At: now,
	}
	err := worker.tx.WithinTx(persistenceCtx, func(txCtx context.Context) error {
		return worker.providerJobs.BindProviderJob(txCtx, binding)
	})
	return worker.resolveConflict(persistenceCtx, record.EventID, err)
}

// b2bRoute 把冻结的执行身份投影成适配器需要的路由。
// StepID 与 ExecutionRoute 分开存放：前者是任务身份，后者是执行身份。
func b2bRoute(route creations.ExecutionRoute, stepID string) polarstarb2b.Route {
	return polarstarb2b.Route{
		StepID: stepID, Provider: route.Provider, AccountRef: route.AccountRef,
		ContractVersion: route.ContractVersion, MappingVersion: route.MappingVersion,
	}
}

// b2bMappingDeterministicFailure 判定「映射失败是这笔任务本身做不了」。
//
// 只有明确的语义失败才算确定性：
//   - ErrMappingCatalogUnavailable：冻结的目录版本确实不在库里（创建时它存在过，
//     现在没了属于数据完整性问题，重试无用）
//   - ErrProviderMappingSnapshot / ErrProviderRouteMismatch：目录形状不可投影、
//     没有启用条目、产品键不存在，或版本与冻结身份不一致
//   - ErrInvalidAdmission / ErrInvalidRequest：冻结配方字节损坏，或公开合同校验不过
//
// 任何未识别的错误（尤其是数据库读失败）都必须返回 false，由调用方重排：
// 把基础设施抖动当成「任务做不了」会直接冲正用户的预扣。
func b2bMappingDeterministicFailure(err error) bool {
	return errors.Is(err, generation.ErrMappingCatalogUnavailable) ||
		errors.Is(err, platform.ErrProviderMappingSnapshot) ||
		errors.Is(err, platform.ErrProviderRouteMismatch) ||
		errors.Is(err, creations.ErrInvalidAdmission) ||
		errors.Is(err, polarstarb2b.ErrInvalidRequest)
}

// b2bDeterministicRejection 判定「重试不会改变结果」的供应商失败。
//
// 只认协议适配器白名单内的错误码：任何未识别的错误都必须走对账，
// 因为未知错误既不能证明任务没被受理，也不能证明重试无用。
//
// 刻意不含 CONFLICT：幂等冲突必须按 409 走对账（见 submitB2B 的 ErrConflict 分支），
// 而不是被当成「这笔任务做不了」直接冲正。
type b2bRejectionDecision struct {
	Reject bool
	Cause  generation.ProviderRejectionCause
}

func b2bDeterministicRejection(err error) b2bRejectionDecision {
	var clientErr *polarstarb2b.ClientError
	if !errors.As(err, &clientErr) {
		return b2bRejectionDecision{}
	}
	switch clientErr.Code {
	case "PAYMENT_REQUIRED":
		return b2bRejectionDecision{Reject: true, Cause: generation.ProviderRejectionCausePaymentRequired}
	case "BAD_REQUEST", "VALIDATION_ERROR", "MODEL_INVALID", "UNAUTHORIZED",
		"FORBIDDEN", "NOT_FOUND", "MODEL_INCOMPATIBLE":
		return b2bRejectionDecision{Reject: true}
	default:
		return b2bRejectionDecision{}
	}
}
