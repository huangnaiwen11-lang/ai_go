package ledger

import (
	"context"
	"time"

	"ai-business-service/internal/biz/shared"
)

// reservationReason 固定表示生成任务已成功建立预留，区别于调用方传入的冲正原因。
const reservationReason = "generation_reserved"

// Usecase 编排预留、冲正和没收的领域状态机。
type Usecase struct {
	repository Repository
	tx         shared.TxRunner
	now        func() time.Time
}

// NewUsecase 创建账本领域用例。
func NewUsecase(repository Repository, tx shared.TxRunner) *Usecase {
	return NewUsecaseWithClock(repository, tx, time.Now)
}

// NewUsecaseWithClock 创建使用调用方提供时钟的账本用例，供可重试事务冻结业务时间。
func NewUsecaseWithClock(repository Repository, tx shared.TxRunner, now func() time.Time) *Usecase {
	if now == nil {
		now = time.Now
	}
	return &Usecase{repository: repository, tx: tx, now: now}
}

// Reserve 在同一事务中优先占用日免，日免不足时再原子预扣钻石。
func (usecase *Usecase) Reserve(ctx context.Context, request ReserveRequest) (*Reservation, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	if request.CreationID == "" {
		return nil, ErrInvalidReservationCommand
	}
	// 事务回调可能被重试；必须在进入回调前冻结一次业务时刻。
	if request.BusinessAt.IsZero() {
		request.BusinessAt = usecase.nowUTC()
	} else {
		request.BusinessAt = request.BusinessAt.UTC()
	}

	var reservation *Reservation
	err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		reservation, err = usecase.ReserveInTx(txCtx, request)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

// ReserveInTx 在调用方已经开启的事务中写入预留事实。
// 调用方必须把同一事务上下文传给创作、账本和其他需要原子提交的写入操作。
func (usecase *Usecase) ReserveInTx(ctx context.Context, request ReserveRequest) (*Reservation, error) {
	if usecase == nil || usecase.repository == nil {
		return nil, ErrLedgerDependenciesUnavailable
	}
	if request.CreationID == "" {
		return nil, ErrInvalidReservationCommand
	}
	if request.BusinessAt.IsZero() {
		return nil, ErrInvalidReservationCommand
	}
	request.BusinessAt = request.BusinessAt.UTC()
	return usecase.reserveInTx(ctx, request)
}

// FindReservation 读取已经持久化的预留事实，供相同幂等键重放时恢复原始结算投影。
func (usecase *Usecase) FindReservation(ctx context.Context, creationID string) (*Reservation, error) {
	if usecase == nil || usecase.repository == nil || creationID == "" {
		return nil, ErrInvalidReservationCommand
	}
	return usecase.repository.FindReservation(ctx, creationID)
}

// CurrentDiamondBalance 在预留已经完成后读取当前事务可见的自有账户余额。
// 它绝不能用于创建前的余额判断，创建是否允许仍由条件预扣保证并发安全。
func (usecase *Usecase) CurrentDiamondBalance(ctx context.Context, userID string) (int64, error) {
	if usecase == nil || usecase.repository == nil || userID == "" {
		return 0, ErrInvalidReservationCommand
	}
	return usecase.repository.FindDiamondBalance(ctx, userID)
}

func (usecase *Usecase) reserveInTx(ctx context.Context, request ReserveRequest) (*Reservation, error) {
	existing, err := usecase.repository.FindReservation(ctx, request.CreationID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		// 重试必须携带完全相同的业务命令，避免不同请求被错误收敛为成功。
		if !sameReservationCommand(existing, request) {
			return nil, ErrReservationCommandConflict
		}
		return existing, nil
	}
	if err := validateReserveRequest(request); err != nil {
		return nil, err
	}

	consumedQuota := false
	if !quotaDisabled(request.Quota) {
		consumedQuota, err = usecase.repository.TryConsumeQuota(ctx, request.UserID, request.Quota, request.BusinessAt)
		if err != nil {
			return nil, err
		}
	}

	source := BenefitSourceDailyQuota
	chargedDiamonds := int64(0)
	if !consumedQuota {
		debited, err := usecase.repository.TryDebitDiamonds(ctx, request.UserID, request.PriceDiamonds, request.BusinessAt)
		if err != nil {
			return nil, err
		}
		if !debited {
			return nil, shared.ErrInsufficientFunds
		}
		source = BenefitSourceDiamonds
		chargedDiamonds = request.PriceDiamonds
	}

	reservation := &Reservation{
		ID:              reservationID(request.CreationID),
		CreationID:      request.CreationID,
		UserID:          request.UserID,
		PriceDiamonds:   request.PriceDiamonds,
		Source:          source,
		ChargedDiamonds: chargedDiamonds,
		Quota:           request.Quota,
		Status:          ReservationStatusReserved,
		CreatedAt:       request.BusinessAt,
		UpdatedAt:       request.BusinessAt,
	}
	if err := usecase.repository.CreateReservation(ctx, reservation); err != nil {
		return nil, err
	}
	if err := usecase.repository.AppendLedgerEntry(ctx, &LedgerEntry{
		IdempotencyKey: reserveLedgerKey(request.CreationID),
		CreationID:     request.CreationID,
		DeltaDiamonds:  -chargedDiamonds,
		Reason:         reservationReason,
		CreatedAt:      request.BusinessAt,
	}); err != nil {
		return nil, err
	}
	return reservation, nil
}

// Reverse 对失败的生成预留执行一次冲正。
func (usecase *Usecase) Reverse(ctx context.Context, creationID string, reason ReversalReason) (*Reservation, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	if creationID == "" || !isAllowedReversalReason(reason) {
		return nil, ErrInvalidReservationCommand
	}
	// 冲正分录也处于可重试事务中，因此在进入事务前固定其创建时刻。
	businessAt := usecase.nowUTC()

	var reservation *Reservation
	err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		reservation, err = usecase.ReverseInTx(txCtx, creationID, reason, businessAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

// ReverseInTx 在调用方已经开启的事务中完成一次预留冲正。
// 调用方必须使用相同事务上下文串联创作状态、账本和发件箱状态变更。
func (usecase *Usecase) ReverseInTx(ctx context.Context, creationID string, reason ReversalReason, businessAt time.Time) (*Reservation, error) {
	if usecase == nil || usecase.repository == nil {
		return nil, ErrLedgerDependenciesUnavailable
	}
	if creationID == "" || !isAllowedReversalReason(reason) || businessAt.IsZero() {
		return nil, ErrInvalidReservationCommand
	}
	businessAt = businessAt.UTC()

	found, err := usecase.repository.FindReservation(ctx, creationID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrReservationNotFound
	}
	switch found.Status {
	case ReservationStatusReversed:
		return found, nil
	case ReservationStatusConfiscated:
		return nil, ErrReservationStateConflict
	case ReservationStatusReserved:
	default:
		return nil, ErrReservationStateConflict
	}

	// 先做条件迁移；未命中时绝不退款，防止并发重试造成双冲正。
	transitioned, err := usecase.repository.TransitionReservation(
		ctx,
		creationID,
		ReservationStatusReserved,
		ReservationStatusReversed,
		businessAt,
	)
	if err != nil {
		return nil, err
	}
	if !transitioned {
		return nil, ErrReservationStateConflict
	}

	delta := int64(0)
	switch found.Source {
	case BenefitSourceDiamonds:
		delta = found.ChargedDiamonds
		if err := usecase.repository.CreditDiamonds(ctx, found.UserID, delta, businessAt); err != nil {
			return nil, err
		}
	case BenefitSourceDailyQuota:
		if err := usecase.repository.RestoreQuota(ctx, found.UserID, found.Quota, businessAt); err != nil {
			return nil, err
		}
	default:
		return nil, ErrReservationStateConflict
	}
	if err := usecase.repository.AppendLedgerEntry(ctx, &LedgerEntry{
		IdempotencyKey: reverseLedgerKey(creationID),
		CreationID:     creationID,
		DeltaDiamonds:  delta,
		Reason:         string(reason),
		CreatedAt:      businessAt,
	}); err != nil {
		return nil, err
	}
	found.Status = ReservationStatusReversed
	return found, nil
}

// Confiscate 将审核未通过的预留迁移为没收，不产生退款或额度释放。
func (usecase *Usecase) Confiscate(ctx context.Context, creationID, reason string) (*Reservation, error) {
	if err := usecase.ready(); err != nil {
		return nil, err
	}
	if creationID == "" || reason == "" {
		return nil, ErrInvalidReservationCommand
	}

	var reservation *Reservation
	err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		reservation, err = usecase.ConfiscateInTx(txCtx, creationID, reason, usecase.nowUTC())
		return err
	})
	if err != nil {
		return nil, err
	}
	return reservation, nil
}

// ConfiscateInTx 在调用方已开启的事务中没收审核拒绝的预留。
// 它仅允许 reserved 到 confiscated 的条件迁移，不退款、不归还日免，也不追加资金分录。
func (usecase *Usecase) ConfiscateInTx(ctx context.Context, creationID, reason string, businessAt time.Time) (*Reservation, error) {
	if usecase == nil || usecase.repository == nil {
		return nil, ErrLedgerDependenciesUnavailable
	}
	if creationID == "" || reason == "" || businessAt.IsZero() {
		return nil, ErrInvalidReservationCommand
	}
	businessAt = businessAt.UTC()

	found, err := usecase.repository.FindReservation(ctx, creationID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrReservationNotFound
	}
	if found.Status != ReservationStatusReserved {
		return nil, ErrReservationStateConflict
	}
	transitioned, err := usecase.repository.TransitionReservation(
		ctx,
		creationID,
		ReservationStatusReserved,
		ReservationStatusConfiscated,
		businessAt,
	)
	if err != nil {
		return nil, err
	}
	if !transitioned {
		return nil, ErrReservationStateConflict
	}
	found.Status = ReservationStatusConfiscated
	return found, nil
}

func (usecase *Usecase) nowUTC() time.Time {
	if usecase == nil || usecase.now == nil {
		return time.Now().UTC()
	}
	return usecase.now().UTC()
}

func (usecase *Usecase) ready() error {
	if usecase == nil || usecase.repository == nil || usecase.tx == nil {
		return ErrLedgerDependenciesUnavailable
	}
	return nil
}

func validateReserveRequest(request ReserveRequest) error {
	if request.CreationID == "" || request.UserID == "" || request.PriceDiamonds <= 0 || request.BusinessAt.IsZero() {
		return ErrInvalidReservationCommand
	}
	if request.Quota.Kind == "" || request.Quota.LocalDate == "" {
		return ErrInvalidReservationCommand
	}
	if quotaDisabled(request.Quota) {
		return nil
	}
	if request.Quota.Limit <= 0 || request.Quota.Units <= 0 || request.Quota.Units > request.Quota.Limit {
		return ErrInvalidReservationCommand
	}
	return nil
}

func quotaDisabled(quota QuotaReservation) bool {
	return quota.Limit == 0 && quota.Units == 0
}

func sameReservationCommand(reservation *Reservation, request ReserveRequest) bool {
	return reservation.UserID == request.UserID &&
		reservation.PriceDiamonds == request.PriceDiamonds &&
		reservation.Quota.Kind == request.Quota.Kind &&
		reservation.Quota.LocalDate == request.Quota.LocalDate &&
		reservation.Quota.Limit == request.Quota.Limit &&
		reservation.Quota.Units == request.Quota.Units
}

func reservationID(creationID string) string {
	return "reservation:" + creationID
}

func reserveLedgerKey(creationID string) string {
	return "reserve:" + creationID
}

func reverseLedgerKey(creationID string) string {
	return "reverse:" + creationID
}

func isAllowedReversalReason(reason ReversalReason) bool {
	switch reason {
	case ReversalReasonGenerationFailed, ReversalReasonSubmissionRejected:
		return true
	default:
		return false
	}
}
