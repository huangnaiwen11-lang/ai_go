package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	ProviderInboxSource      = "polarstar.b2b.v2"
	providerInboxMaxPayload  = 1 << 20
	providerInboxMaxIdentity = 200
)

// ProviderReconcileEventType 是 B2B 对账恢复事件的固定类型。事件 ID 前缀与
// 事件类型必须来自同一处定义，否则两处字面量一旦漂移，终态写入就再也
// 匹配不上自己创建的对账事件。
const ProviderReconcileEventType = "generation.reconcile"

// ProviderInboxConsumeEventType 是「消费一条已验签 delivery」的恢复事件类型。
// 它按 receipt 身份去重，与按步骤去重的 reconcile 事件是两个不同粒度的
// 工作项：同一步骤可以收到多条 delivery，每条都必须各自可被消费一次。
const ProviderInboxConsumeEventType = "generation.inbox.consume"

// ProviderReconcileEventPayload 是按步骤去重的 recovery event 的唯一载荷形状。
// 同一事件可能由提交意图、回调 ACK、任务绑定和终态 CAS 共同读写；因此不能让
// 每个生产者私自定义自己的 payload 或 aggregate 语义。
type ProviderReconcileEventPayload struct {
	CreationID string `json:"creationId"`
	StepID     string `json:"stepId"`
}

// MarshalProviderReconcileEventPayload 构造恢复事件的规范载荷。步骤 ID 同时参与
// event ID，CreationID 则是 aggregate 身份；两者均须稳定且不能从回调报文猜测。
func MarshalProviderReconcileEventPayload(creationID, stepID string) ([]byte, error) {
	if !providerInboxStableIdentity(creationID) || ProviderInboxRecoveryEventID(stepID) == "" {
		return nil, ErrInvalidProviderInboxRecord
	}
	return json.Marshal(ProviderReconcileEventPayload{CreationID: creationID, StepID: stepID})
}

var (
	ErrInvalidProviderInboxRecord = errors.New("generation: invalid provider inbox record")
	ErrProviderInboxConflict      = errors.New("generation: provider inbox conflict")
)

// ProviderInboxStatus is the local lifecycle of a verified B2B delivery fact.
// Persistence and callback transport are deliberately outside this model.
type ProviderInboxStatus string

const (
	ProviderInboxPending     ProviderInboxStatus = "pending"
	ProviderInboxApplied     ProviderInboxStatus = "applied"
	ProviderInboxQuarantined ProviderInboxStatus = "quarantined"
)

// ProviderInboxApplyResult describes a pure merge decision for one delivery key.
type ProviderInboxApplyResult string

const (
	InboxApplyInserted    ProviderInboxApplyResult = "inserted"
	InboxApplyNoop        ProviderInboxApplyResult = "noop"
	InboxApplyQuarantined ProviderInboxApplyResult = "quarantined"
)

// ProviderInboxStore 将已验签 delivery 与恢复事件作为一个事务边界写入。
// 实现必须要求外层 Mongo transaction；ACK 只能在该方法成功后返回。
type ProviderInboxStore interface {
	ApplyAndSchedule(context.Context, ProviderInboxRecord) (ProviderInboxApplyResult, error)
}

// ProviderInboxRecoveryEventID 是按步骤去重的恢复事件 ID。多个 delivery
// 可能对应同一终态，故对账队列只需要唤醒一次，具体事实从 inbox 读取。
func ProviderInboxRecoveryEventID(stepID string) string {
	if !providerInboxStableIdentity(stepID) {
		return ""
	}
	return ProviderReconcileEventType + ":" + stepID
}

// ProviderInboxEventID 是按 receipt 身份去重的 delivery 消费事件 ID。
// 两条 delivery 即使属于同一步骤也必须拿到不同事件，否则先到的 ACK 会让
// 后到的 delivery 无处可消费。
func ProviderInboxEventID(source, accountRef, deliveryID string) string {
	if !providerInboxStableIdentity(source) || !providerInboxStableIdentity(accountRef) || !providerInboxStableIdentity(deliveryID) {
		return ""
	}
	return ProviderInboxConsumeEventType + ":" + ProviderInboxDeliveryDigest(source, accountRef, deliveryID)
}

// ProviderInboxDeliveryDigest 是 receipt 身份（source/account/delivery）的稳定
// 摘要。inbox 文档 ID 与消费事件 ID 都由它派生，避免两处各算一遍摘要而漂移。
func ProviderInboxDeliveryDigest(source, accountRef, deliveryID string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + accountRef + "\x00" + deliveryID))
	return hex.EncodeToString(sum[:])
}

// ProviderInboxRecord stores only bounded, already-verified delivery facts.
// PayloadDigest identifies the exact raw payload; TerminalDigest identifies the
// separately normalized business terminal summary and may be empty while pending.
type ProviderInboxRecord struct {
	Source         string
	AccountRef     string
	DeliveryID     string
	StepID         string
	JobID          string
	TerminalDigest string
	PayloadDigest  string
	Payload        []byte
	Status         ProviderInboxStatus
	Attempts       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewProviderInboxRecord constructs the only supported initial state.
// The source is fixed so callers cannot accidentally mix old callback protocols.
func NewProviderInboxRecord(accountRef, deliveryID, stepID, jobID, terminalDigest string, payload []byte, attempts int, at time.Time) (ProviderInboxRecord, error) {
	if at.IsZero() {
		return ProviderInboxRecord{}, ErrInvalidProviderInboxRecord
	}
	now := at.UTC()
	record := ProviderInboxRecord{
		Source: ProviderInboxSource, AccountRef: accountRef, DeliveryID: deliveryID, StepID: stepID, JobID: jobID,
		TerminalDigest: terminalDigest, PayloadDigest: providerInboxPayloadDigest(payload), Payload: append([]byte(nil), payload...),
		Status: ProviderInboxPending, Attempts: attempts, CreatedAt: now, UpdatedAt: now,
	}
	if err := record.Validate(); err != nil {
		return ProviderInboxRecord{}, err
	}
	return record, nil
}

func (record ProviderInboxRecord) Validate() error {
	if record.Source != ProviderInboxSource || !providerInboxStableIdentity(record.AccountRef) ||
		!providerInboxStableIdentity(record.DeliveryID) || !providerInboxStableIdentity(record.StepID) || !providerInboxStableIdentity(record.JobID) ||
		record.Attempts <= 0 || len(record.Payload) == 0 || len(record.Payload) > providerInboxMaxPayload ||
		!providerInboxDigest(record.PayloadDigest) || providerInboxPayloadDigest(record.Payload) != record.PayloadDigest ||
		(record.TerminalDigest != "" && !providerInboxDigest(record.TerminalDigest)) ||
		(record.Status != ProviderInboxPending && record.Status != ProviderInboxApplied && record.Status != ProviderInboxQuarantined) ||
		record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) {
		return ErrInvalidProviderInboxRecord
	}
	if record.Status == ProviderInboxApplied && record.TerminalDigest == "" {
		return ErrInvalidProviderInboxRecord
	}
	return nil
}

// ValidateIncomingDelivery 校验一条入站 delivery 是否有资格进入本地 inbox。
// 入站只允许 pending：applied/quarantined 是本地持久化侧的单向转换结果，
// 供应商永远无权声明自己已经应用过。允许入站携带生命周期状态会让一次
// 重放把已应用的 delivery 重新拉回待处理。落库与合并两个入口都必须走这里。
func (record ProviderInboxRecord) ValidateIncomingDelivery() error {
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Status != ProviderInboxPending {
		return ErrInvalidProviderInboxRecord
	}
	return nil
}

// ApplyProviderInbox merges an incoming delivery into an existing delivery key.
// Equal identity and raw bytes are a no-op; any difference is
// quarantined in the returned record. Inputs and payloads are never aliased.
func ApplyProviderInbox(existing *ProviderInboxRecord, incoming ProviderInboxRecord) (ProviderInboxRecord, ProviderInboxApplyResult, error) {
	if err := incoming.ValidateIncomingDelivery(); err != nil {
		return ProviderInboxRecord{}, "", err
	}
	if existing == nil {
		incoming.Payload = append([]byte(nil), incoming.Payload...)
		return incoming, InboxApplyInserted, nil
	}
	if err := existing.Validate(); err != nil {
		return ProviderInboxRecord{}, "", err
	}
	current := cloneProviderInbox(*existing)
	if !providerInboxSameIdentity(current, incoming) || current.PayloadDigest != incoming.PayloadDigest ||
		!bytes.Equal(current.Payload, incoming.Payload) {
		current.Status = ProviderInboxQuarantined
		if incoming.UpdatedAt.After(current.UpdatedAt) {
			current.UpdatedAt = incoming.UpdatedAt
		}
		return current, InboxApplyQuarantined, ErrProviderInboxConflict
	}
	return current, InboxApplyNoop, nil
}

// Apply is the value-oriented form used by pure domain callers that already
// hold one immutable snapshot of the existing record.
func (record ProviderInboxRecord) Apply(incoming ProviderInboxRecord) (ProviderInboxRecord, ProviderInboxApplyResult, error) {
	return ApplyProviderInbox(&record, incoming)
}

func providerInboxSameIdentity(a, b ProviderInboxRecord) bool {
	return a.Source == b.Source && a.AccountRef == b.AccountRef && a.DeliveryID == b.DeliveryID && a.StepID == b.StepID && a.JobID == b.JobID
}

func cloneProviderInbox(record ProviderInboxRecord) ProviderInboxRecord {
	record.Payload = append([]byte(nil), record.Payload...)
	return record
}

func providerInboxStableIdentity(value string) bool {
	if value == "" || len(value) > providerInboxMaxIdentity || strings.TrimSpace(value) != value {
		return false
	}
	for index, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			continue
		}
		if index == 0 || !strings.ContainsRune("._:-", char) {
			return false
		}
	}
	return true
}

func providerInboxDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func providerInboxPayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
