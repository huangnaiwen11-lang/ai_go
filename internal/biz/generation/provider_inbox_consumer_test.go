package generation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
)

type stubInboxConsumerStore struct {
	record    *ProviderInboxRecord
	readErr   error
	markErr   error
	readCalls int
	marks     []string
	markAt    []time.Time
}

func (store *stubInboxConsumerStore) ReadProviderInbox(_ context.Context, _ ProviderInboxKey) (*ProviderInboxRecord, error) {
	store.readCalls++
	if store.readErr != nil {
		return nil, store.readErr
	}
	return store.record, nil
}

func (store *stubInboxConsumerStore) MarkProviderInboxApplied(_ context.Context, _ ProviderInboxKey, digest string, at time.Time) error {
	if store.markErr != nil {
		return store.markErr
	}
	store.marks = append(store.marks, digest)
	store.markAt = append(store.markAt, at)
	return nil
}

type stubSubmissionIntentStore struct {
	intent *ProviderSubmissionIntent
	err    error
	calls  int
}

func (store *stubSubmissionIntentStore) PrepareProviderSubmission(context.Context, PrepareProviderSubmissionCommand) error {
	return errors.New("stub: prepare is not part of the consumer contract")
}

func (store *stubSubmissionIntentStore) ReadProviderSubmission(context.Context, string) (*ProviderSubmissionIntent, error) {
	store.calls++
	if store.err != nil {
		return nil, store.err
	}
	return store.intent, nil
}

type stubTerminalStore struct {
	result ProviderTerminalApplyResult
	err    error
	facts  []ProviderTerminalFact
}

func (store *stubTerminalStore) ApplyProviderTerminal(_ context.Context, fact ProviderTerminalFact) (ProviderTerminalApplyResult, error) {
	store.facts = append(store.facts, fact)
	if store.err != nil {
		return "", store.err
	}
	return store.result, nil
}

// consumerIntent 构造一份内部一致的冻结提交意图。任何一处字段被改坏，
// Request.Validate() 都会失败，消费用例必须因此拒绝而不是继续写终态。
func consumerIntent() *ProviderSubmissionIntent {
	payload := []byte(`{"externalId":"step-b2b-1","idempotencyKey":"cling-step:step-b2b-1","capability":"text_to_image"}`)
	sum := sha256.Sum256(payload)
	return &ProviderSubmissionIntent{
		Fence: 2,
		Request: FrozenProviderRequest{
			Route: creations.ExecutionRoute{
				Provider: creations.PolarStarB2BProvider, AccountRef: "account-main",
				ContractVersion: creations.B2BContractVersion, MappingVersion: "polarstar.image.v1",
			},
			StepID: "step-b2b-1", Capability: "text_to_image",
			IdempotencyKey: "cling-step:step-b2b-1", Digest: hex.EncodeToString(sum[:]), Payload: payload,
		},
	}
}

func consumerRecord(status ProviderInboxStatus) *ProviderInboxRecord {
	record := ProviderInboxRecord{
		Source: ProviderInboxSource, AccountRef: "account-main", DeliveryID: "delivery-b2b-1",
		StepID: "step-b2b-1", JobID: "job-b2b-1",
		Payload: []byte(`{"status":"completed"}`),
		Status:  status, Attempts: 1,
		CreatedAt: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC),
	}
	// PayloadDigest 必须是 payload 的真实摘要，否则记录本身校验不过。
	record.PayloadDigest = providerInboxPayloadDigest(record.Payload)
	if status == ProviderInboxApplied {
		record.TerminalDigest = ProviderTerminalSummaryDigest(ProviderTerminalCompleted, "https://cdn.example.test/r.mp4")
	}
	return &record
}

func completedObservation() ProviderTerminalObservation {
	return ProviderTerminalObservation{
		Capability: "text_to_image", Status: ProviderTerminalCompleted,
		ResultRef: "https://cdn.example.test/r.mp4",
	}
}

func consumerKey() ProviderInboxKey {
	return ProviderInboxKey{Source: ProviderInboxSource, AccountRef: "account-main", DeliveryID: "delivery-b2b-1"}
}

func newConsumerFixture() (*ProviderInboxConsumerUsecase, *stubInboxConsumerStore, *stubSubmissionIntentStore, *stubTerminalStore) {
	inbox := &stubInboxConsumerStore{record: consumerRecord(ProviderInboxPending)}
	submissions := &stubSubmissionIntentStore{intent: consumerIntent()}
	terminals := &stubTerminalStore{result: ProviderTerminalApplied}
	return NewProviderInboxConsumerUsecase(inbox, submissions, terminals, &retryingTxRunner{attempts: 1}), inbox, submissions, terminals
}

func TestConsumeSameTerminalMarksNewDeliveryApplied(t *testing.T) {
	usecase, inbox, _, terminals := newConsumerFixture()
	terminals.result = ProviderTerminalNoop
	result, err := usecase.Consume(context.Background(), consumerKey(), completedObservation())
	if err != nil || result != ProviderTerminalNoop || len(inbox.marks) != 1 {
		t.Fatalf("same terminal delivery not consumed: %s / %v / marks=%d", result, err, len(inbox.marks))
	}
}

func TestConsume写入终态并标记投递已消费(t *testing.T) {
	usecase, inbox, _, terminals := newConsumerFixture()
	result, err := usecase.Consume(context.Background(), consumerKey(), completedObservation())
	if err != nil || result != ProviderTerminalApplied {
		t.Fatalf("Consume() = %q, %v; want applied, nil", result, err)
	}
	if len(terminals.facts) != 1 {
		t.Fatalf("终态 CAS 调用 %d 次，want 1", len(terminals.facts))
	}
	fact := terminals.facts[0]
	// 身份与栅栏必须来自冻结意图，而不是来自投递自报。
	if fact.Provider != creations.PolarStarB2BProvider || fact.AccountRef != "account-main" ||
		fact.StepID != "step-b2b-1" || fact.ExternalExecutionID != "job-b2b-1" ||
		fact.Capability != "text_to_image" || fact.AttemptFence != 2 {
		t.Fatalf("终态事实 = %#v", fact)
	}
	if fact.ExpectedVersion != 0 || fact.LeaseToken != "" {
		t.Fatalf("首写必须使用版本 0 且不带租约，实际 %#v", fact)
	}
	if fact.TerminalDigest != ProviderTerminalSummaryDigest(ProviderTerminalCompleted, "https://cdn.example.test/r.mp4") {
		t.Fatalf("终态摘要 = %q", fact.TerminalDigest)
	}
	if fact.PayloadDigest != providerInboxPayloadDigest([]byte(`{"status":"completed"}`)) {
		t.Fatalf("载荷摘要 = %q，必须取自已持久化的原始字节", fact.PayloadDigest)
	}
	if len(inbox.marks) != 1 || inbox.marks[0] != fact.TerminalDigest {
		t.Fatalf("投递标记 = %#v，必须写入同一份摘要", inbox.marks)
	}
}

// 已消费的投递必须幂等：重排同一事件不得改写摘要，也不得再次触碰终态 CAS。
func TestConsume已消费投递返回Noop且不写摘要(t *testing.T) {
	usecase, inbox, _, terminals := newConsumerFixture()
	inbox.record = consumerRecord(ProviderInboxApplied)
	result, err := usecase.Consume(context.Background(), consumerKey(), completedObservation())
	if err != nil || result != ProviderTerminalNoop {
		t.Fatalf("Consume() = %q, %v; want noop, nil", result, err)
	}
	if len(terminals.facts) != 0 || len(inbox.marks) != 0 {
		t.Fatalf("已消费投递仍触发终态 %d 次、标记 %d 次", len(terminals.facts), len(inbox.marks))
	}
}

// 入库时已判定字节冲突的投递不可被消费，也不能被静默当成成功。
func TestConsume冲突投递拒绝消费(t *testing.T) {
	usecase, inbox, _, terminals := newConsumerFixture()
	inbox.record = consumerRecord(ProviderInboxQuarantined)
	if _, err := usecase.Consume(context.Background(), consumerKey(), completedObservation()); !errors.Is(err, ErrProviderInboxConflict) {
		t.Fatalf("Consume() error = %v, want ErrProviderInboxConflict", err)
	}
	if len(terminals.facts) != 0 {
		t.Fatal("冲突投递不得进入终态 CAS")
	}
}

// 终态 CAS 返回 Quarantined 时，投递不得被标成已消费：
// 否则一次冲突会被记成「已正常消费」，投递从此不再被重排。
func TestConsume未落地终态时不标记投递(t *testing.T) {
	for name, result := range map[string]ProviderTerminalApplyResult{
		"完整性冲突": ProviderTerminalQuarantined,
	} {
		t.Run(name, func(t *testing.T) {
			usecase, inbox, _, terminals := newConsumerFixture()
			terminals.result = result
			got, err := usecase.Consume(context.Background(), consumerKey(), completedObservation())
			if err != nil || got != result {
				t.Fatalf("Consume() = %q, %v; want %q, nil", got, err, result)
			}
			if len(inbox.marks) != 0 {
				t.Fatalf("未落地终态却标记了投递 %#v", inbox.marks)
			}
		})
	}
}

// 投递、冻结意图与重新解析出的结论三者任一不一致都必须拒绝：
// 挑一个信会把一笔任务的终态写到另一笔任务上。
func TestConsume交叉核对不一致时拒绝(t *testing.T) {
	cases := map[string]func(*stubInboxConsumerStore, *stubSubmissionIntentStore, *ProviderTerminalObservation){
		"能力不一致": func(_ *stubInboxConsumerStore, s *stubSubmissionIntentStore, o *ProviderTerminalObservation) {
			o.Capability = "image_edit"
		},
		"账号不一致": func(i *stubInboxConsumerStore, _ *stubSubmissionIntentStore, _ *ProviderTerminalObservation) {
			i.record.AccountRef = "account-other"
		},
		"步骤不一致": func(i *stubInboxConsumerStore, _ *stubSubmissionIntentStore, _ *ProviderTerminalObservation) {
			i.record.StepID = "step-b2b-2"
		},
		"冻结意图被改坏": func(_ *stubInboxConsumerStore, s *stubSubmissionIntentStore, _ *ProviderTerminalObservation) {
			s.intent.Request.Route.MappingVersion = ""
		},
		"栅栏为零": func(_ *stubInboxConsumerStore, s *stubSubmissionIntentStore, _ *ProviderTerminalObservation) {
			s.intent.Fence = 0
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			usecase, inbox, submissions, terminals := newConsumerFixture()
			observation := completedObservation()
			mutate(inbox, submissions, &observation)
			if _, err := usecase.Consume(context.Background(), consumerKey(), observation); !errors.Is(err, ErrProviderInboxStepMismatch) {
				t.Fatalf("Consume() error = %v, want ErrProviderInboxStepMismatch", err)
			}
			if len(terminals.facts) != 0 || len(inbox.marks) != 0 {
				t.Fatal("交叉核对失败后不得写终态或标记投递")
			}
		})
	}
}

func TestConsume观察值非法时拒绝(t *testing.T) {
	cases := map[string]ProviderTerminalObservation{
		"完成态缺少结果地址": {Capability: "text_to_image", Status: ProviderTerminalCompleted},
		"失败态携带结果地址": {Capability: "text_to_image", Status: ProviderTerminalFailed, ResultRef: "https://cdn.example.test/r.mp4"},
		"未知能力":      {Capability: "video_upscale", Status: ProviderTerminalCompleted, ResultRef: "https://cdn.example.test/r.mp4"},
		"中间态":       {Capability: "text_to_image", Status: "processing", ResultRef: "https://cdn.example.test/r.mp4"},
		"空状态":       {Capability: "text_to_image"},
	}
	for name, observation := range cases {
		t.Run(name, func(t *testing.T) {
			usecase, inbox, _, terminals := newConsumerFixture()
			if _, err := usecase.Consume(context.Background(), consumerKey(), observation); !errors.Is(err, ErrInvalidProviderTerminalObservation) {
				t.Fatalf("Consume() error = %v, want ErrInvalidProviderTerminalObservation", err)
			}
			// 非法观察值必须在开启事务之前就被拒绝。
			if inbox.readCalls != 0 || len(terminals.facts) != 0 {
				t.Fatal("非法观察值仍进入了存储")
			}
		})
	}
}

func TestConsume键非法时拒绝(t *testing.T) {
	cases := map[string]ProviderInboxKey{
		"来源不符":  {Source: "generation.execution.v2", AccountRef: "account-main", DeliveryID: "delivery-b2b-1"},
		"缺少账号":  {Source: ProviderInboxSource, DeliveryID: "delivery-b2b-1"},
		"缺少投递号": {Source: ProviderInboxSource, AccountRef: "account-main"},
		"账号含空格": {Source: ProviderInboxSource, AccountRef: "account main", DeliveryID: "delivery-b2b-1"},
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			usecase, _, _, _ := newConsumerFixture()
			if _, err := usecase.Consume(context.Background(), key, completedObservation()); !errors.Is(err, ErrInvalidProviderInboxKey) {
				t.Fatalf("Consume() error = %v, want ErrInvalidProviderInboxKey", err)
			}
		})
	}
}

// 工作项存在但投递不存在，说明事件与事实被拆开了。重排不会让它出现，
// 因此必须显式失败，而不是静默成功让事件结案。
func TestConsume投递不存在时失败(t *testing.T) {
	usecase, inbox, _, _ := newConsumerFixture()
	inbox.record = nil
	if _, err := usecase.Consume(context.Background(), consumerKey(), completedObservation()); !errors.Is(err, ErrInvalidProviderInboxKey) {
		t.Fatalf("Consume() error = %v, want ErrInvalidProviderInboxKey", err)
	}
}

// 瞬时竞态与存储故障必须原样上抛，让调用方按退避重排同一事件。
func TestConsume瞬时故障原样上抛(t *testing.T) {
	raceErr := ErrProviderTerminalVersion
	usecase, _, _, terminals := newConsumerFixture()
	terminals.err = raceErr
	if _, err := usecase.Consume(context.Background(), consumerKey(), completedObservation()); !errors.Is(err, raceErr) {
		t.Fatalf("Consume() error = %v, want %v", err, raceErr)
	}

	markErr := errors.New("mongo write conflict")
	usecase, inbox, _, _ := newConsumerFixture()
	inbox.markErr = markErr
	if _, err := usecase.Consume(context.Background(), consumerKey(), completedObservation()); !errors.Is(err, markErr) {
		t.Fatalf("Consume() error = %v, want %v", err, markErr)
	}

	readErr := errors.New("mongo unavailable")
	usecase, inbox, _, _ = newConsumerFixture()
	inbox.readErr = readErr
	if _, err := usecase.Consume(context.Background(), consumerKey(), completedObservation()); !errors.Is(err, readErr) {
		t.Fatalf("Consume() error = %v, want %v", err, readErr)
	}
}

func TestConsume依赖缺失时拒绝(t *testing.T) {
	inbox := &stubInboxConsumerStore{record: consumerRecord(ProviderInboxPending)}
	submissions := &stubSubmissionIntentStore{intent: consumerIntent()}
	terminals := &stubTerminalStore{result: ProviderTerminalApplied}
	runner := &retryingTxRunner{attempts: 1}
	cases := map[string]*ProviderInboxConsumerUsecase{
		"零值用例":   nil,
		"缺少投递存储": NewProviderInboxConsumerUsecase(nil, submissions, terminals, runner),
		"缺少意图存储": NewProviderInboxConsumerUsecase(inbox, nil, terminals, runner),
		"缺少终态存储": NewProviderInboxConsumerUsecase(inbox, submissions, nil, runner),
		"缺少事务":   NewProviderInboxConsumerUsecase(inbox, submissions, terminals, nil),
	}
	for name, usecase := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := usecase.Consume(context.Background(), consumerKey(), completedObservation()); !errors.Is(err, ErrProviderInboxConsumerDependenciesUnavailable) {
				t.Fatalf("Consume() error = %v, want ErrProviderInboxConsumerDependenciesUnavailable", err)
			}
		})
	}
}

// 业务时刻必须在进入可重试事务之前冻结一次：整笔事务重试写出不同的
// ConfirmedAt 会让「同一结论」看起来像两次不同的事件。
func TestConsume业务时刻在进入事务前冻结(t *testing.T) {
	fixed := time.Date(2026, 9, 19, 11, 0, 0, 0, time.UTC)
	clockCalls := 0
	inbox := &stubInboxConsumerStore{record: consumerRecord(ProviderInboxPending)}
	terminals := &stubTerminalStore{result: ProviderTerminalApplied}
	usecase := NewProviderInboxConsumerUsecaseWithClock(inbox, &stubSubmissionIntentStore{intent: consumerIntent()}, terminals,
		&retryingTxRunner{attempts: 3}, func() time.Time {
			clockCalls++
			return fixed
		})
	if _, err := usecase.Consume(context.Background(), consumerKey(), completedObservation()); err != nil {
		t.Fatalf("Consume() error = %v", err)
	}
	if clockCalls != 1 {
		t.Fatalf("业务时钟被调用 %d 次，want 1", clockCalls)
	}
	for index, fact := range terminals.facts {
		if !fact.ObservedAt.Equal(fixed) {
			t.Fatalf("第 %d 次终态时刻 = %s, want %s", index, fact.ObservedAt, fixed)
		}
	}
	for index, at := range inbox.markAt {
		if !at.Equal(fixed) {
			t.Fatalf("第 %d 次标记时刻 = %s, want %s", index, at, fixed)
		}
	}
}

// 摘要必须只由「结论 + 语义摘要」决定：原始报文字节不参与，否则同一结论经
// lookup 与 callback 两条路到达时会被判成冲突。
func TestProviderTerminalSummaryDigest只由结论决定(t *testing.T) {
	base := ProviderTerminalSummaryDigest(ProviderTerminalCompleted, "https://cdn.example.test/r.mp4")
	if base != ProviderTerminalSummaryDigest(ProviderTerminalCompleted, "https://cdn.example.test/r.mp4") {
		t.Fatal("同一结论必须得到同一摘要")
	}
	for name, other := range map[string]string{
		"状态不同": ProviderTerminalSummaryDigest(ProviderTerminalFailed, "https://cdn.example.test/r.mp4"),
		"结果不同": ProviderTerminalSummaryDigest(ProviderTerminalCompleted, "https://cdn.example.test/other.mp4"),
		"结果为空": ProviderTerminalSummaryDigest(ProviderTerminalCompleted, ""),
	} {
		if other == base {
			t.Fatalf("%s 的摘要与基准相同，无法区分结论", name)
		}
	}
	if len(base) != sha256.Size*2 || !providerInboxDigest(base) {
		t.Fatalf("摘要 %q 不是规范的小写 SHA-256", base)
	}
}
