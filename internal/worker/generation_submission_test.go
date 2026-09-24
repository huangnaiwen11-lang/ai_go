package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/contentreview"
	"ai-business-service/internal/biz/creations"
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

func TestDeliverOnce拒绝B2B冻结路由且不触发任何副作用(t *testing.T) {
	fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		t.Fatalf("unsupported route must not call provider: %s", request.URL.Path)
	})
	fixture.store.record.Route = creations.ExecutionRoute{
		Provider: creations.PolarStarB2BProvider, AccountRef: "account-a",
		ContractVersion: creations.B2BContractVersion, MappingVersion: "polarstar.image.v1",
	}
	fixture.store.record.ContentAccess = identity.ContentAccessReviewRestricted
	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); !isFailClosedRouteError(err) {
		t.Fatalf("DeliverOnce() error = %v, want fail-closed route error", err)
	}
	if fixture.reviewer.calls != 0 || fixture.reverser.calls != 0 || fixture.reverser.confiscateCalls != 0 || fixture.store.rejectedCalls != 0 || fixture.store.confiscatedCalls != 0 || fixture.lookupCount != 0 {
		t.Fatalf("unsupported route caused side effects: reviewer=%d reverse=%d confiscate=%d rejected=%d", fixture.reviewer.calls, fixture.reverser.calls, fixture.reverser.confiscateCalls, fixture.store.rejectedCalls)
	}
}

func TestDeliverOnce拒绝部分或未知冻结路由(t *testing.T) {
	cases := []creations.ExecutionRoute{
		{Provider: creations.LocalExecutionProvider},
		{Provider: "unknown-provider", AccountRef: "acct", ContractVersion: "contract", MappingVersion: "mapping"},
		{Provider: creations.LocalExecutionProvider, AccountRef: creations.DefaultLocalAccount, ContractVersion: creations.B2BContractVersion, MappingVersion: creations.LocalMappingVersion},
	}
	for _, route := range cases {
		route := route
		t.Run(route.Provider+":"+route.AccountRef, func(t *testing.T) {
			fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) { t.Fatalf("invalid route called provider") })
			fixture.store.record.Route = route
			if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); !errors.Is(err, ErrUnsupportedSubmissionRoute) {
				t.Fatalf("error = %v, want unsupported route", err)
			}
			if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
				t.Fatal("invalid route entered refund path")
			}
		})
	}
}

func TestDeliverOnce历史空路由仍按本地处理(t *testing.T) {
	postCalls := 0
	fixture := newSubmissionWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		postCalls++
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-local-route","status":"queued"}}`))
	})
	fixture.store.record.Route = creations.ExecutionRoute{}
	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("empty historical route: %v", err)
	}
	if postCalls != 1 || fixture.store.submittedJobID != "job-local-route" {
		t.Fatalf("local submission calls/job=%d/%q", postCalls, fixture.store.submittedJobID)
	}
}

func TestReconcile拒绝B2B冻结路由且不Lookup(t *testing.T) {
	fixture := newReconcilingFixture(t, http.StatusOK)
	fixture.store.record.Route = creations.ExecutionRoute{Provider: creations.PolarStarB2BProvider, AccountRef: "account-a", ContractVersion: creations.B2BContractVersion, MappingVersion: "polarstar.image.v1"}
	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); !isFailClosedRouteError(err) {
		t.Fatalf("error=%v, want fail-closed route error", err)
	}
	if fixture.lookupCount != 0 || fixture.reverser.calls != 0 {
		t.Fatalf("unsupported route lookup/refund: lookup=%d reverse=%d", fixture.lookupCount, fixture.reverser.calls)
	}
}

// isFailClosedRouteError 判定「冻结路由解析不出出站能力」这一类错误。
//
// 两种来源都必须以同一方式收敛——不调用上游、不冲正、不重排、不没收：
//   - ErrUnsupportedSubmissionRoute：路由自身非法，或解析出的句柄既非本地也非 B2B
//   - platform.ErrProviderNotEnabled / ErrProviderRouteMismatch：运行配置没有授权
//     该 provider 或账号，属配置与冻结身份不一致
//
// 它们都可能是「上游已经受理但本地还不知道」的情形，因此绝不能被当成条件写冲突。
func isFailClosedRouteError(err error) bool {
	return errors.Is(err, ErrUnsupportedSubmissionRoute) ||
		errors.Is(err, platform.ErrProviderNotEnabled) ||
		errors.Is(err, platform.ErrProviderRouteMismatch)
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
	fixture.worker = NewGenerationSubmissionWorker(fixture.outbox, fixture.store, fixture.reverser, fixture.tx, LocalClientRouter{Local: client}, fixture.reviewer, "worker-test", func() time.Time { return fixture.now })

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

	// 未知结果会按退避重新可领取；把夹具时钟推过退避窗口才能触发第二次领取。
	// 若这里不推进，事件会因 next_attempt_at 未到而被跳过——那正是本用例要防的忙轮询。
	fixture.now = fixture.now.Add(submissionRetryBase)
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
	fixture.worker = NewGenerationSubmissionWorker(targetedOutbox, fixture.store, fixture.reverser, fixture.tx, LocalClientRouter{Local: mustWorkerClient(t, fixture)}, fixture.reviewer, "worker-test", func() time.Time { return fixture.now })

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
	// 该夹具的 record.Fence 为 0，对应首次退避 10s。具体数值由
	// submission_backoff_test.go 逐一钉住，这里只断言「不是立即重试」。
	if got, want := fixture.event.NextAttemptAt, fixture.now.Add(submissionRetryBase); !got.Equal(want) {
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
	fixture.worker = NewGenerationSubmissionWorker(fixture.outbox, fixture.store, fixture.reverser, fixture.tx, LocalClientRouter{Local: client}, fixture.reviewer, "worker-test", func() time.Time { return fixture.now })
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
	event         *outbox.Event
	failedCalls   int
	attendedCalls int
	recorder      *operationRecorder
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
func (repository *memoryOutboxRepository) RenewLease(_ context.Context, eventID, leaseToken string, now, leaseUntil time.Time) error {
	if repository.event == nil || eventID != repository.event.ID || leaseToken != repository.event.LeaseToken || !repository.event.LeaseUntil.After(now) || !leaseUntil.After(now) {
		return outbox.ErrLeaseConflict
	}
	repository.event.LeaseUntil = leaseUntil
	return nil
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

func (repository *memoryOutboxRepository) MarkNeedsAttention(_ context.Context, eventID, leaseToken string, reason outbox.AttentionReason, _ time.Time) error {
	if eventID != repository.event.ID || leaseToken != repository.event.LeaseToken {
		return outbox.ErrLeaseConflict
	}
	repository.attendedCalls++
	repository.recorder.add("needs_attention")
	repository.event.DeliveryStatus = outbox.DeliveryStatusNeedsAttention
	repository.event.AttentionReason = reason
	repository.event.LeaseToken = ""
	return nil
}

// RedriveAttention 只是接口占位：重驱语义由 internal/data 的 Mongo 实现与
// outbox 的重驱用例测试覆盖，提交链路的用例不驱动人工重驱。
func (repository *memoryOutboxRepository) RedriveAttention(context.Context, outbox.RedriveCommand) (*outbox.Event, error) {
	return nil, outbox.ErrRedriveNotEligible
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

func (repository *targetedMemoryOutbox) RenewLease(_ context.Context, eventID, leaseToken string, now, leaseUntil time.Time) error {
	for _, event := range repository.events {
		if event != nil && event.ID == eventID && event.LeaseToken == leaseToken && event.LeaseUntil.After(now) && leaseUntil.After(now) {
			event.LeaseUntil = leaseUntil
			return nil
		}
	}
	return outbox.ErrLeaseConflict
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

func (repository *targetedMemoryOutbox) MarkNeedsAttention(_ context.Context, eventID, leaseToken string, reason outbox.AttentionReason, _ time.Time) error {
	for _, event := range repository.events {
		if event != nil && event.ID == eventID && event.LeaseToken == leaseToken {
			event.DeliveryStatus = outbox.DeliveryStatusNeedsAttention
			event.AttentionReason = reason
			event.LeaseToken = ""
			return nil
		}
	}
	return outbox.ErrLeaseConflict
}

// RedriveAttention 只是接口占位：本包的用例不驱动人工重驱。
func (repository *targetedMemoryOutbox) RedriveAttention(context.Context, outbox.RedriveCommand) (*outbox.Event, error) {
	return nil, outbox.ErrRedriveNotEligible
}

type memorySubmissionStore struct {
	record           generation.SubmissionRecord
	event            *outbox.Event
	status           string
	submittedJobID   string
	reconcilingCalls int
	rejectedCalls    int
	rejectionCause   generation.ProviderRejectionCause
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
func (store *memorySubmissionStore) MarkReconciling(_ context.Context, command generation.ReconcilingCommand) error {
	store.status = "reconciling"
	store.reconcilingCalls++
	if store.event != nil {
		store.event.DeliveryStatus = outbox.DeliveryStatusReconciling
		store.event.LeaseToken = ""
		// 再次可领取时刻由命令给出，必须原样落到事件上：夹具若把它丢掉，
		// 断言「退避生效」的用例就会在什么都没调度的前提下通过。
		store.event.NextAttemptAt = command.NextAttemptAt
	}
	return nil
}
func (store *memorySubmissionStore) MarkRejected(_ context.Context, command generation.RejectedCommand) error {
	if store.rejectError != nil {
		return store.rejectError
	}
	store.status = "failed"
	store.rejectedCalls++
	store.rejectionCause = command.ProviderRejectionCause
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
