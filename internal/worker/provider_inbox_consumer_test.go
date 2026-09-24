package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const (
	inboxConsumerAccount  = "account-main"
	inboxConsumerStep     = "step-b2b-1"
	inboxConsumerJob      = "job-b2b-1"
	inboxConsumerDelivery = "delivery-b2b-1"
	inboxConsumerLease    = "lease-token-1"
)

var inboxConsumerNow = time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC)

func inboxConsumerRecord(t *testing.T) *generation.ProviderInboxRecord {
	t.Helper()
	record, err := generation.NewProviderInboxRecord(
		inboxConsumerAccount, inboxConsumerDelivery, inboxConsumerStep, inboxConsumerJob, "",
		[]byte(`{"status":"completed"}`), 1, inboxConsumerNow,
	)
	if err != nil {
		t.Fatalf("构造投递事实: %v", err)
	}
	return &record
}

// inboxConsumeEvent 按 data 层生产者的字段布局构造工作项。
// 夹具刻意重写一遍结构体而不是复用生产代码：字段一旦漂移，用例必须自己红。
func inboxConsumeEvent(t *testing.T, record *generation.ProviderInboxRecord) *outbox.Event {
	t.Helper()
	payload, err := json.Marshal(struct {
		Source         string `json:"source"`
		AccountRef     string `json:"accountRef"`
		DeliveryID     string `json:"deliveryId"`
		StepID         string `json:"stepId"`
		JobID          string `json:"jobId"`
		PayloadDigest  string `json:"payloadDigest"`
		TerminalDigest string `json:"terminalDigest,omitempty"`
	}{record.Source, record.AccountRef, record.DeliveryID, record.StepID, record.JobID, record.PayloadDigest, record.TerminalDigest})
	if err != nil {
		t.Fatalf("构造工作项载荷: %v", err)
	}
	return &outbox.Event{
		ID: generation.ProviderInboxEventID(record.Source, record.AccountRef, record.DeliveryID), AggregateID: record.StepID,
		EventType: outbox.EventType(generation.ProviderInboxConsumeEventType), Payload: payload,
		DeliveryStatus: outbox.DeliveryStatusDispatching, AttemptCount: 1,
		NextAttemptAt: inboxConsumerNow, LeaseToken: inboxConsumerLease, LeaseUntil: inboxConsumerNow.Add(time.Minute),
		LeaseOwner: "worker-1", CreatedAt: inboxConsumerNow, UpdatedAt: inboxConsumerNow,
	}
}

type stubInboxOutbox struct {
	event       *outbox.Event
	claimErr    error
	missOnID    bool
	settleErr   error
	claimedType outbox.EventType
	claimedID   string
	delivered   []string
	failed      []string
	requeued    []time.Time
	attended    []outbox.AttentionReason
}

func (stub *stubInboxOutbox) Enqueue(context.Context, *outbox.Event) error {
	return outbox.ErrInvalidEvent
}

func (stub *stubInboxOutbox) Claim(context.Context, string, time.Time, time.Time) (*outbox.Event, error) {
	stub.claimedType = outbox.EventType(generation.ProviderInboxConsumeEventType)
	return stub.event, stub.claimErr
}

func (stub *stubInboxOutbox) ClaimByType(_ context.Context, _ string, eventType outbox.EventType, _, _ time.Time) (*outbox.Event, error) {
	stub.claimedType = eventType
	return stub.event, stub.claimErr
}

func (stub *stubInboxOutbox) ClaimByIDAndType(_ context.Context, _, eventID string, eventType outbox.EventType, _, _ time.Time) (*outbox.Event, error) {
	stub.claimedType, stub.claimedID = eventType, eventID
	if stub.missOnID {
		return nil, stub.claimErr
	}
	return stub.event, stub.claimErr
}

func (stub *stubInboxOutbox) RenewLease(context.Context, string, string, time.Time, time.Time) error {
	return nil
}

func (stub *stubInboxOutbox) Requeue(_ context.Context, _, _ string, nextAttemptAt time.Time) error {
	stub.requeued = append(stub.requeued, nextAttemptAt)
	return stub.settleErr
}

func (stub *stubInboxOutbox) MarkDelivered(_ context.Context, eventID, _ string, _ time.Time) error {
	stub.delivered = append(stub.delivered, eventID)
	return stub.settleErr
}

func (stub *stubInboxOutbox) MarkFailed(_ context.Context, eventID, _ string, _ time.Time) error {
	stub.failed = append(stub.failed, eventID)
	return stub.settleErr
}

func (stub *stubInboxOutbox) MarkNeedsAttention(_ context.Context, _ string, _ string, reason outbox.AttentionReason, _ time.Time) error {
	stub.attended = append(stub.attended, reason)
	return stub.settleErr
}

// RedriveAttention 只是接口占位：本包的用例都不驱动人工重驱。
func (stub *stubInboxOutbox) RedriveAttention(context.Context, outbox.RedriveCommand) (*outbox.Event, error) {
	return nil, outbox.ErrRedriveNotEligible
}

type stubInboxReader struct {
	record *generation.ProviderInboxRecord
	err    error
	keys   []generation.ProviderInboxKey
}

func (stub *stubInboxReader) ReadProviderInbox(_ context.Context, key generation.ProviderInboxKey) (*generation.ProviderInboxRecord, error) {
	stub.keys = append(stub.keys, key)
	return stub.record, stub.err
}

type stubInboxConsumer struct {
	result      generation.ProviderTerminalApplyResult
	err         error
	calls       int
	key         generation.ProviderInboxKey
	observation generation.ProviderTerminalObservation
}

func (stub *stubInboxConsumer) Consume(_ context.Context, key generation.ProviderInboxKey, observation generation.ProviderTerminalObservation) (generation.ProviderTerminalApplyResult, error) {
	stub.calls++
	stub.key, stub.observation = key, observation
	return stub.result, stub.err
}

type stubInboxParser struct {
	observation generation.ProviderTerminalObservation
	err         error
	calls       int
}

func (stub *stubInboxParser) Parse(*generation.ProviderInboxRecord) (generation.ProviderTerminalObservation, error) {
	stub.calls++
	return stub.observation, stub.err
}

type inboxConsumerFixture struct {
	worker   *ProviderInboxConsumerWorker
	outbox   *stubInboxOutbox
	reader   *stubInboxReader
	consumer *stubInboxConsumer
	parser   *stubInboxParser
	record   *generation.ProviderInboxRecord
}

func newInboxConsumerFixture(t *testing.T) *inboxConsumerFixture {
	t.Helper()
	record := inboxConsumerRecord(t)
	event := inboxConsumeEvent(t, record)
	fixture := &inboxConsumerFixture{
		outbox:   &stubInboxOutbox{event: event},
		reader:   &stubInboxReader{record: record},
		consumer: &stubInboxConsumer{result: generation.ProviderTerminalApplied},
		parser: &stubInboxParser{observation: generation.ProviderTerminalObservation{
			Capability: "text_to_image", Status: generation.ProviderTerminalCompleted, ResultRef: "https://cdn.polarstar.work/r.png",
		}},
		record: record,
	}
	fixture.worker = NewProviderInboxConsumerWorker(fixture.outbox, fixture.reader, fixture.consumer, fixture.parser, "worker-1", func() time.Time { return inboxConsumerNow })
	return fixture
}

func TestInboxConsumerWorker把已ACK投递推进为终态并结案(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.outbox.claimedType != outbox.EventType(generation.ProviderInboxConsumeEventType) {
		t.Fatalf("领取类型 = %q", fixture.outbox.claimedType)
	}
	if fixture.parser.calls != 1 || fixture.consumer.calls != 1 {
		t.Fatalf("解析 %d 次 / 消费 %d 次，期望各 1 次", fixture.parser.calls, fixture.consumer.calls)
	}
	wantKey := generation.ProviderInboxKey{Source: generation.ProviderInboxSource, AccountRef: inboxConsumerAccount, DeliveryID: inboxConsumerDelivery}
	if fixture.reader.keys[0] != wantKey || fixture.consumer.key != wantKey {
		t.Fatalf("键 = %#v / %#v，期望 %#v", fixture.reader.keys[0], fixture.consumer.key, wantKey)
	}
	if fixture.consumer.observation != fixture.parser.observation {
		t.Fatalf("观察值 = %#v，期望 %#v", fixture.consumer.observation, fixture.parser.observation)
	}
	if len(fixture.outbox.delivered) != 1 || len(fixture.outbox.failed) != 0 || len(fixture.outbox.requeued) != 0 {
		t.Fatalf("结案 = 投递 %v / 失败 %v / 重排 %v", fixture.outbox.delivered, fixture.outbox.failed, fixture.outbox.requeued)
	}
}

func TestInboxConsumerWorker幂等重放按投递完成结案(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.consumer.result = generation.ProviderTerminalNoop
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(fixture.outbox.delivered) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 投递 %v / 失败 %v", fixture.outbox.delivered, fixture.outbox.failed)
	}
}

// 隔离结论意味着完整性冲突已经落证据，重排不会改变结论：工作项必须以失败
// 结案，否则发件箱会把一条没能收敛的投递记成正常投递。
func TestInboxConsumerWorker隔离结论按失败结案(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.consumer.result = generation.ProviderTerminalQuarantined
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(fixture.outbox.failed) != 1 || len(fixture.outbox.delivered) != 0 || len(fixture.outbox.requeued) != 0 {
		t.Fatalf("结案 = 投递 %v / 失败 %v / 重排 %v", fixture.outbox.delivered, fixture.outbox.failed, fixture.outbox.requeued)
	}
}

func TestInboxConsumerWorker瞬时故障按退避重排(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.consumer.err = errors.New("mongo write conflict")
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(fixture.outbox.requeued) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 重排 %v / 失败 %v", fixture.outbox.requeued, fixture.outbox.failed)
	}
	// 尝试计数在领取时已经自增（本夹具为 1），首次失败应退避 10s。
	if want := inboxConsumerNow.Add(10 * time.Second); !fixture.outbox.requeued[0].Equal(want) {
		t.Fatalf("重排时刻 = %v，期望 %v", fixture.outbox.requeued[0], want)
	}
}

// 事实矛盾类失败必须放弃而不是无限重排：把它们当瞬时故障会让一条坏工作项
// 永远占着队列，而它的结论永远不会变。
func TestInboxConsumerWorker事实矛盾类失败按失败结案(t *testing.T) {
	cases := map[string]error{
		"投递键非法":    generation.ErrInvalidProviderInboxKey,
		"记录损坏":     generation.ErrInvalidProviderInboxRecord,
		"投递已隔离":    generation.ErrProviderInboxConflict,
		"步骤身份不符":   generation.ErrProviderInboxStepMismatch,
		"观察值非法":    generation.ErrInvalidProviderTerminalObservation,
		"终态事实非法":   generation.ErrInvalidProviderTerminal,
		"冻结意图缺失":   generation.ErrSubmissionConflict,
		"字节不再满足合同": polarstarb2b.ErrInvalidCallback,
	}
	for name, cause := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newInboxConsumerFixture(t)
			fixture.consumer.err = cause
			if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
				t.Fatalf("DeliverOnce() error = %v", err)
			}
			if len(fixture.outbox.failed) != 1 || len(fixture.outbox.requeued) != 0 {
				t.Fatalf("结案 = 失败 %v / 重排 %v", fixture.outbox.failed, fixture.outbox.requeued)
			}
		})
	}
}

func TestInboxConsumerWorker投递不存在时按失败结案(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.reader.record = nil
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(fixture.outbox.failed) != 1 || fixture.consumer.calls != 0 {
		t.Fatalf("结案 = 失败 %v / 消费 %d 次", fixture.outbox.failed, fixture.consumer.calls)
	}
}

func TestInboxConsumerWorker读回故障按可重试性分流(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.reader.err = errors.New("mongo timeout")
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(fixture.outbox.requeued) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("瞬时读故障结案 = 重排 %v / 失败 %v", fixture.outbox.requeued, fixture.outbox.failed)
	}

	permanent := newInboxConsumerFixture(t)
	permanent.reader.err = generation.ErrInvalidProviderInboxRecord
	if err := permanent.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(permanent.outbox.failed) != 1 || len(permanent.outbox.requeued) != 0 {
		t.Fatalf("永久读故障结案 = 失败 %v / 重排 %v", permanent.outbox.failed, permanent.outbox.requeued)
	}
}

// 键相同只能保证读到同一条记录，不能保证工作项载荷没被改写。
func TestInboxConsumerWorker工作项与投递事实不符时拒绝消费(t *testing.T) {
	for name, mutate := range map[string]func(*generation.ProviderInboxRecord){
		"步骤不符":   func(record *generation.ProviderInboxRecord) { record.StepID = "step-other" },
		"任务号不符":  func(record *generation.ProviderInboxRecord) { record.JobID = "job-other" },
		"载荷摘要不符": func(record *generation.ProviderInboxRecord) { record.PayloadDigest = "0" + record.PayloadDigest[1:] },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newInboxConsumerFixture(t)
			mutate(fixture.record)
			if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
				t.Fatalf("DeliverOnce() error = %v", err)
			}
			if len(fixture.outbox.failed) != 1 || fixture.parser.calls != 0 || fixture.consumer.calls != 0 {
				t.Fatalf("结案 = 失败 %v / 解析 %d 次 / 消费 %d 次", fixture.outbox.failed, fixture.parser.calls, fixture.consumer.calls)
			}
		})
	}
}

func TestInboxConsumerWorker工作项载荷非法时按失败结案(t *testing.T) {
	for name, payload := range map[string][]byte{
		"非JSON":  []byte("not-json"),
		"缺少身份":   []byte(`{"source":"polarstar.b2b.v2"}`),
		"来源不符合同": []byte(`{"source":"other.source","accountRef":"account-main","deliveryId":"delivery-b2b-1","stepId":"step-b2b-1","jobId":"job-b2b-1","payloadDigest":"x"}`),
		"缺少步骤":   []byte(`{"source":"polarstar.b2b.v2","accountRef":"account-main","deliveryId":"delivery-b2b-1","jobId":"job-b2b-1","payloadDigest":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newInboxConsumerFixture(t)
			fixture.outbox.event.Payload = payload
			if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
				t.Fatalf("DeliverOnce() error = %v", err)
			}
			if len(fixture.outbox.failed) != 1 || fixture.reader.keys != nil {
				t.Fatalf("结案 = 失败 %v / 读回 %v", fixture.outbox.failed, fixture.reader.keys)
			}
		})
	}
}

func TestInboxConsumerWorker解析失败按可重试性分流(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.parser.err = polarstarb2b.ErrInvalidCallback
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(fixture.outbox.failed) != 1 || fixture.consumer.calls != 0 {
		t.Fatalf("确定性解析失败结案 = 失败 %v / 消费 %d 次", fixture.outbox.failed, fixture.consumer.calls)
	}

	transient := newInboxConsumerFixture(t)
	transient.parser.err = errors.New("temporary decode failure")
	if err := transient.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if len(transient.outbox.requeued) != 1 || len(transient.outbox.failed) != 0 {
		t.Fatalf("瞬时解析失败结案 = 重排 %v / 失败 %v", transient.outbox.requeued, transient.outbox.failed)
	}
}

// 租约被并发消费者接管不是故障：上抛会让轮询进程因为一次正常交接而整体退出。
func TestInboxConsumerWorker租约冲突不阻断轮询(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.outbox.settleErr = outbox.ErrLeaseConflict
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
}

func TestInboxConsumerWorker只处理自己的事件类型(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.outbox.event.EventType = outbox.EventTypeGenerationSubmission
	if err := fixture.worker.DeliverOnce(context.Background(), ""); !errors.Is(err, ErrUnexpectedClaimedEvent) {
		t.Fatalf("错误 = %v，期望 ErrUnexpectedClaimedEvent", err)
	}

	fixture = newInboxConsumerFixture(t)
	if err := fixture.worker.DeliverOnce(context.Background(), "generation.inbox.consume:other"); !errors.Is(err, ErrUnexpectedClaimedEvent) {
		t.Fatalf("指定投递不符错误 = %v，期望 ErrUnexpectedClaimedEvent", err)
	}
}

func TestInboxConsumerWorker指定投递不命中时静默返回(t *testing.T) {
	fixture := newInboxConsumerFixture(t)
	fixture.outbox.missOnID = true
	if err := fixture.worker.DeliverOnce(context.Background(), "generation.inbox.consume:other"); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.outbox.claimedID != "generation.inbox.consume:other" || fixture.consumer.calls != 0 {
		t.Fatalf("领取 ID = %q / 消费 %d 次", fixture.outbox.claimedID, fixture.consumer.calls)
	}
}

func TestInboxConsumerWorker依赖缺失时拒绝(t *testing.T) {
	complete := newInboxConsumerFixture(t)
	cases := map[string]*ProviderInboxConsumerWorker{
		"空工作者":   nil,
		"缺发件箱":   NewProviderInboxConsumerWorker(nil, complete.reader, complete.consumer, complete.parser, "worker-1", nil),
		"缺读取器":   NewProviderInboxConsumerWorker(complete.outbox, nil, complete.consumer, complete.parser, "worker-1", nil),
		"缺消费器":   NewProviderInboxConsumerWorker(complete.outbox, complete.reader, nil, complete.parser, "worker-1", nil),
		"缺解析器":   NewProviderInboxConsumerWorker(complete.outbox, complete.reader, complete.consumer, nil, "worker-1", nil),
		"缺工作者标识": NewProviderInboxConsumerWorker(complete.outbox, complete.reader, complete.consumer, complete.parser, "", nil),
	}
	for name, worker := range cases {
		t.Run(name, func(t *testing.T) {
			if err := worker.DeliverOnce(context.Background(), ""); !errors.Is(err, ErrInboxConsumerWorkerDependenciesUnavailable) {
				t.Fatalf("错误 = %v，期望 ErrInboxConsumerWorkerDependenciesUnavailable", err)
			}
		})
	}
}
