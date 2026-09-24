package worker

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const (
	reconcileJobID = "job-b2b-reconcile"
	reconcileLease = "lease-reconcile-test"
	// reconcileAttempts 刻意不等于 1：退避必须取自事件自身的尝试计数，
	// 而不是每次从 0 重新开始。用 3 才能把 40s 与 10s 区分开。
	reconcileAttempts int32 = 3
)

// reconcileDigest 是一个形状合法的取证摘要。它只作取证，不参与判重，
// 因此不必是真实字节的摘要。
var reconcileDigest = strings.Repeat("ab", 32)

var reconcileNow = time.Date(2026, time.September, 19, 14, 0, 0, 0, time.UTC)

// reconcileEvent 构造按步骤去重的对账工作项。
// AttemptCount 已经是 3，模拟「这条工作项已经被领取并重排过两次」。
func reconcileEvent() *outbox.Event {
	return &outbox.Event{
		ID:             generation.ProviderInboxRecoveryEventID(b2bWorkerStepID),
		AggregateID:    b2bWorkerStepID,
		EventType:      outbox.EventType(generation.ProviderReconcileEventType),
		DeliveryStatus: outbox.DeliveryStatusPending,
		AttemptCount:   reconcileAttempts,
		NextAttemptAt:  reconcileNow,
		CreatedAt:      reconcileNow,
		UpdatedAt:      reconcileNow,
	}
}

// reconcileIntent 用真实的映射器产出冻结提交意图。
//
// 刻意不手写 Request：对账路径要先经 RestoreRequest 还原再查询，手写字段
// 一旦与映射器漂移，用例就会在「RestoreRequest 失败」上红，而不是在真正
// 要验证的行为上红。
func reconcileIntent(t *testing.T) *generation.ProviderSubmissionIntent {
	t.Helper()
	record := generation.SubmissionRecord{
		Route: b2bWorkerRoute(), StepID: b2bWorkerStepID,
		ExecutionPayload: b2bWorkerRecipePayload(t), Fence: reconcileAttempts,
	}
	request, err := mapB2BRequest(context.Background(), platform.B2BSubmissionConfig{
		Catalogs: &b2bCatalogStore{catalog: b2bTestCatalog()},
		Callback: polarstarb2b.CallbackConfig{Mode: "lookup_only"},
	}, &record)
	if err != nil {
		t.Fatalf("映射冻结配方: %v", err)
	}
	return &generation.ProviderSubmissionIntent{
		Request: generation.FrozenProviderRequest{
			Route: record.Route, StepID: record.StepID, Capability: request.Capability(),
			IdempotencyKey: request.IdempotencyKey(), Digest: request.Digest(), Payload: request.Payload(),
		},
		PreparedAt:          reconcileNow,
		Fence:               record.Fence,
		ExternalExecutionID: reconcileJobID,
	}
}

type reconcileOutbox struct {
	event     *outbox.Event
	claimErr  error
	missOnID  bool
	settleErr error

	claimedType outbox.EventType
	claimedID   string
	delivered   []string
	failed      []string
	requeued    []time.Time
	attended    []outbox.AttentionReason
}

func (stub *reconcileOutbox) Enqueue(context.Context, *outbox.Event) error {
	return outbox.ErrInvalidEvent
}

func (stub *reconcileOutbox) Claim(_ context.Context, _ string, _, leaseUntil time.Time) (*outbox.Event, error) {
	if stub.event != nil {
		stub.event.DeliveryStatus = outbox.DeliveryStatusDispatching
		stub.event.LeaseToken = reconcileLease
		stub.event.LeaseUntil = leaseUntil
	}
	return stub.event, stub.claimErr
}

// ClaimByType 与 ClaimByIDAndType 刻意无条件返回夹具事件（不做类型过滤）：
// 「领到了不该处理的事件」必须由 Worker 自己拒绝，夹具替它过滤就等于把
// 这条不变量从被测代码里挪走了。
func (stub *reconcileOutbox) ClaimByType(_ context.Context, _ string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	stub.claimedType = eventType
	return stub.Claim(context.Background(), "", now, leaseUntil)
}

func (stub *reconcileOutbox) ClaimByIDAndType(_ context.Context, _, eventID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	stub.claimedType, stub.claimedID = eventType, eventID
	if stub.missOnID {
		return nil, stub.claimErr
	}
	return stub.Claim(context.Background(), "", now, leaseUntil)
}

func (stub *reconcileOutbox) RenewLease(context.Context, string, string, time.Time, time.Time) error {
	return nil
}

func (stub *reconcileOutbox) Requeue(_ context.Context, _, _ string, next time.Time) error {
	if stub.settleErr != nil {
		return stub.settleErr
	}
	stub.requeued = append(stub.requeued, next)
	return nil
}

func (stub *reconcileOutbox) MarkDelivered(_ context.Context, eventID, _ string, _ time.Time) error {
	if stub.settleErr != nil {
		return stub.settleErr
	}
	stub.delivered = append(stub.delivered, eventID)
	return nil
}

func (stub *reconcileOutbox) MarkFailed(_ context.Context, eventID, _ string, _ time.Time) error {
	if stub.settleErr != nil {
		return stub.settleErr
	}
	stub.failed = append(stub.failed, eventID)
	return nil
}

func (stub *reconcileOutbox) MarkNeedsAttention(_ context.Context, _ string, _ string, reason outbox.AttentionReason, _ time.Time) error {
	if stub.settleErr != nil {
		return stub.settleErr
	}
	stub.attended = append(stub.attended, reason)
	return nil
}

// RedriveAttention 只是接口占位：本包的用例都不驱动人工重驱。
func (stub *reconcileOutbox) RedriveAttention(context.Context, outbox.RedriveCommand) (*outbox.Event, error) {
	return nil, outbox.ErrRedriveNotEligible
}

var _ outbox.Repository = (*reconcileOutbox)(nil)

type reconcileSubmissionStore struct {
	intent  *generation.ProviderSubmissionIntent
	readErr error
	reads   int
}

func (store *reconcileSubmissionStore) PrepareProviderSubmission(context.Context, generation.PrepareProviderSubmissionCommand) error {
	return generation.ErrInvalidSubmissionCommand
}

func (store *reconcileSubmissionStore) ReadProviderSubmission(context.Context, string) (*generation.ProviderSubmissionIntent, error) {
	store.reads++
	if store.readErr != nil {
		return nil, store.readErr
	}
	return store.intent, nil
}

var _ generation.ProviderSubmissionStore = (*reconcileSubmissionStore)(nil)

type reconcileTerminalStore struct {
	result generation.ProviderTerminalApplyResult
	err    error
	calls  int
	facts  []generation.ProviderTerminalFact
}

func (store *reconcileTerminalStore) ApplyProviderTerminal(_ context.Context, fact generation.ProviderTerminalFact) (generation.ProviderTerminalApplyResult, error) {
	store.calls++
	store.facts = append(store.facts, fact)
	if store.err != nil {
		return "", store.err
	}
	return store.result, nil
}

var _ generation.ProviderTerminalStore = (*reconcileTerminalStore)(nil)

type reconcileParser struct {
	outcome platform.B2BLookupOutcome
	err     error
	calls   int
}

func (parser *reconcileParser) ParseLookup(*generation.ProviderSubmissionIntent, polarstarb2b.Job) (platform.B2BLookupOutcome, error) {
	parser.calls++
	if parser.err != nil {
		return platform.B2BLookupOutcome{}, parser.err
	}
	return parser.outcome, nil
}

// reconcileLocalRouter 只解析本地出站能力，用于验证「冻结路由不是 B2B」时
// 对账工作项被放弃，而不是被当成一次可重试的配置抖动。
type reconcileLocalRouter struct{}

func (reconcileLocalRouter) ProviderForRoute(string, string) (platform.ProviderHandle, error) {
	return platform.ProviderHandle{ID: creations.LocalExecutionProvider, AccountRef: b2bWorkerAccount}, nil
}

func (reconcileLocalRouter) B2BSubmission() (platform.B2BSubmissionConfig, bool) {
	return platform.B2BSubmissionConfig{}, false
}

var _ submissionRouter = reconcileLocalRouter{}

type reconcileFixture struct {
	worker    *ProviderReconcileWorker
	outbox    *reconcileOutbox
	subs      *reconcileSubmissionStore
	terminals *reconcileTerminalStore
	parser    ProviderLookupParser
	router    *b2bRouter
	event     *outbox.Event
	intent    *generation.ProviderSubmissionIntent
	tx        *memoryTxRunner
	lookups   *int
}

func newReconcileFixture(t *testing.T, handler http.HandlerFunc) *reconcileFixture {
	t.Helper()
	lookups := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/jobs/lookup" {
			lookups++
		}
		handler(writer, request)
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client, err := polarstarb2b.NewClient(polarstarb2b.ClientOptions{
		BaseURL: server.URL, APIKey: "test-only-b2b-key", AccountRef: b2bWorkerAccount,
		ExpectedTenantID: b2bWorkerTenant, RootCAs: roots, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)

	intent := reconcileIntent(t)
	// 默认用真实的归一化解析器：查询响应 → 客户端解码 → 归一化 → 终态事实
	// 整条链路的接缝才是真正被测的东西。
	parser, err := platform.NewB2BObservationParser(b2bWorkerTenant, b2bWorkerAccount)
	if err != nil {
		t.Fatal(err)
	}
	event := reconcileEvent()
	fixture := &reconcileFixture{
		outbox:    &reconcileOutbox{event: event},
		subs:      &reconcileSubmissionStore{intent: intent},
		terminals: &reconcileTerminalStore{result: generation.ProviderTerminalApplied},
		parser:    parser,
		router: &b2bRouter{client: client, enabled: true, config: platform.B2BSubmissionConfig{
			Catalogs: &b2bCatalogStore{catalog: b2bTestCatalog()},
			Callback: polarstarb2b.CallbackConfig{Mode: "lookup_only"},
		}},
		event: event, intent: intent, tx: &memoryTxRunner{}, lookups: &lookups,
	}
	fixture.worker = NewProviderReconcileWorker(
		fixture.outbox, fixture.subs, fixture.terminals, fixture.tx, fixture.router, parser,
		"reconcile-worker-1", func() time.Time { return reconcileNow },
	)
	return fixture
}

// useParser 替换归一化解析器，用于构造解析层的确定性/瞬时失败。
func (fixture *reconcileFixture) useParser(parser ProviderLookupParser) {
	fixture.parser = parser
	fixture.worker.parser = parser
}

// useRouter 替换出站能力解析器，用于构造配置缺失与「路由不是 B2B」两种情形。
func (fixture *reconcileFixture) useRouter(router submissionRouter) {
	fixture.worker.providers = router
}

func (fixture *reconcileFixture) run(t *testing.T) {
	t.Helper()
	if err := fixture.worker.DeliverOnce(context.Background(), ""); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
}

// 终态必须经统一 CAS 落地，并且事实的每一个归因字段都来自冻结意图而不是
// 本次查询的入参。归因错了，账本上就无法靠重放纠正。
func TestReconcileWorker终态落地并结案(t *testing.T) {
	const resultURL = "https://cdn.example.test/out.png"
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(b2bWorkerCompletedJobBody(reconcileJobID, resultURL)))
	})
	fixture.run(t)

	if *fixture.lookups != 1 {
		t.Fatalf("查询次数 = %d，期望 1", *fixture.lookups)
	}
	if fixture.terminals.calls != 1 || fixture.tx.calls != 1 {
		t.Fatalf("终态写入 %d 次 / 事务 %d 次，期望各 1 次", fixture.terminals.calls, fixture.tx.calls)
	}
	fact := fixture.terminals.facts[0]
	if fact.Provider != creations.PolarStarB2BProvider || fact.AccountRef != b2bWorkerAccount || fact.StepID != b2bWorkerStepID {
		t.Fatalf("归因字段 = %s/%s/%s", fact.Provider, fact.AccountRef, fact.StepID)
	}
	if fact.ExternalExecutionID != reconcileJobID || fact.Capability != "text_to_image" || fact.Status != generation.ProviderTerminalCompleted {
		t.Fatalf("外部任务号/能力/结论 = %s/%s/%s", fact.ExternalExecutionID, fact.Capability, fact.Status)
	}
	if fact.AttemptFence != int64(reconcileAttempts) {
		t.Fatalf("尝试栅栏 = %d，期望 %d", fact.AttemptFence, reconcileAttempts)
	}
	// 查询路径不携带租约令牌：带上一个本地租约会让同一结论经回调与查询
	// 两条路到达时被判成冲突。
	if fact.LeaseToken != "" {
		t.Fatalf("租约令牌 = %q，期望空", fact.LeaseToken)
	}
	if fact.TerminalDigest != generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, resultURL) {
		t.Fatalf("终态摘要 = %s", fact.TerminalDigest)
	}
	// 取证摘要必须来自本次查询响应，而不是本地占位值：它是「这份结论出自
	// 哪一串供应商字节」的唯一凭据。
	if len(fact.PayloadDigest) != 64 || strings.ToLower(fact.PayloadDigest) != fact.PayloadDigest || fact.PayloadDigest == reconcileDigest {
		t.Fatalf("取证摘要 = %q，期望来自本次查询响应", fact.PayloadDigest)
	}
	if len(fixture.outbox.delivered) != 1 || len(fixture.outbox.failed) != 0 || len(fixture.outbox.requeued) != 0 {
		t.Fatalf("结案 = 投递 %v / 失败 %v / 重排 %v", fixture.outbox.delivered, fixture.outbox.failed, fixture.outbox.requeued)
	}
}

// 中间态不是终态：把「还在跑」写成「已结束」在账本上不可逆。
func TestReconcileWorker中间态按退避重排(t *testing.T) {
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(b2bWorkerJobBody(reconcileJobID, "processing")))
	})
	fixture.run(t)

	if fixture.terminals.calls != 0 {
		t.Fatalf("中间态写入终态 %d 次", fixture.terminals.calls)
	}
	if len(fixture.outbox.requeued) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 重排 %v / 失败 %v", fixture.outbox.requeued, fixture.outbox.failed)
	}
	// 尝试计数取自事件自身（3）：10s × 2^(3-1) = 40s。
	if want := reconcileNow.Add(40 * time.Second); !fixture.outbox.requeued[0].Equal(want) {
		t.Fatalf("重排时刻 = %v，期望 %v", fixture.outbox.requeued[0], want)
	}
}

// 外部任务号还没绑定说明提交路径尚未收敛。此时既不能按任务号核对，也不能
// 冲正——用户钻石已经预扣，只能等。
func TestReconcileWorker未绑定外部任务号时重排(t *testing.T) {
	fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("未绑定外部任务号时不得发起查询")
	})
	fixture.intent.ExternalExecutionID = ""
	fixture.run(t)

	if *fixture.lookups != 0 || fixture.terminals.calls != 0 {
		t.Fatalf("查询 %d 次 / 终态写入 %d 次，期望均为 0", *fixture.lookups, fixture.terminals.calls)
	}
	if len(fixture.outbox.requeued) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 重排 %v / 失败 %v", fixture.outbox.requeued, fixture.outbox.failed)
	}
}

// 查不到不能证明中台没有这笔任务（可能尚未落库）。按退避继续核对。
func TestReconcileWorker查不到时重排且不冲正(t *testing.T) {
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"success":false,"code":"NOT_FOUND"}`))
	})
	fixture.run(t)

	if *fixture.lookups != 1 {
		t.Fatalf("查询次数 = %d，期望 1", *fixture.lookups)
	}
	if fixture.terminals.calls != 0 {
		t.Fatalf("查不到却写了终态 %d 次", fixture.terminals.calls)
	}
	if len(fixture.outbox.requeued) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 重排 %v / 失败 %v", fixture.outbox.requeued, fixture.outbox.failed)
	}
}

// 平台明确要求的等待时间优先于本地指数退避。
func TestReconcileWorker遵循RetryAfter重排(t *testing.T) {
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Retry-After", "120")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"success":false,"code":"RATE_LIMITED"}`))
	})
	fixture.run(t)

	if len(fixture.outbox.requeued) != 1 {
		t.Fatalf("重排次数 = %d，期望 1", len(fixture.outbox.requeued))
	}
	if want := reconcileNow.Add(120 * time.Second); !fixture.outbox.requeued[0].Equal(want) {
		t.Fatalf("重排时刻 = %v，期望 %v（Retry-After）", fixture.outbox.requeued[0], want)
	}
}

// 查询请求根本没发出去：本地构造的查询不合法，重排一万次也不会变合法。
func TestReconcileWorker查询未发出时放弃(t *testing.T) {
	fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("非法查询键不得发出请求")
	})
	// 外部任务号必须能构成合法的查询键；这里刻意放一个含空格的取值。
	fixture.intent.ExternalExecutionID = "bad job"
	fixture.run(t)

	if *fixture.lookups != 0 {
		t.Fatalf("查询次数 = %d，期望 0", *fixture.lookups)
	}
	if len(fixture.outbox.failed) != 1 || len(fixture.outbox.requeued) != 0 {
		t.Fatalf("结案 = 失败 %v / 重排 %v", fixture.outbox.failed, fixture.outbox.requeued)
	}
}

// 解析层的失败同样要分可重试性：事实矛盾重排不会改变结论，瞬时抖动必须重排。
func TestReconcileWorker解析失败按可重试性分流(t *testing.T) {
	cases := map[string]struct {
		cause       error
		wantFailed  int
		wantRequeue int
	}{
		"步骤身份不符": {generation.ErrProviderInboxStepMismatch, 1, 0},
		"结论本身非法": {generation.ErrInvalidProviderTerminalObservation, 1, 0},
		"瞬时解码抖动": {errors.New("temporary decode failure"), 0, 1},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write([]byte(b2bWorkerCompletedJobBody(reconcileJobID, "https://cdn.example.test/out.png")))
			})
			fixture.useParser(&reconcileParser{err: testCase.cause})
			fixture.run(t)

			if len(fixture.outbox.failed) != testCase.wantFailed || len(fixture.outbox.requeued) != testCase.wantRequeue {
				t.Fatalf("结案 = 失败 %v / 重排 %v", fixture.outbox.failed, fixture.outbox.requeued)
			}
			if fixture.terminals.calls != 0 {
				t.Fatalf("解析失败却写了终态 %d 次", fixture.terminals.calls)
			}
		})
	}
}

// 隔离结论意味着冲突证据已经落库，重排不会改变结论。
func TestReconcileWorker隔离结论按失败结案(t *testing.T) {
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(b2bWorkerCompletedJobBody(reconcileJobID, "https://cdn.example.test/out.png")))
	})
	fixture.terminals.result = generation.ProviderTerminalQuarantined
	fixture.run(t)

	if len(fixture.outbox.failed) != 1 || len(fixture.outbox.delivered) != 0 || len(fixture.outbox.requeued) != 0 {
		t.Fatalf("结案 = 投递 %v / 失败 %v / 重排 %v", fixture.outbox.delivered, fixture.outbox.failed, fixture.outbox.requeued)
	}
}

// 同一结论重放没有副作用，按投递完成结案。
func TestReconcileWorker幂等重放按投递完成结案(t *testing.T) {
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(b2bWorkerCompletedJobBody(reconcileJobID, "https://cdn.example.test/out.png")))
	})
	fixture.terminals.result = generation.ProviderTerminalNoop
	fixture.run(t)

	if len(fixture.outbox.delivered) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 投递 %v / 失败 %v", fixture.outbox.delivered, fixture.outbox.failed)
	}
}

// 存储抖动是瞬时状态：重排即可收敛，放弃会让一笔本可收敛的任务永久停摆。
func TestReconcileWorker终态存储抖动按退避重排(t *testing.T) {
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(b2bWorkerCompletedJobBody(reconcileJobID, "https://cdn.example.test/out.png")))
	})
	fixture.terminals.err = errors.New("mongo write conflict")
	fixture.run(t)

	if len(fixture.outbox.requeued) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 重排 %v / 失败 %v", fixture.outbox.requeued, fixture.outbox.failed)
	}
}

// 运行配置缺失是可恢复的部署状态，不是「这笔任务做不了」。按永久失败结案
// 会让一次配置回滚永久丢掉一笔在途任务。
func TestReconcileWorker运行配置缺失时重排(t *testing.T) {
	fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("配置缺失不得发起查询")
	})
	fixture.router.client = nil
	fixture.run(t)

	if *fixture.lookups != 0 {
		t.Fatalf("查询次数 = %d，期望 0", *fixture.lookups)
	}
	if len(fixture.outbox.requeued) != 1 || len(fixture.outbox.failed) != 0 {
		t.Fatalf("结案 = 重排 %v / 失败 %v", fixture.outbox.requeued, fixture.outbox.failed)
	}
}

// 冻结路由根本不是 B2B：这一步永远不可能有供应商任务号，重排只是空转。
func TestReconcileWorker冻结路由不是B2B时放弃(t *testing.T) {
	fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("本地路由不得发起 B2B 查询")
	})
	fixture.useRouter(reconcileLocalRouter{})
	fixture.run(t)

	if len(fixture.outbox.failed) != 1 || len(fixture.outbox.requeued) != 0 {
		t.Fatalf("结案 = 失败 %v / 重排 %v", fixture.outbox.failed, fixture.outbox.requeued)
	}
}

// 冻结意图不存在说明这一步从来没被授权过，对账无事可做。
func TestReconcileWorker冻结意图缺失时放弃(t *testing.T) {
	fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("意图缺失不得发起查询")
	})
	fixture.subs.intent = nil
	fixture.run(t)

	if len(fixture.outbox.failed) != 1 || fixture.terminals.calls != 0 {
		t.Fatalf("结案 = 失败 %v / 终态写入 %d 次", fixture.outbox.failed, fixture.terminals.calls)
	}
}

func TestReconcileWorker读回故障按可重试性分流(t *testing.T) {
	transient := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("读回失败不得发起查询")
	})
	transient.subs.readErr = errors.New("mongo timeout")
	transient.run(t)
	if len(transient.outbox.requeued) != 1 || len(transient.outbox.failed) != 0 {
		t.Fatalf("瞬时读故障结案 = 重排 %v / 失败 %v", transient.outbox.requeued, transient.outbox.failed)
	}

	permanent := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("读回失败不得发起查询")
	})
	permanent.subs.readErr = generation.ErrSubmissionConflict
	permanent.run(t)
	if len(permanent.outbox.failed) != 1 || len(permanent.outbox.requeued) != 0 {
		t.Fatalf("永久读故障结案 = 失败 %v / 重排 %v", permanent.outbox.failed, permanent.outbox.requeued)
	}
}

// 事件标识必须能被还原成步骤身份。还原不出来说明工作项本身是坏的，
// 重排只会让它永远占着队列。
func TestReconcileWorker事件标识无法还原步骤身份时放弃(t *testing.T) {
	for name, eventID := range map[string]string{
		"前缀不符":  "generation.inbox.consume:deadbeef",
		"步骤为空":  generation.ProviderReconcileEventType + ":",
		"步骤含空格": generation.ProviderReconcileEventType + ":step with space",
		"无分隔符":  generation.ProviderReconcileEventType,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
				t.Error("标识坏掉的工作项不得发起查询")
			})
			fixture.event.ID = eventID
			fixture.run(t)
			if len(fixture.outbox.failed) != 1 || len(fixture.outbox.requeued) != 0 {
				t.Fatalf("结案 = 失败 %v / 重排 %v", fixture.outbox.failed, fixture.outbox.requeued)
			}
		})
	}
}

func TestReconcileStepID往返校验(t *testing.T) {
	if stepID, err := reconcileStepID(generation.ProviderInboxRecoveryEventID(b2bWorkerStepID)); err != nil || stepID != b2bWorkerStepID {
		t.Fatalf("往返 = %q / %v", stepID, err)
	}
	for _, eventID := range []string{"", "generation.reconcile:", "generation.reconcile", "other:" + b2bWorkerStepID} {
		if _, err := reconcileStepID(eventID); !errors.Is(err, ErrInvalidReconcileEvent) {
			t.Fatalf("标识 %q 错误 = %v，期望 ErrInvalidReconcileEvent", eventID, err)
		}
	}
}

// 租约被并发消费者接管不是故障：上抛会让轮询进程因为一次正常交接而整体退出。
func TestReconcileWorker租约冲突不阻断轮询(t *testing.T) {
	fixture := newReconcileFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(b2bWorkerCompletedJobBody(reconcileJobID, "https://cdn.example.test/out.png")))
	})
	fixture.outbox.settleErr = outbox.ErrLeaseConflict
	fixture.run(t)
}

func TestReconcileWorker只处理自己的事件类型(t *testing.T) {
	fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("事件类型不符不得发起查询")
	})
	fixture.event.EventType = outbox.EventTypeGenerationSubmission
	if err := fixture.worker.DeliverOnce(context.Background(), ""); !errors.Is(err, ErrUnexpectedClaimedEvent) {
		t.Fatalf("错误 = %v，期望 ErrUnexpectedClaimedEvent", err)
	}

	fixture = newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("指定投递不符不得发起查询")
	})
	if err := fixture.worker.DeliverOnce(context.Background(), generation.ProviderInboxRecoveryEventID("step-other")); !errors.Is(err, ErrUnexpectedClaimedEvent) {
		t.Fatalf("指定投递不符错误 = %v，期望 ErrUnexpectedClaimedEvent", err)
	}
}

func TestReconcileWorker指定投递不命中时静默返回(t *testing.T) {
	fixture := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("指定投递不命中不得发起查询")
	})
	fixture.outbox.missOnID = true
	if err := fixture.worker.DeliverOnce(context.Background(), generation.ProviderInboxRecoveryEventID("step-other")); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.subs.reads != 0 || len(fixture.outbox.delivered) != 0 {
		t.Fatalf("读回 %d 次 / 结案 %v", fixture.subs.reads, fixture.outbox.delivered)
	}
}

func TestReconcileWorker依赖缺失时拒绝(t *testing.T) {
	complete := newReconcileFixture(t, func(http.ResponseWriter, *http.Request) {})
	cases := map[string]*ProviderReconcileWorker{
		"空工作者":   nil,
		"缺发件箱":   NewProviderReconcileWorker(nil, complete.subs, complete.terminals, complete.tx, complete.router, complete.parser, "w", nil),
		"缺提交仓储":  NewProviderReconcileWorker(complete.outbox, nil, complete.terminals, complete.tx, complete.router, complete.parser, "w", nil),
		"缺终态仓储":  NewProviderReconcileWorker(complete.outbox, complete.subs, nil, complete.tx, complete.router, complete.parser, "w", nil),
		"缺事务运行器": NewProviderReconcileWorker(complete.outbox, complete.subs, complete.terminals, nil, complete.router, complete.parser, "w", nil),
		"缺路由解析":  NewProviderReconcileWorker(complete.outbox, complete.subs, complete.terminals, complete.tx, nil, complete.parser, "w", nil),
		"缺解析器":   NewProviderReconcileWorker(complete.outbox, complete.subs, complete.terminals, complete.tx, complete.router, nil, "w", nil),
		"缺工作者标识": NewProviderReconcileWorker(complete.outbox, complete.subs, complete.terminals, complete.tx, complete.router, complete.parser, "", nil),
	}
	for name, worker := range cases {
		t.Run(name, func(t *testing.T) {
			if err := worker.DeliverOnce(context.Background(), ""); !errors.Is(err, ErrReconcileWorkerDependenciesUnavailable) {
				t.Fatalf("错误 = %v，期望 ErrReconcileWorkerDependenciesUnavailable", err)
			}
		})
	}
}
