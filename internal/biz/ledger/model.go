package ledger

import "time"

// BenefitSource 表示预留所消耗的权益来源。
type BenefitSource string

const (
	// BenefitSourceDailyQuota 表示预留占用了用户本地日期内的日免额度。
	BenefitSourceDailyQuota BenefitSource = "daily_quota"
	// BenefitSourceDiamonds 表示预留预扣了用户自有钻石。
	BenefitSourceDiamonds BenefitSource = "diamonds"
)

// ReservationStatus 表示预留在生成流程中的终态或可处理状态。
type ReservationStatus string

const (
	// ReservationStatusReserved 表示预留已创建，等待生成或审核结果。
	ReservationStatusReserved ReservationStatus = "reserved"
	// ReservationStatusReversed 表示生成失败后已完成一次冲正。
	ReservationStatusReversed ReservationStatus = "reversed"
	// ReservationStatusConfiscated 表示审核未通过，预留被没收且不退款。
	ReservationStatusConfiscated ReservationStatus = "confiscated"
)

// ReversalReason 表示账本允许持久化的冲正规范原因。
type ReversalReason string

const (
	// ReversalReasonGenerationFailed 表示生成提交在已知失败路径中需要冲正。
	ReversalReasonGenerationFailed ReversalReason = "generation_failed"
	// ReversalReasonSubmissionRejected 表示生成提交被明确拒绝，供提交失败事务使用。
	ReversalReasonSubmissionRejected ReversalReason = "submission_rejected"
)

// QuotaReservation 是创建预留时冻结的日免额度上下文。
// 冲正必须使用该快照，不能按照当前日期或当前权益重新计算。
type QuotaReservation struct {
	Kind      string
	LocalDate string
	Limit     int32
	Units     int32
}

// ReserveRequest 是生成任务申请资金或日免预留的领域命令。
type ReserveRequest struct {
	CreationID    string
	UserID        string
	PriceDiamonds int64
	Quota         QuotaReservation
	// BusinessAt 必须在可重试事务开始前固定，由服务端编排层传入，不能信任客户端时间。
	BusinessAt time.Time
}

// Reservation 是一次生成任务的稳定资金预留事实。
type Reservation struct {
	ID              string
	CreationID      string
	UserID          string
	PriceDiamonds   int64
	Source          BenefitSource
	ChargedDiamonds int64
	Quota           QuotaReservation
	Status          ReservationStatus
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// LedgerEntry 是与预留状态变化对应的不可变账本分录。
type LedgerEntry struct {
	IdempotencyKey string
	CreationID     string
	DeltaDiamonds  int64
	Reason         string
	CreatedAt      time.Time
}
