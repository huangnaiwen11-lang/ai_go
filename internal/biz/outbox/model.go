// Package outbox 定义可靠投递所需的领域事件与仓储边界。
package outbox

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	generationSubmissionEventPrefix = "generation.submission:"
	maxEventIdentifierLength        = 512
	maxPayloadBytes                 = 1 << 20
)

const (
	// MaterialUploadRetryBudget 是素材搬运（下载 + 不可变写入 R2）允许的自动尝试次数。
	//
	// 口径来自 docs/对象存储_配置基线.md §6.2 冻结的「重试 5 次」。刻意不复用提交链路的
	// 重排规则：素材搬运不是向未知 provider POST，两者的退避基数与预算互不相干，
	// 混用会让素材重试跟着平台 Retry-After 漂移。
	MaterialUploadRetryBudget int32 = 5
	// MaxEventAge 是任何非终态事件允许停留的最长时间。超过后即使仍有剩余重试预算
	// 也必须转入人工关注，否则一个长期卡死的事件可以永远不被发现。
	MaxEventAge = 24 * time.Hour
	// MaxRedriveCount 是 V1 允许的人工重驱次数上限。上限本身就是目的：在自动化
	// 出口出现之前，不让「反复人工重驱」变成一条隐藏的无限重试通道。
	MaxRedriveCount int32 = 2
)

var (
	// ErrInvalidEvent 表示事件字段、租约或时间不满足领域约束。
	ErrInvalidEvent = errors.New("outbox: invalid event")
	// ErrInvalidTransition 表示事件当前状态不允许目标状态迁移。
	ErrInvalidTransition = errors.New("outbox: invalid event transition")
	// ErrEventAlreadyExists 表示稳定事件 ID 已经写入，调用方可按幂等成功处理。
	ErrEventAlreadyExists = errors.New("outbox: event already exists")
	// ErrLeaseConflict 表示条件更新未命中当前事件租约。
	ErrLeaseConflict = errors.New("outbox: event lease conflict")
)

// EventType 表示发件箱事件的固定业务类型。
type EventType string

const (
	// EventTypeGenerationSubmission 表示向生成中台提交单个步骤的事件。
	EventTypeGenerationSubmission EventType = "generation.submission"
	// RequeueLastError 是重新入队时唯一允许持久化的固定安全摘要。
	RequeueLastError = "outbox: retry_scheduled"
)

// DeliveryStatus 表示发件箱事件的投递生命周期。
type DeliveryStatus string

const (
	// DeliveryStatusPending 表示事件等待领取。
	DeliveryStatusPending DeliveryStatus = "pending"
	// DeliveryStatusDispatching 表示事件已被一个工作者持有租约。
	DeliveryStatusDispatching DeliveryStatus = "dispatching"
	// DeliveryStatusReconciling 表示事件需要重新核对投递结果。
	DeliveryStatusReconciling DeliveryStatus = "reconciling"
	// DeliveryStatusDelivered 表示事件已确认投递完成。
	DeliveryStatusDelivered DeliveryStatus = "delivered"
	// DeliveryStatusFailed 表示事件已不可重试地失败。
	DeliveryStatusFailed DeliveryStatus = "failed"
	// DeliveryStatusNeedsAttention 表示自动路径已放弃、但事实仍然有效的事件。
	//
	// 它不是另一种 failed：failed 意味着这件事不能再做，而 needs_attention 意味着
	// 「provider 已经确认完成，只是我们还没把成果搬进自有存储」，因此既不能自动退款，
	// 也不能把 provider 地址当作成果发布。它也不会回到 pending —— 可领取集合刻意
	// 不包含该状态，否则预算耗尽的事件会被无限重新领取，等于没有预算。
	//
	// 这里用独立状态而不是复用 failed，是因为两者的运营动作不同：failed 需要查因，
	// needs_attention 需要补传或人工重驱。也刻意不做成 failed 上的一个布尔位，
	// 否则仓储过滤与告警都得靠二级条件，容易漏。
	DeliveryStatusNeedsAttention DeliveryStatus = "needs_attention"
)

// AttentionReason 是事件转入人工关注时唯一允许持久化的固定安全摘要。
//
// 与 RequeueLastError 同族：只允许枚举值，不允许自由文本，避免把 provider 地址、
// 对象 key 或原始错误串写进可导出、可被运营浏览的文档。
type AttentionReason string

const (
	// AttentionReasonMaterialUploadBudget 表示素材搬运已经用尽基线冻结的自动重试预算。
	AttentionReasonMaterialUploadBudget AttentionReason = "provider_result_material_upload_budget_exhausted"
	// AttentionReasonEventAge 表示事件已经超过允许的非终态停留时间。它同样覆盖
	// 依赖类失败（读发布目标、写发布事务、解析冻结配方），这类失败不消耗素材预算。
	AttentionReasonEventAge AttentionReason = "provider_result_event_age_exceeded"
)

// ValidAttentionReason 报告给定的关注原因是否为已登记的枚举值。
func ValidAttentionReason(reason AttentionReason) bool {
	switch reason {
	case AttentionReasonMaterialUploadBudget, AttentionReasonEventAge:
		return true
	default:
		return false
	}
}

// Event 是不含存储实现细节的发件箱领域事件。
type Event struct {
	ID             string
	AggregateID    string
	EventType      EventType
	Payload        []byte
	DeliveryStatus DeliveryStatus
	AttemptCount   int32
	NextAttemptAt  time.Time
	LeaseToken     string
	LeaseUntil     time.Time
	LeaseOwner     string
	LastError      string
	// AttentionReason 只在 DeliveryStatus 为 needs_attention 时有值。它与
	// LastError 分开存放：Requeue 每次都会把 LastError 重写成固定摘要，
	// 塞在那里会把上一次的关注原因冲掉。
	AttentionReason AttentionReason
	// RedriveStartedAt 与 RedriveAttemptBase 共同构成「当前自动重试窗口」。
	// 人工重驱开启一个新窗口：预算从 RedriveAttemptBase 起算，年龄从
	// RedriveStartedAt 起算。二者都只被重驱改写。
	//
	// 不靠改写 AttemptCount / CreatedAt 来表达重驱：前者是唯一权威的尝试次数
	// 事实源（清零会让历史次数永久不可考），后者是事件的事实时间。
	RedriveStartedAt   time.Time
	RedriveAttemptBase int32
	// RedriveCount 单调递增、只增不减，是重驱次数上限与审计幂等键的事实源。
	RedriveCount int32
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SubmissionEventID 返回给定生成步骤唯一且稳定的提交事件 ID。
// 无效步骤 ID 返回空字符串，由 NewPending 统一拒绝。
func SubmissionEventID(stepID string) string {
	if !isValidStepID(stepID) {
		return ""
	}
	return generationSubmissionEventPrefix + stepID
}

// NewPending 创建一个等待投递的生成提交事件，并复制调用方提供的载荷。
func NewPending(id, aggregateID string, payload []byte, now time.Time) (*Event, error) {
	event := &Event{
		ID:             id,
		AggregateID:    aggregateID,
		EventType:      EventTypeGenerationSubmission,
		Payload:        append([]byte(nil), payload...),
		DeliveryStatus: DeliveryStatusPending,
		AttemptCount:   0,
		NextAttemptAt:  now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := ValidatePending(event); err != nil {
		return nil, err
	}
	return event, nil
}

// ValidatePending 校验待写入事件的固定类型、稳定 ID、原始 JSON 载荷及初始状态。
// Enqueue 也必须复用此校验，避免调用方直接构造 Event 绕过领域边界。
func ValidatePending(event *Event) error {
	if event == nil ||
		!isValidSubmissionEventID(event.ID) ||
		!isValidIdentifier(event.AggregateID) ||
		event.EventType != EventTypeGenerationSubmission ||
		event.DeliveryStatus != DeliveryStatusPending ||
		event.AttemptCount != 0 ||
		len(event.Payload) == 0 ||
		len(event.Payload) > maxPayloadBytes ||
		!json.Valid(event.Payload) ||
		event.NextAttemptAt.IsZero() ||
		event.LeaseToken != "" ||
		event.LeaseOwner != "" ||
		!event.LeaseUntil.IsZero() ||
		event.LastError != "" ||
		event.AttentionReason != "" ||
		event.CreatedAt.IsZero() ||
		event.UpdatedAt.IsZero() {
		return ErrInvalidEvent
	}
	return nil
}

// MarkDispatching 将待领取或待核对的事件变更为已领取状态。
func (event *Event) MarkDispatching(leaseToken, leaseOwner string, leaseUntil, now time.Time) error {
	if event == nil || (event.DeliveryStatus != DeliveryStatusPending && event.DeliveryStatus != DeliveryStatusReconciling) {
		return ErrInvalidTransition
	}
	if !isValidIdentifier(leaseToken) || !isValidIdentifier(leaseOwner) || now.IsZero() || !leaseUntil.After(now) {
		return ErrInvalidEvent
	}

	event.DeliveryStatus = DeliveryStatusDispatching
	event.LeaseToken = leaseToken
	event.LeaseOwner = leaseOwner
	event.LeaseUntil = leaseUntil
	event.AttemptCount++
	event.UpdatedAt = now
	return nil
}

// MarkDelivered 将已领取的事件结案为已投递并清除租约。
func (event *Event) MarkDelivered(now time.Time) error {
	if event == nil || event.DeliveryStatus != DeliveryStatusDispatching {
		return ErrInvalidTransition
	}
	if now.IsZero() {
		return ErrInvalidEvent
	}

	event.DeliveryStatus = DeliveryStatusDelivered
	event.clearLease()
	event.UpdatedAt = now
	return nil
}

// MarkFailed 将已领取的事件结案为失败并清除租约。
func (event *Event) MarkFailed(now time.Time) error {
	if event == nil || event.DeliveryStatus != DeliveryStatusDispatching {
		return ErrInvalidTransition
	}
	if now.IsZero() {
		return ErrInvalidEvent
	}

	event.DeliveryStatus = DeliveryStatusFailed
	event.clearLease()
	event.UpdatedAt = now
	return nil
}

// MarkNeedsAttention 将已领取的事件结案为人工关注并清除租约。
//
// 与 MarkFailed 的差别是运营含义而不是机制：failed 表示这件事不能再做，
// needs_attention 表示事实仍然有效、只是自动路径放弃了，所以调用方不得据此退款
// 或发布半成品。原因必须是已登记的枚举值，不能是自由文本。
func (event *Event) MarkNeedsAttention(reason AttentionReason, now time.Time) error {
	if event == nil || event.DeliveryStatus != DeliveryStatusDispatching {
		return ErrInvalidTransition
	}
	if now.IsZero() || !ValidAttentionReason(reason) {
		return ErrInvalidEvent
	}

	event.DeliveryStatus = DeliveryStatusNeedsAttention
	event.AttentionReason = reason
	event.clearLease()
	event.UpdatedAt = now
	return nil
}

// RetryBudgetExhausted 报告一次已领取事件是否已经用完给定的自动尝试预算。
//
// attempt 取自已领取事件的 AttemptCount：它在领取时自增、重新入队时不重置，
// 是唯一权威的尝试次数事实源，不要另建计数器。
func RetryBudgetExhausted(attempt, budget int32) bool {
	return budget > 0 && attempt >= budget
}

// EventAgeExceeded 报告事件是否已经超过允许的非终态停留时间。
func EventAgeExceeded(createdAt, now time.Time) bool {
	return !createdAt.IsZero() && !now.IsZero() && now.Sub(createdAt) >= MaxEventAge
}

// AttentionWindow 返回当前自动重试窗口的起点与起始尝试数。
//
// 从未重驱过的窗口就是「建单即窗口」：起点 CreatedAt、基数 0，因此不重驱的
// 事件与引入重驱之前的行为逐位一致。人工重驱会推进窗口，使事件重新获得一份
// 完整预算与一段完整年龄 —— 否则重驱后第一次失败就会立刻再次进入人工关注，
// 等于重驱这个能力在语义上是空的。
//
// 所有终态判定都必须走这里，不要在调用点各自拼 CreatedAt / AttemptCount：
// 只要有一个调用点忘了减 attemptBase，被重驱过的事件就会立刻再次终态。
func (event *Event) AttentionWindow() (startedAt time.Time, attemptBase int32) {
	if event == nil {
		return time.Time{}, 0
	}
	if event.RedriveStartedAt.IsZero() {
		return event.CreatedAt, 0
	}
	if event.RedriveStartedAt.After(event.CreatedAt) {
		return event.RedriveStartedAt, event.RedriveAttemptBase
	}
	// 窗口起点早于建单时间只可能来自时钟回拨；以建单时间为准，但保留基数，
	// 避免把已经推进过的窗口退回去。
	return event.CreatedAt, event.RedriveAttemptBase
}

// RedriveEligible 报告事件是否还能被人工重驱。
func (event *Event) RedriveEligible() bool {
	return event != nil && event.DeliveryStatus == DeliveryStatusNeedsAttention && event.RedriveCount < MaxRedriveCount
}

// Requeue 将已领取的事件放回等待队列并写入固定安全摘要。
func (event *Event) Requeue(nextAttemptAt time.Time) error {
	if event == nil || event.DeliveryStatus != DeliveryStatusDispatching {
		return ErrInvalidTransition
	}
	if nextAttemptAt.IsZero() {
		return ErrInvalidEvent
	}

	event.DeliveryStatus = DeliveryStatusPending
	event.NextAttemptAt = nextAttemptAt
	event.LastError = RequeueLastError
	event.clearLease()
	event.UpdatedAt = nextAttemptAt
	return nil
}

func (event *Event) clearLease() {
	event.LeaseToken = ""
	event.LeaseOwner = ""
	event.LeaseUntil = time.Time{}
}

func isValidIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= maxEventIdentifierLength
}

func isValidSubmissionEventID(eventID string) bool {
	if !strings.HasPrefix(eventID, generationSubmissionEventPrefix) {
		return false
	}
	stepID := strings.TrimPrefix(eventID, generationSubmissionEventPrefix)
	return SubmissionEventID(stepID) == eventID
}

func isValidStepID(stepID string) bool {
	if stepID == "" || len(stepID)+len(generationSubmissionEventPrefix) > maxEventIdentifierLength {
		return false
	}
	for index := range stepID {
		character := stepID[index]
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '-' && character != '_' {
			return false
		}
	}
	return true
}
