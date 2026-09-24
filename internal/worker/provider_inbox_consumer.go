package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
)

const defaultInboxConsumerWorkerID = "local-provider-inbox-consumer-worker"

var (
	// ErrInboxConsumerWorkerDependenciesUnavailable 表示工作者未被完整装配。
	ErrInboxConsumerWorkerDependenciesUnavailable = errors.New("provider inbox consumer worker dependencies are unavailable")
	// ErrInvalidInboxConsumeEvent 表示工作项载荷与它声称的投递事实不符。
	//
	// 它与「投递不存在」区分开：前者说明工作项本身是坏的，后者说明工作项指向
	// 的事实已经被清理。两者都不该重排，但原因不同，运维要看得出区别。
	ErrInvalidInboxConsumeEvent = errors.New("provider inbox consumer worker: invalid consume event")
)

// ProviderInboxRecordReader 是读回投递原文的最小边界。
//
// 它与消费用例分开声明：Worker 必须先拿到原始字节才能重新解析终态语义，而
// 用例只关心归一化后的结论。生产实现是 data.mongoGenerationProviderInboxConsumerStore。
type ProviderInboxRecordReader interface {
	ReadProviderInbox(context.Context, generation.ProviderInboxKey) (*generation.ProviderInboxRecord, error)
}

// ProviderInboxTerminalConsumer 是终态消费用例的最小边界。
// 生产实现是 generation.ProviderInboxConsumerUsecase。
type ProviderInboxTerminalConsumer interface {
	Consume(context.Context, generation.ProviderInboxKey, generation.ProviderTerminalObservation) (generation.ProviderTerminalApplyResult, error)
}

// ProviderInboxObservationParser 从已持久化的投递事实重新解析归一化终态。
//
// 它是协议层的职责，不属于领域：领域只接受归一化结论，只有协议层知道
// b2b.callback.v2 的字段布局。生产实现是 platform.B2BCallbackObservationParser。
type ProviderInboxObservationParser interface {
	Parse(*generation.ProviderInboxRecord) (generation.ProviderTerminalObservation, error)
}

// providerInboxConsumePayload 是 generation.inbox.consume 事件的固定载荷形状。
//
// 它与 data 层写入时的结构体逐字段对应：任一侧改字段都必须同时改另一侧，
// 否则消费侧会读到一份字段齐全但语义为空的载荷。
type providerInboxConsumePayload struct {
	Source         string `json:"source"`
	AccountRef     string `json:"accountRef"`
	DeliveryID     string `json:"deliveryId"`
	StepID         string `json:"stepId"`
	JobID          string `json:"jobId"`
	PayloadDigest  string `json:"payloadDigest"`
	TerminalDigest string `json:"terminalDigest"`
}

// ProviderInboxConsumerWorker 消费一条已 ACK 的 B2B 投递。
//
// 它是 ACK 与终态之间唯一的执行体：没有它，投递只会停在 pending，创作永远停在
// submitted，用户预扣的钻石也永远不结算。
//
// 它不做终态 CAS——那是 ProviderInboxConsumerUsecase 的职责；这里只负责
// 「领取工作项 → 读回原文 → 重新解析 → 调用用例 → 结案」，因此重排策略
// （重试 / 放弃）集中在这一层，不会渗进领域。
type ProviderInboxConsumerWorker struct {
	outbox   outbox.Repository
	inbox    ProviderInboxRecordReader
	consumer ProviderInboxTerminalConsumer
	parse    ProviderInboxObservationParser
	workerID string
	now      func() time.Time
}

// NewProviderInboxConsumerWorker 构造一次性投递消费者。
// 该构造器不领取事件、不写库、也不发起任何网络请求。
func NewProviderInboxConsumerWorker(
	outboxRepository outbox.Repository,
	inbox ProviderInboxRecordReader,
	consumer ProviderInboxTerminalConsumer,
	parse ProviderInboxObservationParser,
	workerID string,
	now func() time.Time,
) *ProviderInboxConsumerWorker {
	if now == nil {
		now = time.Now
	}
	return &ProviderInboxConsumerWorker{
		outbox: outboxRepository, inbox: inbox, consumer: consumer, parse: parse,
		workerID: workerID, now: now,
	}
}

// NewDefaultProviderInboxConsumerWorker 提供组合根使用的默认装配。
//
// 它刻意不进 `ProviderSet`：B2B 投递消费者只在显式启动的 Worker 进程里运行，
// 而 HTTP 组合图（ai-business-service）从不调用 DeliverOnce。放进去会让 Wire
// 图多出一个永不执行的依赖，也会把协议解析器带进 HTTP 进程。
func NewDefaultProviderInboxConsumerWorker(
	outboxRepository outbox.Repository,
	inbox ProviderInboxRecordReader,
	consumer ProviderInboxTerminalConsumer,
	parse ProviderInboxObservationParser,
) *ProviderInboxConsumerWorker {
	return NewProviderInboxConsumerWorker(outboxRepository, inbox, consumer, parse, defaultInboxConsumerWorkerID, time.Now)
}

// DeliverOnce 领取一个投递消费工作项并处理一次。
//
// 结案语义固定为三类，直接决定发件箱里的可观测状态：
//   - 已应用 / 已幂等重放：工作项投递完成（delivered）；
//   - 完整性冲突（隔离）或事实矛盾：工作项不可重试地失败（failed），
//     证据保留在 inbox 记录与步骤终态槽位里；
//   - 存储或事务抖动：按退避重排（pending）。
func (worker *ProviderInboxConsumerWorker) DeliverOnce(ctx context.Context, expectedEventID string) error {
	if err := worker.ready(); err != nil {
		return err
	}
	now := worker.nowUTC()
	eventType := outbox.EventType(generation.ProviderInboxConsumeEventType)
	var event *outbox.Event
	var err error
	if expectedEventID == "" {
		event, err = worker.outbox.ClaimByType(ctx, worker.workerID, eventType, now, now.Add(defaultLeaseDuration))
	} else {
		// 指定投递必须在数据库条件更新中锁定目标 ID，禁止先租约其他事件再归还。
		event, err = worker.outbox.ClaimByIDAndType(ctx, worker.workerID, expectedEventID, eventType, now, now.Add(defaultLeaseDuration))
	}
	if err != nil || event == nil {
		return err
	}
	if event.EventType != eventType {
		return ErrUnexpectedClaimedEvent
	}
	if expectedEventID != "" && event.ID != expectedEventID {
		return ErrUnexpectedClaimedEvent
	}

	payload, err := decodeProviderInboxConsumePayload(event)
	if err != nil {
		return worker.abandon(ctx, event)
	}
	key := generation.ProviderInboxKey{Source: payload.Source, AccountRef: payload.AccountRef, DeliveryID: payload.DeliveryID}
	record, err := worker.inbox.ReadProviderInbox(ctx, key)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	if record == nil {
		// 工作项存在但投递不存在：说明事件与事实被拆开了（例如手工清理）。
		// 重排不会让它出现，必须显式失败而不是静默结案。
		return worker.abandon(ctx, event)
	}
	if err := validateConsumePayloadAgainstRecord(payload, record); err != nil {
		return worker.abandon(ctx, event)
	}
	observation, err := worker.parse.Parse(record)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	result, err := worker.consumer.Consume(ctx, key, observation)
	if err != nil {
		return worker.retryOrAbandon(ctx, event, err)
	}
	if result == generation.ProviderTerminalQuarantined {
		// 冲突证据已经由仓储写入，重排不会改变结论。工作项以失败结案，
		// 让「有一条投递没能收敛」在发件箱里可见，而不是记成正常投递。
		return worker.abandon(ctx, event)
	}
	return worker.deliver(ctx, event)
}

// retryOrAbandon 按失败是否可重试决定重排还是放弃。
func (worker *ProviderInboxConsumerWorker) retryOrAbandon(ctx context.Context, event *outbox.Event, cause error) error {
	if providerTerminalPermanentFailure(cause) {
		return worker.abandon(ctx, event)
	}
	return worker.requeue(ctx, event, cause)
}

func (worker *ProviderInboxConsumerWorker) deliver(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkDelivered(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderInboxConsumerWorker) abandon(ctx context.Context, event *outbox.Event) error {
	return settleOutboxError(worker.outbox.MarkFailed(ctx, event.ID, event.LeaseToken, worker.nowUTC()))
}

func (worker *ProviderInboxConsumerWorker) requeue(ctx context.Context, event *outbox.Event, cause error) error {
	now := worker.nowUTC()
	nextAttemptAt := nextSubmissionAttempt(now, event.AttemptCount, cause)
	return settleOutboxError(worker.outbox.Requeue(ctx, event.ID, event.LeaseToken, nextAttemptAt))
}

// settleOutboxError 把租约冲突视为「另一份工作项正在处理同一条投递」。
//
// 它不是故障：并发的消费者已经接管了这条事件，这里重复上抛只会让轮询进程
// 因为一次正常的租约交接而整体退出。
func settleOutboxError(err error) error {
	if errors.Is(err, outbox.ErrLeaseConflict) {
		return nil
	}
	return err
}

func decodeProviderInboxConsumePayload(event *outbox.Event) (providerInboxConsumePayload, error) {
	if event == nil {
		return providerInboxConsumePayload{}, ErrInvalidInboxConsumeEvent
	}
	var payload providerInboxConsumePayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return providerInboxConsumePayload{}, ErrInvalidInboxConsumeEvent
	}
	key := generation.ProviderInboxKey{Source: payload.Source, AccountRef: payload.AccountRef, DeliveryID: payload.DeliveryID}
	if err := key.Validate(); err != nil {
		return providerInboxConsumePayload{}, ErrInvalidInboxConsumeEvent
	}
	if payload.StepID == "" || payload.JobID == "" || payload.PayloadDigest == "" {
		return providerInboxConsumePayload{}, ErrInvalidInboxConsumeEvent
	}
	return payload, nil
}

// validateConsumePayloadAgainstRecord 核对工作项与它指向的投递事实。
//
// 键相同只能保证读到同一条记录，不能保证载荷没有被改写。这里逐字段比对
// ACK 时冻结的身份与摘要；TerminalDigest 不参与——它会在消费成功后变化。
func validateConsumePayloadAgainstRecord(payload providerInboxConsumePayload, record *generation.ProviderInboxRecord) error {
	if payload.Source != record.Source || payload.AccountRef != record.AccountRef || payload.DeliveryID != record.DeliveryID ||
		payload.StepID != record.StepID || payload.JobID != record.JobID || payload.PayloadDigest != record.PayloadDigest {
		return ErrInvalidInboxConsumeEvent
	}
	return nil
}

func (worker *ProviderInboxConsumerWorker) ready() error {
	if worker == nil || worker.outbox == nil || worker.inbox == nil || worker.consumer == nil ||
		worker.parse == nil || worker.workerID == "" {
		return ErrInboxConsumerWorkerDependenciesUnavailable
	}
	return nil
}

func (worker *ProviderInboxConsumerWorker) nowUTC() time.Time {
	if worker.now == nil {
		return time.Now().UTC()
	}
	return worker.now().UTC()
}

var _ onceDeliverer = (*ProviderInboxConsumerWorker)(nil)
