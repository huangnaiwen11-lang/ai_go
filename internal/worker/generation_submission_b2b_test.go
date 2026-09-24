package worker

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/outbox"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const (
	b2bWorkerStepID  = "step-b2b-worker-test"
	b2bWorkerAccount = "account-a"
	b2bWorkerTenant  = "tenant-1"
	b2bCatalogVer    = "ps-map-2026-09-18"
)

// b2bTestCatalog 是一份「已发布」的最小目录版本，只包含一个文生图产品键。
// 版本号同时充当步骤冻结的 mapping_version：二者必须完全一致才允许映射。
func b2bTestCatalog() generation.PublishedMappingCatalog {
	return generation.PublishedMappingCatalog{
		Version:       b2bCatalogVer,
		SourceVersion: "src-2026-09-18",
		PublishedAt:   time.Date(2026, time.September, 18, 0, 0, 0, 0, time.UTC),
		Entries: []generation.ModelMappingEntry{{
			Capability:    "text_to_image",
			ProductKey:    "ps-image-v1",
			PublicModel:   "ps-image-v1",
			AllowedInputs: []string{"prompt", "aspectRatio"},
			Sizes:         []generation.ModelMappingSize{{Width: 1024, Height: 1024}},
			AspectRatios:  []string{"1:1"},
			Enabled:       true,
		}},
	}
}

func b2bWorkerRoute() creations.ExecutionRoute {
	return creations.ExecutionRoute{
		Provider:        creations.PolarStarB2BProvider,
		AccountRef:      b2bWorkerAccount,
		ContractVersion: creations.B2BContractVersion,
		MappingVersion:  b2bCatalogVer,
	}
}

// b2bWorkerRecipePayload 是模板编译器应当产出的公开产品配方字节。
// 它随创建事务冻结进发件箱载荷，提交时只按这些字节还原。
func b2bWorkerRecipePayload(t *testing.T) []byte {
	t.Helper()
	recipe := creations.B2BProductRecipe{
		ProductKey: "ps-image-v1",
		Input:      json.RawMessage(`{"prompt":"海边的灯塔","aspectRatio":"1:1"}`),
	}
	payload, err := recipe.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestB2BDeterministicRejection仅PaymentRequired持久化原因(t *testing.T) {
	tests := []struct {
		name  string
		code  string
		want  bool
		cause generation.ProviderRejectionCause
	}{
		{name: "payment required", code: "PAYMENT_REQUIRED", want: true, cause: generation.ProviderRejectionCausePaymentRequired},
		{name: "bad request", code: "BAD_REQUEST", want: true},
		{name: "unknown", code: "UPSTREAM_ERROR", want: false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			decision := b2bDeterministicRejection(&polarstarb2b.ClientError{Code: testCase.code, HTTPStatus: 402})
			if decision.Reject != testCase.want || decision.Cause != testCase.cause {
				t.Fatalf("decision = %#v, want reject=%v cause=%q", decision, testCase.want, testCase.cause)
			}
		})
	}
}

func b2bWorkerJobBody(jobID, status string) string {
	return `{"success":true,"data":{"contractVersion":"b2b.job.v2","jobId":"` + jobID +
		`","tenantId":"` + b2bWorkerTenant + `","externalId":"` + b2bWorkerStepID +
		`","capability":"text_to_image","status":"` + status + `"}}`
}

// 完成态必须携带 resultUrl：适配器把「completed 却没有结果地址」当成无效响应，
// 而不是当成一笔没有素材的任务。
func b2bWorkerCompletedJobBody(jobID, resultURL string) string {
	return `{"success":true,"data":{"contractVersion":"b2b.job.v2","jobId":"` + jobID +
		`","tenantId":"` + b2bWorkerTenant + `","externalId":"` + b2bWorkerStepID +
		`","capability":"text_to_image","status":"completed","output":{"resultUrl":"` + resultURL + `"}}}`
}

type b2bCatalogStore struct {
	catalog generation.PublishedMappingCatalog
	err     error
	version string
}

func (store *b2bCatalogStore) PublishedCatalog(_ context.Context, version string) (*generation.PublishedMappingCatalog, error) {
	store.version = version
	if store.err != nil {
		return nil, store.err
	}
	catalog := store.catalog
	return &catalog, nil
}

// b2bSubmissionStore 在既有提交状态机上补齐 B2B 所需的两个持久化边界。
// 它刻意对命令调用 Validate：仓储是最后一道拒绝非法冻结事实的地方，
// 测试替身跳过校验就会让「命令本身不合法」在单测里永远发现不了。
type b2bSubmissionStore struct {
	*memorySubmissionStore
	intent      *generation.ProviderSubmissionIntent
	prepared    []generation.PrepareProviderSubmissionCommand
	preparedErr error
	bindings    []generation.ProviderJobBinding
	bindErr     error
	readErr     error
	// 重新授权替身必须与生产实现一样对第二次调用返回哨兵：
	// 若这里放行，`只发一次` 这条不变量在单测里就永远发现不了。
	reauthorizations []generation.ReauthorizeProviderSubmissionCommand
	reauthorizeErr   error
	reauthorized     bool
	requireTx        bool
}

func (store *b2bSubmissionStore) ReauthorizeProviderSubmission(ctx context.Context, command generation.ReauthorizeProviderSubmissionCommand) error {
	if store.requireTx && ctx.Value(b2bTxKey{}) != true {
		return errors.New("reauthorization requires transaction context")
	}
	if store.reauthorizeErr != nil {
		return store.reauthorizeErr
	}
	if err := command.Validate(); err != nil {
		return err
	}
	if store.reauthorized {
		return generation.ErrReauthorizationExhausted
	}
	store.reauthorized = true
	store.reauthorizations = append(store.reauthorizations, command)
	return nil
}

var _ generation.ProviderReauthorizationStore = (*b2bSubmissionStore)(nil)

func (store *b2bSubmissionStore) PrepareProviderSubmission(_ context.Context, command generation.PrepareProviderSubmissionCommand) error {
	if store.preparedErr != nil {
		return store.preparedErr
	}
	if err := command.Validate(); err != nil {
		return err
	}
	if store.intent != nil {
		return generation.ErrSubmissionConflict
	}
	store.prepared = append(store.prepared, command)
	store.intent = &generation.ProviderSubmissionIntent{Request: command.Request, PreparedAt: command.At, Fence: command.Fence}
	return nil
}

func (store *b2bSubmissionStore) ReadProviderSubmission(context.Context, string) (*generation.ProviderSubmissionIntent, error) {
	if store.readErr != nil {
		return nil, store.readErr
	}
	return store.intent, nil
}

func (store *b2bSubmissionStore) BindProviderJob(_ context.Context, binding generation.ProviderJobBinding) error {
	if store.bindErr != nil {
		return store.bindErr
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	store.bindings = append(store.bindings, binding)
	return nil
}

var _ generation.ProviderSubmissionStore = (*b2bSubmissionStore)(nil)
var _ generation.ProviderJobBinder = (*b2bSubmissionStore)(nil)

type b2bTxKey struct{}
type b2bContextTx struct{ err error }

func (tx b2bContextTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	if tx.err != nil {
		return tx.err
	}
	return fn(context.WithValue(ctx, b2bTxKey{}, true))
}

func TestB2BDefaultWorkerDoesNotReplayAfterLookup404(t *testing.T) {
	posts := 0
	f := newB2BWorkerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(http.StatusNotFound)
	})
	f.seedPreparedIntent(t)
	f.event.DeliveryStatus = outbox.DeliveryStatusReconciling
	f.now = f.now.Add(24 * time.Hour)
	// Use the production constructor, not a test-only reauthorization injection.
	w := NewGenerationSubmissionWorker(f.outbox, f.store, f.reverser, f.tx, f.router, f.reviewer, "default-recovery", func() time.Time { return f.now })
	if err := w.DeliverOnce(f.ctx, f.event.ID); err != nil {
		t.Fatal(err)
	}
	if posts != 0 || len(f.store.reauthorizations) != 0 || f.reverser.calls != 0 {
		t.Fatal("lookup 404 enabled replay or refund without verified provider semantics")
	}
}

func TestB2BRetryBeforeIntentCanSubmit(t *testing.T) {
	posts := 0
	f := newB2BWorkerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		posts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(b2bWorkerJobBody("job-after-retry", "queued")))
	})
	f.catalogs.err = errors.New("temporary catalog failure")
	if err := f.worker.DeliverOnce(f.ctx, f.event.ID); err != nil {
		t.Fatal(err)
	}
	if f.store.intent != nil || posts != 0 {
		t.Fatal("catalog failure created submission intent")
	}
	f.catalogs.err = nil
	f.now = f.event.NextAttemptAt
	if err := f.worker.DeliverOnce(f.ctx, f.event.ID); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || len(f.store.prepared) != 1 || len(f.store.bindings) != 1 {
		t.Fatal("retry did not finish first submission")
	}
}

func TestB2BCancellingWithoutDurableIntent绝不首次提交(t *testing.T) {
	posts := 0
	f := newB2BWorkerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(b2bWorkerJobBody("job-must-not-submit", "queued")))
	})
	f.store.record.CreationStatus = creations.CreationStatusCancelling

	err := f.worker.DeliverOnce(f.ctx, f.event.ID)
	if !errors.Is(err, ErrB2BSubmissionUnavailable) {
		t.Fatalf("DeliverOnce() error = %v, want ErrB2BSubmissionUnavailable", err)
	}
	if posts != 0 || len(f.store.prepared) != 0 || len(f.store.bindings) != 0 || f.reverser.calls != 0 {
		t.Fatalf("cancelling without intent caused submit side effects: post=%d prepared=%d bindings=%d reversals=%d", posts, len(f.store.prepared), len(f.store.bindings), f.reverser.calls)
	}
}

func TestB2BCorruptIntentReadCannotSubmitOrRefund(t *testing.T) {
	calls := 0
	f := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) { calls++ })
	f.store.readErr = generation.ErrSubmissionConflict
	if err := f.worker.DeliverOnce(f.ctx, f.event.ID); !errors.Is(err, generation.ErrSubmissionConflict) {
		t.Fatalf("error = %v", err)
	}
	if calls != 0 || len(f.store.prepared) != 0 || f.reverser.calls != 0 {
		t.Fatal("corrupt intent produced side effects")
	}
}

func TestB2BReauthorizationTransactionFailureDoesNotPost(t *testing.T) {
	posts := 0
	f := newB2BWorkerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.WriteHeader(http.StatusNotFound)
	})
	f.seedPreparedIntent(t)
	f.event.DeliveryStatus = outbox.DeliveryStatusReconciling
	f.now = f.now.Add(reauthorizationGracePeriod)
	txErr := errors.New("transaction failed")
	f.worker.tx = b2bContextTx{err: txErr}
	if err := f.worker.DeliverOnce(f.ctx, f.event.ID); !errors.Is(err, txErr) {
		t.Fatalf("error = %v", err)
	}
	if posts != 0 || len(f.store.reauthorizations) != 0 || f.reverser.calls != 0 {
		t.Fatal("failed authorization transaction produced side effects")
	}
}

// b2bRouter 只解析 B2B 出站能力；本地能力为空，因此任何本地步骤都会失败，
// 这正是测试想要的：B2B 用例里不应出现本地路径。
// 用指针接收者，测试才能在装配后关掉 B2B 授权并验证 fail closed。
type b2bRouter struct {
	client  *polarstarb2b.Client
	config  platform.B2BSubmissionConfig
	enabled bool
}

func (router *b2bRouter) ProviderForRoute(provider, accountRef string) (platform.ProviderHandle, error) {
	if provider != creations.PolarStarB2BProvider || accountRef != b2bWorkerAccount || router.client == nil {
		return platform.ProviderHandle{}, platform.ErrProviderNotEnabled
	}
	return platform.ProviderHandle{ID: creations.PolarStarB2BProvider, B2B: router.client, AccountRef: b2bWorkerAccount}, nil
}

func (router *b2bRouter) B2BSubmission() (platform.B2BSubmissionConfig, bool) {
	return router.config, router.enabled
}

var _ submissionRouter = (*b2bRouter)(nil)

type b2bWorkerFixture struct {
	*submissionWorkerFixture
	store    *b2bSubmissionStore
	catalogs *b2bCatalogStore
	router   *b2bRouter
}

func newB2BWorkerFixture(t *testing.T, handler http.HandlerFunc) *b2bWorkerFixture {
	t.Helper()
	server := httptest.NewTLSServer(handler)
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

	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	payload := b2bWorkerRecipePayload(t)
	event, err := outbox.NewPending(outbox.SubmissionEventID(b2bWorkerStepID), "creation-b2b-worker-test", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &operationRecorder{}
	base := &submissionWorkerFixture{
		t: t, ctx: context.Background(), now: now, event: event, recorder: recorder, serverURL: server.URL,
	}
	base.outbox = &memoryOutboxRepository{event: event, recorder: recorder}
	store := &b2bSubmissionStore{memorySubmissionStore: &memorySubmissionStore{
		record: generation.SubmissionRecord{
			Route: b2bWorkerRoute(), EventID: event.ID, CreationID: event.AggregateID, StepID: b2bWorkerStepID,
			UserID: "user-b2b-worker-test", ContentAccess: identity.ContentAccessStandard,
			// 租约令牌必须与假发件箱 Claim 写入的值一致，否则 DeliverOnce 会
			// 判定为条件写冲突而直接返回，测试会在什么都没发生的情况下「通过」。
			LeaseToken: "lease-worker-test", LeaseOwner: "worker-test", Fence: 1,
			ExecutionPayload: payload,
		},
		event: event, recorder: recorder,
	}}
	base.store = store.memorySubmissionStore
	base.reverser = &memoryReverser{recorder: recorder}
	base.reviewer = &memoryReviewer{}
	base.tx = &memoryTxRunner{}
	catalogs := &b2bCatalogStore{catalog: b2bTestCatalog()}
	router := &b2bRouter{client: client, enabled: true, config: platform.B2BSubmissionConfig{
		Catalogs: catalogs, Callback: polarstarb2b.CallbackConfig{Mode: "lookup_only"},
	}}
	base.worker = NewGenerationSubmissionWorker(base.outbox, store, base.reverser, base.tx, router, base.reviewer, "worker-test", func() time.Time { return base.now })
	// Test-only capability injection; the runtime constructor intentionally leaves it disabled.
	base.worker.reauthorization = store
	return &b2bWorkerFixture{submissionWorkerFixture: base, store: store, catalogs: catalogs, router: router}
}

// seedPreparedIntent 模拟「上一次已经领取过提交授权，但结果未知」的持久化事实。
func (fixture *b2bWorkerFixture) seedPreparedIntent(t *testing.T) {
	t.Helper()
	record := fixture.store.record
	request, err := mapB2BRequest(context.Background(), fixture.router.config, &record)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.intent = &generation.ProviderSubmissionIntent{
		Request: generation.FrozenProviderRequest{
			Route: record.Route, StepID: record.StepID, Capability: request.Capability(),
			IdempotencyKey: request.IdempotencyKey(), Digest: request.Digest(), Payload: request.Payload(),
		},
		PreparedAt: fixture.now, Fence: record.Fence,
	}
}

// assertRejectedAndReversed 断言确定性拒绝的收敛结果。
//
// 与本地路径不同，B2B 先开一个事务领取一次性提交授权，再开一个事务冲正，
// 因此事务次数是 2 而不是 1。关键不变量是「冲正、拒绝、结案各一次」——
// 多冲正一次就会把已经退回的钻石再退一次。
func (fixture *b2bWorkerFixture) assertRejectedAndReversed(t *testing.T) {
	t.Helper()
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusFailed || fixture.store.status != "failed" {
		t.Fatalf("失败状态 event/store = %s/%s, want failed/failed", fixture.event.DeliveryStatus, fixture.store.status)
	}
	if fixture.reverser.calls != 1 || fixture.outbox.failedCalls != 1 || fixture.store.rejectedCalls != 1 {
		t.Fatalf("冲正/结案/拒绝次数 = %d/%d/%d, want 1/1/1",
			fixture.reverser.calls, fixture.outbox.failedCalls, fixture.store.rejectedCalls)
	}
	if got, want := fixture.recorder.calls, []string{"rejected", "reversed", "failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("拒绝事务顺序 = %v, want %v", got, want)
	}
}

// 冻结配方必须映射成确定字节：同一份目录与配方在任何时刻都产出同一请求，
// 否则对账时按同一幂等键查询会拿到别人的任务。
func TestSubmitB2B映射冻结配方得到确定字节(t *testing.T) {
	fixture := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) {})
	record := fixture.store.record
	first, err := mapB2BRequest(context.Background(), fixture.router.config, &record)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mapB2BRequest(context.Background(), fixture.router.config, &record)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Payload()) != string(second.Payload()) || first.Digest() != second.Digest() {
		t.Fatal("同一冻结配方重复映射得到不同字节")
	}
	if fixture.catalogs.version != b2bCatalogVer {
		t.Fatalf("目录读取版本 = %q, want 冻结版本 %q", fixture.catalogs.version, b2bCatalogVer)
	}
	// 冻结字节必须能被协议适配器原样还原，否则对账路径无法重放。
	if _, err := polarstarb2b.RestoreRequest(
		polarstarb2b.Route{StepID: record.StepID, Provider: record.Route.Provider, AccountRef: record.Route.AccountRef,
			ContractVersion: record.Route.ContractVersion, MappingVersion: record.Route.MappingVersion},
		first.Payload(), first.Digest(),
	); err != nil {
		t.Fatalf("冻结请求不可还原: %v", err)
	}
}

func TestSubmitB2B成功提交后绑定外部任务(t *testing.T) {
	var posted []byte
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/jobs" {
			t.Fatalf("非预期请求: %s %s", request.Method, request.URL.Path)
		}
		postCalls++
		posted, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(b2bWorkerJobBody("job-b2b-1", "queued")))
	})

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 1 {
		t.Fatalf("POST 次数 = %d, want 1", postCalls)
	}
	if len(fixture.store.prepared) != 1 {
		t.Fatalf("提交授权次数 = %d, want 1", len(fixture.store.prepared))
	}
	prepared := fixture.store.prepared[0]
	if prepared.Request.Route != b2bWorkerRoute() || prepared.Fence != fixture.store.record.Fence {
		t.Fatalf("冻结授权 = %#v", prepared)
	}
	// 发出的字节必须与持久化的授权完全一致，否则对账会按不同的键去查。
	sum := sha256.Sum256(posted)
	if hex.EncodeToString(sum[:]) != prepared.Request.Digest {
		t.Fatal("实际发送字节与持久化摘要不一致")
	}
	if len(fixture.store.bindings) != 1 {
		t.Fatalf("绑定次数 = %d, want 1", len(fixture.store.bindings))
	}
	binding := fixture.store.bindings[0]
	if binding.ExternalID != "job-b2b-1" || binding.StepID != b2bWorkerStepID || binding.Route != b2bWorkerRoute() {
		t.Fatalf("任务绑定 = %#v", binding)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 || fixture.store.reconcilingCalls != 0 {
		t.Fatal("成功提交不得冲正、拒绝或进入对账")
	}
}

// 已经领过授权就不能再 POST：同一幂等键重发可能被上游当成两笔任务。
func TestSubmitB2B已存在提交意图时只对账不重发(t *testing.T) {
	postCalls := 0
	lookupCalls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost:
			postCalls++
			t.Errorf("已存在提交意图时不得再次 POST")
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/jobs/lookup":
			lookupCalls++
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(b2bWorkerJobBody("job-b2b-existing", "processing")))
		default:
			t.Errorf("非预期请求: %s %s", request.Method, request.URL.Path)
		}
	})
	fixture.seedPreparedIntent(t)
	// 已领取但在 POST 前后崩溃，再次领取仍只能按冻结意图查询。
	fixture.event.AttemptCount = 1

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 || lookupCalls != 1 {
		t.Fatalf("POST/Lookup = %d/%d, want 0/1", postCalls, lookupCalls)
	}
	if len(fixture.store.bindings) != 1 || fixture.store.bindings[0].ExternalID != "job-b2b-existing" {
		t.Fatalf("对账未绑定既有任务: %#v", fixture.store.bindings)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
		t.Fatal("对账路径不得冲正或拒绝")
	}
}

// 确定性 4xx 说明重试不会改变结果，且请求未形成可信受理，必须立即冲正。
func TestSubmitB2B确定性拒绝立即冲正(t *testing.T) {
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"success":false,"code":"BAD_REQUEST"}`))
	})

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	fixture.assertRejectedAndReversed(t)
	if len(fixture.store.bindings) != 0 {
		t.Fatal("被拒绝的提交不得绑定任务")
	}
	if fixture.store.rejectionCause != "" {
		t.Fatalf("BAD_REQUEST rejection cause = %q, want empty", fixture.store.rejectionCause)
	}
}

func TestSubmitB2BPaymentRequired持久化内部原因但保持账本冲正口径(t *testing.T) {
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusPaymentRequired)
		_, _ = writer.Write([]byte(`{"success":false,"code":"PAYMENT_REQUIRED"}`))
	})

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	fixture.assertRejectedAndReversed(t)
	if fixture.store.rejectionCause != generation.ProviderRejectionCausePaymentRequired {
		t.Fatalf("rejection cause = %q, want %q", fixture.store.rejectionCause, generation.ProviderRejectionCausePaymentRequired)
	}
}

// 5xx 无法证明中台没有受理，只能进入对账；冲正会把用户已扣的钻石退掉而任务仍在跑。
func TestSubmitB2B服务不可用进入对账而非冲正(t *testing.T) {
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"success":false,"code":"SERVICE_UNAVAILABLE"}`))
	})

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.store.reconcilingCalls != 1 || fixture.event.DeliveryStatus != outbox.DeliveryStatusReconciling {
		t.Fatalf("对账次数/状态 = %d/%s, want 1/reconciling", fixture.store.reconcilingCalls, fixture.event.DeliveryStatus)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 || fixture.outbox.failedCalls != 0 {
		t.Fatal("结果未知不得冲正或结案")
	}
	// 没有 Retry-After 时按尝试次数指数退避：该夹具 Fence 为 1，首次退避 10s。
	// 关键断言是「不早于 now」——退回成当前时刻就等于下一次轮询立刻重试。
	if got, want := fixture.event.NextAttemptAt, fixture.now.Add(submissionRetryBase); !got.Equal(want) {
		t.Fatalf("next attempt = %s, want %s", got, want)
	}
}

// 429 带 Retry-After 时，重排时刻必须取自平台而不是本地估算。
// 平台刚刚要求我们等待，我们却在下一次轮询就再打一次，是典型的忙轮询。
func TestSubmitB2B遵循RetryAfter重排(t *testing.T) {
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Retry-After", "120")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"success":false,"code":"RATE_LIMITED"}`))
	})

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.store.reconcilingCalls != 1 || fixture.event.DeliveryStatus != outbox.DeliveryStatusReconciling {
		t.Fatalf("对账次数/状态 = %d/%s, want 1/reconciling", fixture.store.reconcilingCalls, fixture.event.DeliveryStatus)
	}
	if got, want := fixture.event.NextAttemptAt, fixture.now.Add(120*time.Second); !got.Equal(want) {
		t.Fatalf("next attempt = %s, want %s (Retry-After)", got, want)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
		t.Fatal("限流不得冲正")
	}
}

// 幂等冲突说明同一键已有任务，既不能重发也不能冲正，只能对账。
func TestSubmitB2B幂等冲突转入对账(t *testing.T) {
	lookupCalls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			lookupCalls++
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(b2bWorkerJobBody("job-b2b-conflict", "queued")))
			return
		}
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"success":false,"code":"CONFLICT"}`))
	})

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if lookupCalls != 1 {
		t.Fatalf("冲突后 Lookup 次数 = %d, want 1", lookupCalls)
	}
	if len(fixture.store.bindings) != 1 || fixture.store.bindings[0].ExternalID != "job-b2b-conflict" {
		t.Fatalf("冲突后未绑定既有任务: %#v", fixture.store.bindings)
	}
	if fixture.reverser.calls != 0 {
		t.Fatal("幂等冲突不得冲正")
	}
}

// 目录版本与冻结版本不一致属确定性失败，且请求尚未发出，可以安全冲正。
func TestSubmitB2B目录版本不一致时冲正且不请求(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) { postCalls++ })
	catalog := b2bTestCatalog()
	catalog.Version = "ps-map-other"
	fixture.catalogs.catalog = catalog

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("映射失败仍请求中台 %d 次", postCalls)
	}
	fixture.assertRejectedAndReversed(t)
}

func TestSubmitB2B目录版本不存在时冲正且不请求(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) { postCalls++ })
	fixture.catalogs.err = generation.ErrMappingCatalogUnavailable

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("目录不可用仍请求中台 %d 次", postCalls)
	}
	fixture.assertRejectedAndReversed(t)
}

// 目录读取失败属基础设施问题，不是「这笔任务做不了」。
// 冲正会把用户已预扣的钻石退掉并宣告创作失败，代价远大于重排一次。
func TestSubmitB2B目录读取失败时重排而非冲正(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) { postCalls++ })
	fixture.catalogs.err = errors.New("mongo: connection reset by peer")

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("目录读取失败仍请求中台 %d 次", postCalls)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 || fixture.outbox.failedCalls != 0 {
		t.Fatal("基础设施抖动不得冲正或结案")
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending || !fixture.event.NextAttemptAt.After(fixture.now) {
		t.Fatalf("重排状态 = %s/%s, want pending/未来时刻", fixture.event.DeliveryStatus, fixture.event.NextAttemptAt)
	}
}

// 运行配置没有授权 B2B 时必须原样失败：上游可能已经受理，冲正会掩盖事实。
func TestSubmitB2B未配置提交能力时原样失败(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(http.ResponseWriter, *http.Request) { postCalls++ })
	fixture.router.enabled = false

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); !errors.Is(err, ErrB2BSubmissionUnavailable) {
		t.Fatalf("DeliverOnce() error = %v, want ErrB2BSubmissionUnavailable", err)
	}
	if postCalls != 0 || fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 || fixture.store.reconcilingCalls != 0 {
		t.Fatal("未授权的 B2B 提交不得产生任何副作用")
	}
}

// 对账查到终态时不得报错：终态是每笔任务最终都会走到的正常终局。
//
// 这条断言直接对应一次真实缺陷：这里曾经返回 ErrB2BTerminalUnavailable，
// 而 Runner.Run 对任何错误都是 return err、组合根随即 panic
// （cmd/generation-submission-worker/main.go 里 runner.Run 之后就是 panic）。
// 于是一笔正常完成的任务会打断整个 Worker 进程，连同进程内其他在途任务。
//
// 「提交事件被结清 + generation.reconcile 被唤醒」由 BindProviderJob 在同一事务
// 内完成，已由 data 层 provider_job_binding_test.go 断言；本用例只需钉住
// 「终态不再产生错误」与「不凭空造终态」这两件事。
func TestReconcileB2B终态不报错且不凭空落终态(t *testing.T) {
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(b2bWorkerCompletedJobBody("job-b2b-done", "https://cdn.example.test/out.png")))
	})
	fixture.seedPreparedIntent(t)
	fixture.event.DeliveryStatus = outbox.DeliveryStatusReconciling

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		// 非 nil 会让 Runner.Run 上抛、组合根 panic。
		t.Fatalf("DeliverOnce() error = %v, want nil（终态不是错误）", err)
	}
	if len(fixture.store.bindings) != 1 {
		t.Fatal("终态任务号仍必须先绑定，否则恢复会丢掉外部任务号")
	}
	// 终态由对账 Worker 走统一 CAS 落库；提交 Worker 不得自己写终态。
	if fixture.store.status != "" || fixture.store.submittedJobID != "" {
		t.Fatalf("提交 Worker 不得凭空落终态：status=%q jobID=%q", fixture.store.status, fixture.store.submittedJobID)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 || fixture.store.confiscatedCalls != 0 {
		t.Fatal("终态不得冲正、拒绝或没收")
	}
}

// 对账查不到不能**立刻**当作「没有任务」：刚受理的任务可能因平台侧读后写延迟
// 短暂查不到。等待期之内只重排，不消费重新授权额度。
//
// 用户钻石已预扣，因此这个等待期是必要的：它让「平台真的没有」与「平台还没
// 暴露出来」这两件事在时间上分开。
func TestReconcileB2B查不到时重新入队(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			postCalls++
		}
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"success":false,"code":"NOT_FOUND"}`))
	})
	fixture.seedPreparedIntent(t)
	fixture.event.DeliveryStatus = outbox.DeliveryStatusReconciling

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending || !fixture.event.NextAttemptAt.After(fixture.now) {
		t.Fatalf("重新入队状态 = %s/%s, want pending/未来时刻", fixture.event.DeliveryStatus, fixture.event.NextAttemptAt)
	}
	if postCalls != 0 || len(fixture.store.reauthorizations) != 0 {
		t.Fatal("等待期未满不得重发，也不得消费重新授权额度")
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
		t.Fatal("查不到不得冲正或拒绝")
	}
}

// 平台按确定派生的幂等键明确返回 404，可以证明它从未受理这笔任务。
// 此时消费唯一一次有审计的重新授权，用同一份冻结字节重发，任务才可能收敛。
//
// 否则活锁无解：提交与对账共用同一个发件箱事件，而 DeliverOnce 在
// attempt_count > 1 时永远走对账分支 —— 重排一旦发生，该事件就再也回不到
// 提交路径，而 404 又永远不变，任务既不前进也不收敛。
func TestReconcileB2B平台明确查不到时消费一次重新授权并重发(t *testing.T) {
	var posted []byte
	postCalls, lookupCalls := 0, 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/jobs":
			postCalls++
			posted, _ = io.ReadAll(request.Body)
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(b2bWorkerJobBody("job-b2b-reauthorized", "queued")))
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/jobs/lookup":
			lookupCalls++
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"success":false,"code":"NOT_FOUND"}`))
		default:
			t.Errorf("非预期请求: %s %s", request.Method, request.URL.Path)
		}
	})
	fixture.seedPreparedIntent(t)
	fixture.store.requireTx = true
	fixture.worker.tx = b2bContextTx{}
	fixture.event.DeliveryStatus = outbox.DeliveryStatusReconciling
	// 把时钟推过等待期：等待期之内只有「等」这一条路（由上一条用例钉住）。
	fixture.now = fixture.now.Add(reauthorizationGracePeriod)

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if lookupCalls != 1 || postCalls != 1 {
		t.Fatalf("Lookup/POST = %d/%d, want 1/1", lookupCalls, postCalls)
	}
	// 关键不变量：重发必须复用同一份冻结身份。同一个幂等键才让平台侧的
	// 重放是幂等的，否则「至多一笔任务」这个前提就不成立。
	var wire struct {
		IdempotencyKey string `json:"idempotencyKey"`
		ExternalID     string `json:"externalId"`
	}
	if err := json.Unmarshal(posted, &wire); err != nil {
		t.Fatalf("重发载荷不可解析: %v", err)
	}
	if wire.IdempotencyKey != "cling-step:"+b2bWorkerStepID || wire.ExternalID != b2bWorkerStepID {
		t.Fatalf("重发幂等键/外部标识 = %q/%q, want cling-step:%s/%s",
			wire.IdempotencyKey, wire.ExternalID, b2bWorkerStepID, b2bWorkerStepID)
	}
	// 审计事实必须落库，且恰好一次。
	if len(fixture.store.reauthorizations) != 1 {
		t.Fatalf("重新授权次数 = %d, want 1", len(fixture.store.reauthorizations))
	}
	if got := fixture.store.reauthorizations[0]; got.Reason != generation.ReauthorizationReasonLookupNotFound ||
		got.StepID != b2bWorkerStepID || got.Fence != fixture.store.record.Fence {
		t.Fatalf("重新授权命令 = %#v", got)
	}
	if len(fixture.store.bindings) != 1 || fixture.store.bindings[0].ExternalID != "job-b2b-reauthorized" {
		t.Fatal("重发成功后必须绑定外部任务号，否则恢复会丢掉它")
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
		t.Fatal("重新授权路径不得冲正或拒绝")
	}
}

// 5xx 与传输失败都不具备「平台没有这笔任务」的证明力：即使等待期已满也不能重发。
// 适配器把传输失败的 HTTPStatus 留成零值，因此它与 404 必须被分开判定。
func TestReconcileB2B传输失败时即使等待期满也不重发(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			postCalls++
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(b2bWorkerJobBody("job-should-not-happen", "queued")))
			return
		}
		// 直接掐断连接：客户端会把它归成 transport_failure，而不是任何 HTTP 状态。
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("测试服务器不支持连接劫持")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("劫持连接失败: %v", err)
			return
		}
		_ = conn.Close()
	})
	fixture.seedPreparedIntent(t)
	fixture.event.DeliveryStatus = outbox.DeliveryStatusReconciling
	fixture.now = fixture.now.Add(reauthorizationGracePeriod)

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 || len(fixture.store.reauthorizations) != 0 {
		t.Fatal("传输失败不得重发，也不得消费重新授权额度")
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending {
		t.Fatalf("重排状态 = %s, want pending", fixture.event.DeliveryStatus)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
		t.Fatal("传输失败不得冲正或拒绝")
	}
}

// 额度只有一次：平台持续 404 而额度已用完时，不能再发第二次。
// 此时也不凭空造终态 —— 证据只够证明「平台没有这笔任务」，
// 不够证明「用户不该被收费」。
func TestReconcileB2B重新授权额度用完后不再重发(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			postCalls++
		}
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"success":false,"code":"NOT_FOUND"}`))
	})
	fixture.seedPreparedIntent(t)
	fixture.event.DeliveryStatus = outbox.DeliveryStatusReconciling
	fixture.now = fixture.now.Add(reauthorizationGracePeriod)
	fixture.store.reauthorized = true // 上一次对账已经把额度用掉了

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("额度已用完仍重发 %d 次", postCalls)
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending {
		t.Fatalf("重排状态 = %s, want pending", fixture.event.DeliveryStatus)
	}
	if fixture.reverser.calls != 0 || fixture.store.rejectedCalls != 0 {
		t.Fatal("额度用完不得冲正或拒绝")
	}
}

// 仓储不支持重新授权时必须保持既有语义：只对账，不重发。
// 这是 fail closed —— 能力缺失不能降级成任何别的路径。
func TestReconcileB2B仓储不支持重新授权时只对账(t *testing.T) {
	postCalls := 0
	fixture := newB2BWorkerFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			postCalls++
		}
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"success":false,"code":"NOT_FOUND"}`))
	})
	fixture.seedPreparedIntent(t)
	fixture.event.DeliveryStatus = outbox.DeliveryStatusReconciling
	fixture.now = fixture.now.Add(reauthorizationGracePeriod)
	fixture.worker.reauthorization = nil // 模拟仓储未实现该可选能力

	if err := fixture.worker.DeliverOnce(fixture.ctx, fixture.event.ID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 {
		t.Fatalf("能力缺失时仍重发 %d 次", postCalls)
	}
	if fixture.event.DeliveryStatus != outbox.DeliveryStatusPending {
		t.Fatalf("重排状态 = %s, want pending", fixture.event.DeliveryStatus)
	}
}
