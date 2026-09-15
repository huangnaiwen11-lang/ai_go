package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/executionv2"
	platform "ai-business-service/internal/integrations/generation"
)

func TestDeliverOnce明确拒绝时事务内冲正一次(t *testing.T) {
	fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Fatalf("请求方法 = %s, want POST", request.Method)
		}
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"code":"INVALID_INPUT"}`))
	})

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	fixture.assertFailedAndReversed(t)

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("重复 DeliverOnce() error = %v", err)
	}
	fixture.assertFailedAndReversed(t)
}

// 审核拒绝必须在调用生成中台之前终止任务，并将预留转为不可退款的没收状态。
func TestDeliverOnce审核拒绝不调用生成中台且没收一次(t *testing.T) {
	postCalls := 0
	fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		postCalls++
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"must-not-submit","status":"accepted"}}`))
	})
	fixture.store.record.ContentAccess = identity.ContentAccessReviewRestricted
	fixture.reviewer.decision = contentreview.Decision{Outcome: contentreview.OutcomeRejected}

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("审核拒绝仍调用生成中台 %d 次", postCalls)
	}
	if fixture.reverser.confiscateCalls != 1 || fixture.reverser.calls != 0 {
		t.Fatalf("没收/冲正次数 = %d/%d, want 1/0", fixture.reverser.confiscateCalls, fixture.reverser.calls)
	}
	if fixture.store.confiscatedCalls != 1 || fixture.outbox.failedCalls != 1 {
		t.Fatalf("没收/发件箱结案次数 = %d/%d, want 1/1", fixture.store.confiscatedCalls, fixture.outbox.failedCalls)
	}
}

// 审核依赖故障不能被当作内容拒绝；不发中台且沿用确定失败的冲正路径。
func TestDeliverOnce审核不可用不调用生成中台并冲正(t *testing.T) {
	postCalls := 0
	fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		postCalls++
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"must-not-submit","status":"accepted"}}`))
	})
	fixture.store.record.ContentAccess = identity.ContentAccessReviewRestricted
	fixture.reviewer.err = contentreview.ErrUnavailable

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("审核不可用仍调用生成中台 %d 次", postCalls)
	}
	if fixture.reverser.confiscateCalls != 0 || fixture.reverser.calls != 1 {
		t.Fatalf("没收/冲正次数 = %d/%d, want 0/1", fixture.reverser.confiscateCalls, fixture.reverser.calls)
	}
	fixture.assertFailedAndReversed(t)
}

func TestDeliverOnce仅领取生成提交事件(t *testing.T) {
	fixture := newSubmissionWorkerFixture(t, func(_ http.ResponseWriter, request *http.Request) {
		t.Fatalf("非生成事件不得请求中台: %s", request.URL.Path)
	})
	fixture.event.EventType = outbox.EventType("other.event")

	if err := fixture.worker.DeliverOnce(fixture.ctx, ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v, want nil", err)
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending || fixture.event.LeaseToken != "" || fixture.event.AttemptCount != 0 {
		t.Fatalf("非生成事件被工作者领取: %#v", fixture.event)
	}
}

func TestDeliverOnce本地载荷校验失败时不请求中台并立即冲正(t *testing.T) {
	postCount := 0
	fixture := newSubmissionWorkerFixture(t, func(_ http.ResponseWriter, request *http.Request) {
		postCount++
		t.Fatalf("本地校验失败不得请求中台: %s", request.URL.Path)
	})
	fixture.store.record.ExecutionPayload = []byte(`{"contractVersion":"invalid"}`)

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	fixture.assertFailedAndReversed(t)
	if postCount != 0 {
		t.Fatalf("本地校验失败的 POST 次数 = %d, want 0", postCount)
	}
}

func TestDeliverOnce本地空Nonce时不请求中台并立即冲正(t *testing.T) {
	postCount := 0
	fixture := newSubmissionWorkerFixture(t, func(_ http.ResponseWriter, request *http.Request) {
		postCount++
		t.Fatalf("空 nonce 不得请求中台: %s", request.URL.Path)
	})
	client, err := platform.NewClient(
		fixture.serverURL,
		"test-generation-request-hmac-key-12345",
		fixture.clientHTTP,
		func() time.Time { return fixture.now },
		func() string { return "" },
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.worker = NewGenerationSubmissionWorker(fixture.outbox, fixture.store, fixture.reverser, fixture.tx, client, fixture.reviewer, "worker-test", func() time.Time { return fixture.now })

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	fixture.assertFailedAndReversed(t)
	if postCount != 0 {
		t.Fatalf("空 nonce 的 POST 次数 = %d, want 0", postCount)
	}
}

func TestDeliverOnce请求超时先查询且不创建第二个技术任务(t *testing.T) {
	postStarted := make(chan struct{})
	releasePost := make(chan struct{})
	postFinished := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePost) }) }
	t.Cleanup(release)
	var fixture *submissionWorkerFixture
	fixture = newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/executions":
			fixture.mu.Lock()
			fixture.postCount++
			fixture.mu.Unlock()
			close(postStarted)
			// 客户端超时不会保证服务端 request.Context 立刻结束；由测试显式释放，避免 httptest.Close 遗留连接。
			<-releasePost
			close(postFinished)
		case "/api/v2/executions/lookup":
			fixture.mu.Lock()
			fixture.lookupCount++
			fixture.mu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-1","status":"accepted"}}`))
		default:
			t.Fatalf("意外请求路径: %s", request.URL.Path)
		}
	})
	// 模拟真实事务：若工作者错误复用已取消的外部请求 context，状态写入必须失败。
	fixture.tx.rejectCanceledContext = true
	timeoutContext, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	fixture.ctx = timeoutContext

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("第一次 DeliverOnce() error = %v", err)
	}
	cancel()
	select {
	case <-postStarted:
	case <-time.After(time.Second):
		t.Fatal("超时请求未到达 httptest 服务")
	}
	release()
	select {
	case <-postFinished:
	case <-time.After(time.Second):
		t.Fatal("超时请求未在测试释放后结束")
	}
	fixture.ctx = context.Background()
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusReconciling || fixture.store.reconcilingCalls != 1 {
		t.Fatalf("超时后 event/store = %s/%d, want reconciling/1", fixture.event.DeliveryStatus, fixture.store.reconcilingCalls)
	}
	if fixture.postCount != 1 || fixture.lookupCount != 0 {
		t.Fatalf("超时后 POST/lookup = %d/%d, want 1/0", fixture.postCount, fixture.lookupCount)
	}

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("第二次 DeliverOnce() error = %v", err)
	}
	if fixture.store.submittedJobID != "job-1" || fixture.event.DeliveryStatus != outbox.DeliveryStatusDelivered {
		t.Fatalf("查询接受后 job/event = %q/%s, want job-1/delivered", fixture.store.submittedJobID, fixture.event.DeliveryStatus)
	}
	if fixture.postCount != 1 || fixture.lookupCount != 1 {
		t.Fatalf("POST/lookup = %d/%d, want 1/1", fixture.postCount, fixture.lookupCount)
	}
}

func TestDeliverOnce查询HTTP拒绝状态仍未知不冲正(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fixture := newReconcilingFixture(t, status)

			if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
				t.Fatalf("DeliverOnce() error = %v", err)
			}
			if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending {
				t.Fatalf("查询 HTTP %d 后 event status = %s, want pending", status, fixture.event.DeliveryStatus)
			}
			if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
				t.Fatalf("查询 HTTP %d 错误触发了冲正: reverse/rejected = %d/%d", status, fixture.reverser.calls, fixture.store.rejectedCalls)
			}
		})
	}
}

func TestDeliverOnce查询已验证拒绝语义时冲正(t *testing.T) {
	fixture := newReconcilingFixtureWithResponse(t, http.StatusOK, `{"success":true,"data":{"jobId":"not-accepted","status":"rejected"}}`)

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	fixture.assertFailedAndReversed(t)
}

func TestDeliverOnce指定目标不领取其他事件且目标仍首次提交(t *testing.T) {
	postCalls := 0
	fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v2/executions" {
			t.Fatalf("指定目标首次投递不得查询: %s", request.URL.Path)
		}
		postCalls++
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-target","status":"accepted"}}`))
	})
	other, err := outbox.NewPending("generation.submission:step-other", "creation-other", fixture.event.Payload, fixture.now)
	if err != nil {
		t.Fatalf("创建其他测试事件: %v", err)
	}
	targetedOutbox := &targetedMemoryOutbox{events: []*outbox.Event{other, fixture.event}}
	fixture.worker = NewGenerationSubmissionWorker(targetedOutbox, fixture.store, fixture.reverser, fixture.tx, mustWorkerClient(t, fixture), fixture.reviewer, "worker-test", func() time.Time { return fixture.now })

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if other.DeliveryStatus != outbox.DeliveryStatusPending || other.AttemptCount != 0 || other.LeaseToken != "" {
		t.Fatalf("目标事件投递影响了其他事件: %#v", other)
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusDelivered || fixture.event.AttemptCount != 1 || postCalls != 1 {
		t.Fatalf("目标首次投递状态/次数/POST = %s/%d/%d, want delivered/1/1", fixture.event.DeliveryStatus, fixture.event.AttemptCount, postCalls)
	}
}

func TestDeliverOnce查询仍未知时带退避重新入队(t *testing.T) {
	fixture := newReconcilingFixture(t, http.StatusInternalServerError)

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending {
		t.Fatalf("event status = %s, want pending", fixture.event.DeliveryStatus)
	}
	if got, want := fixture.event.NextAttemptAt, fixture.now.Add(defaultRetryBackoff); !got.Equal(want) {
		t.Fatalf("next attempt = %s, want %s", got, want)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
		t.Fatalf("未知查询触发了冲正: reverse/rejected = %d/%d", fixture.reverser.calls, fixture.store.rejectedCalls)
	}
}

func TestDeliverOnce已被其他路径收敛时不再次冲正(t *testing.T) {
	fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
	})
	fixture.store.rejectError = generation.ErrSubmissionConflict

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.reverser.calls != 0 || fixture.outbox.failedCalls != 0 {
		t.Fatalf("已收敛冲突不应再次冲正或结案: reverse/failed = %d/%d", fixture.reverser.calls, fixture.outbox.failedCalls)
	}
	if fixture.store.readFacts != 1 {
		t.Fatalf("已收敛冲突未读取既有事实: %d", fixture.store.readFacts)
	}
}

type submissionWorkerFixture struct {
	t           *testing.T
	ctx         context.Context
	now         time.Time
	event       *outbox.Event
	outbox      *memoryOutboxRepository
	store       *memorySubmissionStore
	reverser    *memoryReverser
	reviewer    *memoryReviewer
	recorder    *operationRecorder
	tx          *memoryTxRunner
	serverURL   string
	clientHTTP  *http.Client
	worker      *GenerationSubmissionWorker
	mu          sync.Mutex
	postCount   int
	lookupCount int
}

func newSubmissionWorkerFixture(t *testing.T, handler http.HandlerFunc) *submissionWorkerFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	stepID := "step-worker-test"
	snapshot, err := executionv2.Compile(
		executionv2.CapabilityTextToImage,
		"ps-image-v1",
		[]byte(`{"prompt":"test","assets":[],"parameters":{}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := snapshot.MarshalSubmissionPayload()
	if err != nil {
		t.Fatal(err)
	}
	event, err := outbox.NewPending(outbox.SubmissionEventID(stepID), "creation-worker-test", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	clientHTTP := server.Client()
	client, err := platform.NewClient(server.URL, "test-generation-request-hmac-key-12345", clientHTTP, func() time.Time { return now }, func() string { return "nonce-worker-test" })
	if err != nil {
		t.Fatal(err)
	}
	fixture := &submissionWorkerFixture{
		t: t, ctx: context.Background(), now: now, event: event,
		recorder:   &operationRecorder{},
		serverURL:  server.URL,
		clientHTTP: clientHTTP,
	}
	fixture.outbox = &memoryOutboxRepository{event: event, recorder: fixture.recorder}
	fixture.store = &memorySubmissionStore{record: generation.SubmissionRecord{EventID: event.ID, CreationID: event.AggregateID, StepID: stepID, UserID: "user-worker-test", ContentAccess: identity.ContentAccessStandard, LeaseToken: "lease-worker-test", ExecutionPayload: payload}, event: event, recorder: fixture.recorder}
	fixture.reverser = &memoryReverser{recorder: fixture.recorder}
	fixture.reviewer = &memoryReviewer{}
	fixture.tx = &memoryTxRunner{}
	fixture.worker = NewGenerationSubmissionWorker(fixture.outbox, fixture.store, fixture.reverser, fixture.tx, client, fixture.reviewer, "worker-test", func() time.Time { return fixture.now })
	return fixture
}

func newReconcilingFixture(t *testing.T, lookupStatus int) *submissionWorkerFixture {
	return newReconcilingFixtureWithResponse(t, lookupStatus, "")
}

func newReconcilingFixtureWithResponse(t *testing.T, lookupStatus int, body string) *submissionWorkerFixture {
	var fixture *submissionWorkerFixture
	fixture = newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v2/executions/lookup" {
			t.Fatalf("reconciling 不得 POST: %s", request.URL.Path)
		}
		fixture.mu.Lock()
		fixture.lookupCount++
		fixture.mu.Unlock()
		writer.WriteHeader(lookupStatus)
		if body != "" {
			_, _ = writer.Write([]byte(body))
		}
	})
	fixture.event.DeliveryStatus = outbox.DeliveryStatusReconciling
	// 真实事件先经历过一次未知提交，重新领取时尝试计数至少为 2。
	fixture.event.AttemptCount = 1
	fixture.store.status = "reconciling"
	return fixture
}

func mustWorkerClient(t *testing.T, fixture *submissionWorkerFixture) *platform.Client {
	t.Helper()
	client, err := platform.NewClient(
		fixture.serverURL,
		"test-generation-request-hmac-key-12345",
		fixture.clientHTTP,
		func() time.Time { return fixture.now },
		func() string { return "nonce-worker-test" },
	)
	if err != nil {
		t.Fatalf("创建测试中台客户端: %v", err)
	}
	return client
}

func (fixture *submissionWorkerFixture) assertFailedAndReversed(t *testing.T) {
	t.Helper()
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusFailed || fixture.store.status != "failed" {
		t.Fatalf("失败状态 event/store = %s/%s, want failed/failed", fixture.event.DeliveryStatus, fixture.store.status)
	}
	if fixture.reverser.calls != 1 || fixture.outbox.failedCalls != 1 {
		t.Fatalf("冲正/失败结案次数 = %d/%d, want 1/1", fixture.reverser.calls, fixture.outbox.failedCalls)
	}
	if fixture.store.rejectedCalls != 1 {
		t.Fatalf("MarkRejected 次数 = %d, want 1", fixture.store.rejectedCalls)
	}
	if fixture.tx.calls != 1 {
		t.Fatalf("明确拒绝事务次数 = %d, want 1", fixture.tx.calls)
	}
	if got, want := fixture.recorder.calls, []string{"rejected", "reversed", "failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("明确拒绝事务顺序 = %v, want %v", got, want)
	}
}

type memoryOutboxRepository struct {
	event       *outbox.Event
	failedCalls int
	recorder    *operationRecorder
}

func (repository *memoryOutboxRepository) Enqueue(context.Context, *outbox.Event) error { return nil }
func (repository *memoryOutboxRepository) Claim(_ context.Context, _ string, now, leaseUntil time.Time) (*outbox.Event, error) {
	if repository.event == nil || (repository.event.DeliveryStatus != outbox.DeliveryStatusPending && repository.event.DeliveryStatus != outbox.DeliveryStatusReconciling) || repository.event.NextAttemptAt.After(now) {
		return nil, nil
	}
	repository.event.DeliveryStatus = outbox.DeliveryStatusDispatching
	repository.event.LeaseToken = "lease-worker-test"
	repository.event.LeaseUntil = leaseUntil
	repository.event.AttemptCount++
	return repository.event, nil
}
func (repository *memoryOutboxRepository) ClaimByType(ctx context.Context, workerID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	if repository.event == nil || repository.event.EventType != eventType {
		return nil, nil
	}
	return repository.Claim(ctx, workerID, now, leaseUntil)
}
func (repository *memoryOutboxRepository) ClaimByIDAndType(ctx context.Context, workerID, eventID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	if repository.event == nil || repository.event.ID != eventID || repository.event.EventType != eventType {
		return nil, nil
	}
	return repository.Claim(ctx, workerID, now, leaseUntil)
}
func (repository *memoryOutboxRepository) Requeue(_ context.Context, eventID, leaseToken string, next time.Time) error {
	if eventID != repository.event.ID || leaseToken != repository.event.LeaseToken {
		return outbox.ErrLeaseConflict
	}
	repository.event.DeliveryStatus = outbox.DeliveryStatusPending
	repository.event.NextAttemptAt = next
	repository.event.LeaseToken = ""
	return nil
}
func (repository *memoryOutboxRepository) MarkDelivered(context.Context, string, string, time.Time) error {
	return nil
}
func (repository *memoryOutboxRepository) MarkFailed(_ context.Context, eventID, leaseToken string, _ time.Time) error {
	if eventID != repository.event.ID || leaseToken != repository.event.LeaseToken {
		return outbox.ErrLeaseConflict
	}
	repository.failedCalls++
	repository.recorder.add("failed")
	repository.event.DeliveryStatus = outbox.DeliveryStatusFailed
	repository.event.LeaseToken = ""
	return nil
}

// targetedMemoryOutbox 模拟同一队列中的多个事件，用于验证指定投递不会占用其他事件。
type targetedMemoryOutbox struct {
	events []*outbox.Event
}

func (repository *targetedMemoryOutbox) Enqueue(context.Context, *outbox.Event) error { return nil }

func (repository *targetedMemoryOutbox) Claim(_ context.Context, _ string, now, leaseUntil time.Time) (*outbox.Event, error) {
	return repository.claimFirst(now, leaseUntil, "")
}

func (repository *targetedMemoryOutbox) ClaimByType(_ context.Context, _ string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	return repository.claimFirst(now, leaseUntil, eventType)
}

func (repository *targetedMemoryOutbox) ClaimByIDAndType(_ context.Context, _ string, eventID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	for _, event := range repository.events {
		if event == nil || event.ID != eventID || event.EventType != eventType || event.NextAttemptAt.After(now) || (event.DeliveryStatus != outbox.DeliveryStatusPending && event.DeliveryStatus != outbox.DeliveryStatusReconciling) {
			continue
		}
		event.DeliveryStatus = outbox.DeliveryStatusDispatching
		event.LeaseToken = "lease-worker-test"
		event.LeaseUntil = leaseUntil
		event.AttemptCount++
		return event, nil
	}
	return nil, nil
}

func (repository *targetedMemoryOutbox) claimFirst(now, leaseUntil time.Time, eventType outbox.EventType) (*outbox.Event, error) {
	for _, event := range repository.events {
		if event == nil || (eventType != "" && event.EventType != eventType) || event.NextAttemptAt.After(now) || (event.DeliveryStatus != outbox.DeliveryStatusPending && event.DeliveryStatus != outbox.DeliveryStatusReconciling) {
			continue
		}
		event.DeliveryStatus = outbox.DeliveryStatusDispatching
		event.LeaseToken = "lease-worker-test"
		event.LeaseUntil = leaseUntil
		event.AttemptCount++
		return event, nil
	}
	return nil, nil
}

func (repository *targetedMemoryOutbox) Requeue(_ context.Context, eventID, leaseToken string, next time.Time) error {
	for _, event := range repository.events {
		if event != nil && event.ID == eventID && event.LeaseToken == leaseToken {
			event.DeliveryStatus = outbox.DeliveryStatusPending
			event.LeaseToken = ""
			event.NextAttemptAt = next
			return nil
		}
	}
	return outbox.ErrLeaseConflict
}

func (repository *targetedMemoryOutbox) MarkDelivered(context.Context, string, string, time.Time) error {
	return nil
}

func (repository *targetedMemoryOutbox) MarkFailed(context.Context, string, string, time.Time) error {
	return nil
}

type memorySubmissionStore struct {
	record           generation.SubmissionRecord
	event            *outbox.Event
	status           string
	submittedJobID   string
	reconcilingCalls int
	rejectedCalls    int
	confiscatedCalls int
	readFacts        int
	rejectError      error
	recorder         *operationRecorder
}

func (store *memorySubmissionStore) ClaimedSubmission(_ context.Context, eventID string) (*generation.SubmissionRecord, error) {
	if eventID != store.record.EventID {
		return nil, generation.ErrSubmissionConflict
	}
	return &store.record, nil
}
func (store *memorySubmissionStore) MarkSubmitted(_ context.Context, command generation.SubmittedCommand) error {
	store.status = "submitted"
	store.submittedJobID = command.JobID
	if store.event != nil {
		store.event.DeliveryStatus = outbox.DeliveryStatusDelivered
		store.event.LeaseToken = ""
	}
	return nil
}
func (store *memorySubmissionStore) MarkReconciling(_ context.Context, _ generation.ReconcilingCommand) error {
	store.status = "reconciling"
	store.reconcilingCalls++
	if store.event != nil {
		store.event.DeliveryStatus = outbox.DeliveryStatusReconciling
		store.event.LeaseToken = ""
	}
	return nil
}
func (store *memorySubmissionStore) MarkRejected(_ context.Context, _ generation.RejectedCommand) error {
	if store.rejectError != nil {
		return store.rejectError
	}
	store.status = "failed"
	store.rejectedCalls++
	store.recorder.add("rejected")
	return nil
}
func (store *memorySubmissionStore) MarkConfiscated(_ context.Context, _ generation.ConfiscatedCommand) error {
	store.status = "confiscated"
	store.confiscatedCalls++
	store.recorder.add("confiscated")
	return nil
}
func (store *memorySubmissionStore) ExistingSubmission(context.Context, string) error {
	store.readFacts++
	return nil
}

type memoryReverser struct {
	calls           int
	confiscateCalls int
	recorder        *operationRecorder
}

func (reverser *memoryReverser) ConfiscateInTx(context.Context, string, string, time.Time) (*ledger.Reservation, error) {
	reverser.confiscateCalls++
	reverser.recorder.add("confiscated_reservation")
	return &ledger.Reservation{Status: ledger.ReservationStatusConfiscated}, nil
}

type memoryReviewer struct {
	decision contentreview.Decision
	err      error
	calls    int
}

func (reviewer *memoryReviewer) Review(context.Context, contentreview.Request) (contentreview.Decision, error) {
	reviewer.calls++
	return reviewer.decision, reviewer.err
}

func (reverser *memoryReverser) ReverseInTx(context.Context, string, ledger.ReversalReason, time.Time) (*ledger.Reservation, error) {
	reverser.calls++
	reverser.recorder.add("reversed")
	return &ledger.Reservation{Status: ledger.ReservationStatusReversed}, nil
}

type memoryTxRunner struct {
	calls                 int
	rejectCanceledContext bool
}

func (memoryTxRunner *memoryTxRunner) WithinTx(ctx context.Context, callback func(context.Context) error) error {
	memoryTxRunner.calls++
	if memoryTxRunner.rejectCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	return callback(ctx)
}

var _ outbox.Repository = (*memoryOutboxRepository)(nil)
var _ generation.SubmissionStore = (*memorySubmissionStore)(nil)
var _ shared.TxRunner = (*memoryTxRunner)(nil)

type operationRecorder struct{ calls []string }

func (recorder *operationRecorder) add(operation string) {
	if recorder != nil {
		recorder.calls = append(recorder.calls, operation)
	}
}
