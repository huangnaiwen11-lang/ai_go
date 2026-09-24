package creations

import (
	"context"
	"errors"
	"time"

	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/executionv2"

	"github.com/google/uuid"
)

// userReader 是创作编排读取用户状态所需的最小依赖。
// 它避免创作模块依赖身份模块的完整用例或底层仓储实现。
type userReader interface {
	Find(context.Context, string) (*identity.User, error)
}

// entitlementEvaluator 是创作编排计算门禁、价格和日免上限所需的最小依赖。
type entitlementEvaluator interface {
	EvaluateGenerationAt(entitlement.UserSnapshot, entitlement.GenerationRequest, time.Time) (*entitlement.GenerationDecision, error)
	ResolveDailyBenefitsAt(entitlement.UserSnapshot, time.Time) (*entitlement.DailyBenefits, error)
}

// reservationUsecase 是创作总事务调用账本预留所需的最小依赖。
// 调用方已持有事务，账本实现不得在该入口内创建嵌套事务。
type reservationUsecase interface {
	ReserveInTx(context.Context, ledger.ReserveRequest) (*ledger.Reservation, error)
	FindReservation(context.Context, string) (*ledger.Reservation, error)
	CurrentDiamondBalance(context.Context, string) (int64, error)
}

type deferredRecipeWriterProvider interface {
	DeferredRecipeWriter() DeferredRecipeWriter
}

// Usecase 在同一事务中协调创作占位、步骤计划和账本预留。
// 它不调用生成中台，不创建支付、账户或每日钻石事实。
type Usecase struct {
	users         userReader
	subscriptions entitlement.SubscriptionReader
	entitlements  entitlementEvaluator
	creations     Repository
	reservations  reservationUsecase
	events        outbox.Writer
	recipes       DeferredRecipeWriter
	tx            shared.TxRunner
	admissions    AdmissionResolver
	clock         func() time.Time
}

// NewUsecase 创建创作占位与预留编排用例。
// admissions 为 nil 表示运行配置尚未授权 B2B：此时新步骤只冻结本地归属，
// 且任何已编译的公开配方都会被拒绝，不会静默降级为本地执行。
func NewUsecase(
	users userReader,
	subscriptions entitlement.SubscriptionReader,
	entitlements entitlementEvaluator,
	creations Repository,
	reservations reservationUsecase,
	events outbox.Writer,
	tx shared.TxRunner,
	admissions AdmissionResolver,
) *Usecase {
	return newUsecase(users, subscriptions, entitlements, creations, deferredRecipeWriterFor(creations), reservations, events, tx, admissions, func() time.Time {
		return time.Now().UTC()
	})
}

// NewUsecaseWithClock 仅用于本地测试中固定业务时刻。
// 生产装配必须使用 NewUsecase，避免业务时钟被调用方随意替换。
func NewUsecaseWithClock(
	users userReader,
	subscriptions entitlement.SubscriptionReader,
	entitlements entitlementEvaluator,
	creations Repository,
	reservations reservationUsecase,
	events outbox.Writer,
	tx shared.TxRunner,
	admissions AdmissionResolver,
	clock func() time.Time,
) *Usecase {
	if clock == nil {
		clock = func() time.Time {
			return time.Now().UTC()
		}
	}
	return newUsecase(users, subscriptions, entitlements, creations, deferredRecipeWriterFor(creations), reservations, events, tx, admissions, clock)
}

func newUsecase(
	users userReader,
	subscriptions entitlement.SubscriptionReader,
	entitlements entitlementEvaluator,
	creations Repository,
	recipes DeferredRecipeWriter,
	reservations reservationUsecase,
	events outbox.Writer,
	tx shared.TxRunner,
	admissions AdmissionResolver,
	clock func() time.Time,
) *Usecase {
	return &Usecase{
		users:         users,
		subscriptions: subscriptions,
		entitlements:  entitlements,
		creations:     creations,
		recipes:       recipes,
		reservations:  reservations,
		events:        events,
		tx:            tx,
		admissions:    admissionResolverOrDefault(admissions),
		clock:         clock,
	}
}

func deferredRecipeWriterFor(repository Repository) DeferredRecipeWriter {
	provider, ok := repository.(deferredRecipeWriterProvider)
	if !ok {
		return nil
	}
	return provider.DeferredRecipeWriter()
}

// CreateReserved 原子地创建创作占位、内部步骤和账本预留。
// 相同幂等键仅能重放相同用户和相同请求指纹对应的既有创作。
func (usecase *Usecase) CreateReserved(ctx context.Context, request CreateReservedRequest) (*CreateReservedResult, error) {
	if err := usecase.validateDependencies(); err != nil {
		return nil, err
	}
	if err := validateCreateReservedRequest(request); err != nil {
		return nil, err
	}
	if request.InitialSubmission != nil && request.B2BSubmission != nil {
		// 两种合同不得同时编译：那说明调用方自己也不确定该发给哪个 provider。
		return nil, ErrInvalidCreateCommand
	}
	if (request.InitialSubmission != nil || request.B2BSubmission != nil) && usecase.events == nil {
		return nil, ErrInvalidCreateCommand
	}
	if err := ValidatePlan(request.Product, request.Plan); err != nil {
		return nil, err
	}
	deferredRecipe, err := compileDeferredRecipe(request)
	if err != nil {
		return nil, err
	}
	if deferredRecipe != nil && usecase.recipes == nil {
		return nil, ErrInvalidCreateCommand
	}
	snapshot, err := compileInitialSubmission(request)
	if err != nil {
		return nil, err
	}
	recipe, err := compileB2BSubmission(request)
	if err != nil {
		return nil, err
	}
	if request.Product.Output == entitlement.ProductOutputVideo && snapshot == nil && recipe == nil {
		return nil, ErrInvalidCreateCommand
	}

	// 指纹计算失败时不能继续查询幂等键，避免无效请求错误收敛到既有创作。
	deferredDigest := ""
	if deferredRecipe != nil {
		deferredDigest = deferredRecipe.Digest
	}
	fingerprint, err := requestFingerprint(request, snapshot, deferredDigest)
	if err != nil {
		return nil, err
	}

	if _, err := usecase.findAvailableUser(ctx, request.UserID); err != nil {
		return nil, err
	}
	var submissionPayload []byte
	switch {
	case snapshot != nil:
		submissionPayload, err = snapshot.MarshalSubmissionPayload()
		if err != nil {
			return nil, err
		}
	case recipe != nil:
		// 公开配方是 B2B 首步骤的冻结载荷，与本地快照占同一个发件箱载荷槽位；
		// 提交器按步骤冻结的 provider 决定用哪种合同解析它。
		submissionPayload, err = recipe.Marshal()
		if err != nil {
			return nil, ErrInvalidCreateCommand
		}
	}
	if existing, err := usecase.creations.FindByIdempotencyKey(ctx, request.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return usecase.resolveExisting(ctx, existing, request.UserID, fingerprint)
	}

	// 事务回调可能被 MongoDB 重试，业务时刻必须在进入事务前固定一次。
	businessNow := usecase.clock().UTC()
	// 归属同样在事务前冻结。已发布目录版本不可变，因此这里定下的版本号在事务内
	// 以及后续任何重放时刻都对应同一份映射；事务只负责把它写下去。
	admissions, err := usecase.resolveAdmissions(ctx, request, recipe, deferredRecipe)
	if err != nil {
		return nil, err
	}
	creation, steps := newCreationFacts(request, admissions, fingerprint, businessNow)
	if deferredRecipe != nil {
		deferredRecipe.CreatedAt = businessNow
		deferredRecipe.UpdatedAt = businessNow
	}
	result := &CreateReservedResult{Creation: creation, Steps: steps}
	err = usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		user, err := usecase.findAvailableUser(txCtx, request.UserID)
		if err != nil {
			return err
		}

		subscription, err := usecase.subscriptions.FindByUserID(txCtx, request.UserID)
		if err != nil {
			return err
		}
		userSnapshot := entitlement.UserSnapshot{
			AccountBound: user.BindingState == identity.BindingStateBound,
			Timezone:     user.Timezone,
			Subscription: subscription,
		}
		decision, err := usecase.entitlements.EvaluateGenerationAt(userSnapshot, request.Product, businessNow)
		if err != nil {
			return err
		}
		benefits, err := usecase.entitlements.ResolveDailyBenefitsAt(userSnapshot, businessNow)
		if err != nil {
			return err
		}
		if decision == nil || benefits == nil {
			return ErrInvalidCreateCommand
		}

		quota, err := quotaReservationFor(request.Product, benefits)
		if err != nil {
			return err
		}
		if err := usecase.creations.Create(txCtx, creation, steps); err != nil {
			return err
		}
		reservation, err := usecase.reservations.ReserveInTx(txCtx, ledger.ReserveRequest{
			CreationID:    creation.ID,
			UserID:        request.UserID,
			PriceDiamonds: decision.PriceDiamonds,
			Quota:         quota,
			BusinessAt:    businessNow,
		})
		if err != nil {
			return err
		}
		balanceAfter, err := usecase.reservations.CurrentDiamondBalance(txCtx, request.UserID)
		if err != nil {
			return err
		}
		result.Reservation = reservation
		result.DiamondBalanceAfter = balanceAfter
		if len(submissionPayload) > 0 {
			firstReadyStep, err := firstReadyStep(steps)
			if err != nil {
				return err
			}
			event, err := outbox.NewPending(
				outbox.SubmissionEventID(firstReadyStep.ID),
				creation.ID,
				submissionPayload,
				businessNow,
			)
			if err != nil {
				return err
			}
			if err := usecase.events.Enqueue(txCtx, event); err != nil {
				return err
			}
		}
		if deferredRecipe == nil {
			return nil
		}
		deferredRecipe.StepID = steps[1].ID
		deferredRecipe.CreationID = creation.ID
		return usecase.recipes.Create(txCtx, deferredRecipe)
	})
	if err == nil {
		return result, nil
	}
	if errors.Is(err, ErrCreationAlreadyExists) {
		return usecase.resolveExistingByKey(ctx, request.IdempotencyKey, request.UserID, fingerprint, err)
	}
	return nil, err
}

func compileDeferredRecipe(request CreateReservedRequest) (*DeferredRecipe, error) {
	isTwoStepTextToVideo := request.Product.Output == entitlement.ProductOutputVideo && len(request.Plan) == 2 && request.Plan[0].Atom == AtomTextToImage && request.Plan[1].Atom == AtomImageToVideo
	if !isTwoStepTextToVideo {
		if request.DeferredImageToVideo != nil || request.DeferredB2BImageToVideo != nil {
			return nil, ErrInvalidCreateCommand
		}
		return nil, nil
	}
	if (request.DeferredImageToVideo == nil) == (request.DeferredB2BImageToVideo == nil) {
		return nil, ErrInvalidCreateCommand
	}
	if request.DeferredB2BImageToVideo != nil {
		if request.InitialSubmission != nil || request.B2BSubmission == nil {
			return nil, ErrInvalidCreateCommand
		}
		deferred, err := request.DeferredB2BImageToVideo.Normalize()
		if err != nil {
			return nil, ErrInvalidCreateCommand
		}
		return &DeferredRecipe{
			Atom: AtomImageToVideo, Protocol: DeferredRecipeProtocolB2B,
			B2B: &deferred, Digest: deferred.Digest, Status: DeferredRecipeStatusPending,
		}, nil
	}
	compiled, err := executionv2.CompileDeferredImageToVideo(request.DeferredImageToVideo.ModelSKU, request.DeferredImageToVideo.InputTemplate)
	if err != nil {
		return nil, ErrInvalidCreateCommand
	}
	return &DeferredRecipe{
		Atom:          AtomImageToVideo,
		Protocol:      DeferredRecipeProtocolExecutionV2,
		ModelSKU:      compiled.ModelSKU(),
		InputTemplate: compiled.FrozenInputTemplate(),
		Digest:        compiled.Digest,
		Status:        DeferredRecipeStatusPending,
	}, nil
}

// compileB2BSubmission 规范化服务端模板编译器给出的公开产品配方。
// 它不判断归属：归属由 AdmissionResolver 决定，这里只保证字节可以被冻结。
func compileB2BSubmission(request CreateReservedRequest) (*B2BProductRecipe, error) {
	if request.B2BSubmission == nil {
		return nil, nil
	}
	normalized, err := request.B2BSubmission.Normalize()
	if err != nil {
		return nil, ErrInvalidCreateCommand
	}
	return &normalized, nil
}

// resolveAdmissions 冻结每个新步骤的执行归属。
//
// 失败一律向上返回，不提供任何「解析不出来就走本地」的兜底：那会把 B2B 请求
// 静默跑成本地模拟器，并让用户在不知情的情况下拿到本地生成结果。
func (usecase *Usecase) resolveAdmissions(ctx context.Context, request CreateReservedRequest, recipe *B2BProductRecipe, deferred *DeferredRecipe) ([]StepAdmission, error) {
	var deferredB2B *DeferredB2BImageToVideo
	if deferred != nil && deferred.Protocol == DeferredRecipeProtocolB2B {
		deferredB2B = deferred.B2B
	}
	if recipe != nil && len(request.Plan) != 1 && deferredB2B == nil {
		// 两步 B2B 计划必须同时有一份未绑定的第二阶段公开配方；仅有第一步
		// 配方会把用户带进一个永远无法提交的 blocked 步骤，因此仍 fail closed。
		return nil, ErrB2BProductRecipeUnavailable
	}
	admissionRequests := make([]AdmissionRequest, 0, len(request.Plan))
	for index, plan := range request.Plan {
		admissionRequest := AdmissionRequest{
			UserID:          request.UserID,
			TemplateID:      request.TemplateID,
			TemplateVersion: request.TemplateVersion,
			Product:         request.Product,
			Atom:            plan.Atom,
			Sequence:        plan.Sequence,
		}
		if index == 0 {
			admissionRequest.B2B = recipe
		} else if deferredB2B != nil && index == 1 {
			admissionRequest.B2B = &deferredB2B.Recipe
			admissionRequest.DeferredB2B = true
		}
		admissionRequests = append(admissionRequests, admissionRequest)
	}
	admissions := make([]StepAdmission, 0, len(admissionRequests))
	if batch, ok := usecase.admissions.(BatchAdmissionResolver); ok {
		resolved, err := batch.ResolveAdmissions(ctx, admissionRequests)
		if err != nil {
			return nil, err
		}
		if len(resolved) != len(admissionRequests) {
			return nil, ErrAdmissionConfigurationUnavailable
		}
		admissions = append(admissions, resolved...)
	} else {
		for _, admissionRequest := range admissionRequests {
			resolved, err := usecase.admissions.ResolveAdmission(ctx, admissionRequest)
			if err != nil {
				return nil, err
			}
			normalized, err := resolved.Normalize()
			if err != nil {
				return nil, err
			}
			admissions = append(admissions, normalized)
		}
	}
	for index, admission := range admissions {
		normalized, err := admission.Normalize()
		if err != nil {
			return nil, err
		}
		admissions[index] = normalized
	}
	// 编译器已经按公开合同编译，但解析结果不是 B2B：这是静默降级，必须拒绝。
	if recipe != nil && admissions[0].B2B == nil {
		return nil, ErrB2BProductRecipeUnavailable
	}
	if deferredB2B != nil && (len(admissions) != 2 || admissions[1].B2B == nil || admissions[1].Route.Provider != PolarStarB2BProvider) {
		return nil, ErrB2BProductRecipeUnavailable
	}
	return admissions, nil
}

func (usecase *Usecase) validateDependencies() error {
	if usecase == nil || usecase.users == nil || usecase.subscriptions == nil || usecase.entitlements == nil || usecase.creations == nil || usecase.reservations == nil || usecase.tx == nil || usecase.admissions == nil || usecase.clock == nil {
		return ErrInvalidCreateCommand
	}
	return nil
}

func validateCreateReservedRequest(request CreateReservedRequest) error {
	if request.UserID == "" || request.IdempotencyKey == "" || request.TemplateID == "" || request.TemplateVersion <= 0 {
		return ErrInvalidCreateCommand
	}
	return nil
}

func firstReadyStep(steps []CreationStep) (CreationStep, error) {
	for _, step := range steps {
		if step.SubmitStatus == StepSubmitStatusReady {
			return step, nil
		}
	}
	return CreationStep{}, ErrInvalidCreateCommand
}

func (usecase *Usecase) findAvailableUser(ctx context.Context, userID string) (*identity.User, error) {
	user, err := usecase.users.Find(ctx, userID)
	if err != nil {
		if errors.Is(err, identity.ErrUserNotFound) {
			return nil, ErrAccountUnavailable
		}
		return nil, err
	}
	if user == nil || user.AccountStatus != identity.AccountStatusNormal {
		return nil, ErrAccountUnavailable
	}
	return user, nil
}

func (usecase *Usecase) resolveExistingByKey(ctx context.Context, idempotencyKey, userID, fingerprint string, originalErr error) (*CreateReservedResult, error) {
	existing, err := usecase.creations.FindByIdempotencyKey(ctx, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, originalErr
	}
	return usecase.resolveExisting(ctx, existing, userID, fingerprint)
}

func (usecase *Usecase) resolveExisting(ctx context.Context, existing *Creation, userID, fingerprint string) (*CreateReservedResult, error) {
	if existing.UserID != userID || existing.RequestFingerprint != fingerprint {
		return nil, ErrCreationCommandConflict
	}
	steps, err := usecase.creations.ListSteps(ctx, existing.ID)
	if err != nil {
		return nil, err
	}
	reservation, err := usecase.reservations.FindReservation(ctx, existing.ID)
	if err != nil {
		return nil, err
	}
	if reservation == nil {
		return nil, ErrInvalidCreateCommand
	}
	balanceAfter, err := usecase.reservations.CurrentDiamondBalance(ctx, existing.UserID)
	if err != nil {
		return nil, err
	}
	return &CreateReservedResult{Creation: existing, Steps: steps, Reservation: reservation, DiamondBalanceAfter: balanceAfter}, nil
}

// newCreationFacts 用已冻结的归属构造创作与全部步骤。
// admissions 与 request.Plan 一一对应，由 resolveAdmissions 保证。
func newCreationFacts(request CreateReservedRequest, admissions []StepAdmission, fingerprint string, now time.Time) (*Creation, []CreationStep) {
	creationID := uuid.NewString()
	creation := &Creation{
		ID:                   creationID,
		IdempotencyKey:       request.IdempotencyKey,
		UserID:               request.UserID,
		TemplateID:           request.TemplateID,
		TemplateVersion:      request.TemplateVersion,
		Output:               request.Product.Output,
		VideoDurationSeconds: request.Product.Video.DurationSeconds,
		RequestFingerprint:   fingerprint,
		Status:               CreationStatusPendingSubmission,
		Version:              1,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	steps := make([]CreationStep, 0, len(request.Plan))
	for index, plan := range request.Plan {
		status := StepSubmitStatusReady
		if index > 0 {
			status = StepSubmitStatusBlocked
		}
		steps = append(steps, CreationStep{
			ID:           uuid.NewString(),
			CreationID:   creationID,
			Sequence:     plan.Sequence,
			Atom:         plan.Atom,
			Route:        admissions[index].Route,
			SubmitStatus: status,
			CreatedAt:    now,
		})
	}
	return creation, steps
}

func quotaReservationFor(product entitlement.GenerationRequest, benefits *entitlement.DailyBenefits) (ledger.QuotaReservation, error) {
	if benefits == nil || benefits.LocalDate == "" {
		return ledger.QuotaReservation{}, ErrInvalidCreateCommand
	}

	var kind string
	var limit, units int32
	switch product.Output {
	case entitlement.ProductOutputImage:
		kind = "vip_daily_image"
		limit = benefits.ImageLimit
		units = 1
	case entitlement.ProductOutputVideo:
		kind = "vip_daily_video"
		limit = benefits.VideoLimit
		units = product.Video.DurationSeconds / 5
	default:
		return ledger.QuotaReservation{}, ErrInvalidCreateCommand
	}

	// 非 VIP 没有日免上限，账本以零单位表示禁用日免并继续尝试条件扣钻。
	if limit == 0 {
		units = 0
	}
	return ledger.QuotaReservation{
		Kind:      kind,
		LocalDate: benefits.LocalDate,
		Limit:     limit,
		Units:     units,
	}, nil
}
