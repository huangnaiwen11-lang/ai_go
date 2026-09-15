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
)

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
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
