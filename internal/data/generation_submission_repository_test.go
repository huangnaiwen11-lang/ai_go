package data

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"ai-business-service/internal/executionv2"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/worker"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMongoSubmission提交CAS只允许一个结案且不覆盖任务ID(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	repository := NewGenerationSubmissionRepository(fixture.data)
	fixture.seedDispatching()

	if err := repository.MarkSubmitted(fixture.ctx, generation.SubmittedCommand{
		EventID: fixture.eventID, LeaseToken: "错误租约", JobID: "job-ignored", At: fixture.now,
	}); !errors.Is(err, generation.ErrSubmissionConflict) {
		t.Fatalf("错误租约 MarkSubmitted() error = %v, want ErrSubmissionConflict", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, jobID := range []string{"job-a", "job-b"} {
		group.Add(1)
		go func(jobID string) {
			defer group.Done()
			ctx, cancel := newMongoTestContext()
			defer cancel()
			<-start
			results <- fixture.runner.WithinTx(ctx, func(txCtx context.Context) error {
				return repository.MarkSubmitted(txCtx, generation.SubmittedCommand{
					EventID: fixture.eventID, LeaseToken: fixture.leaseToken, JobID: jobID, At: fixture.now,
				})
			})
		}(jobID)
	}
	close(start)
	group.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, generation.ErrSubmissionConflict):
			conflicts++
		default:
			t.Fatalf("并发 MarkSubmitted() error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("并发 MarkSubmitted() successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}

	var event model.OutboxEventDocument
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.eventID}}).Decode(&event); err != nil {
		t.Fatalf("读取 outbox 事件: %v", err)
	}
	if event.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || event.JobID == "" || event.LeaseToken != "" {
		t.Fatalf("提交后 outbox 事件 = %#v, want delivered with one job ID and no lease", event)
	}
	var step model.CreationStepDocument
	if err := fixture.steps.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.stepID}}).Decode(&step); err != nil {
		t.Fatalf("读取创作步骤: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusSubmitted) || step.ExternalExecutionID != event.JobID {
		t.Fatalf("提交后步骤 = %#v, want submitted with event job ID", step)
	}
}

func TestMongoSubmissionJobID全局唯一CAS拒绝第二步骤(t *testing.T) {
	first := newSubmissionMongoFixture(t)
	second := newSubmissionMongoFixture(t)
	first.seedDispatching()
	second.seedDispatching()
	firstRepository := NewGenerationSubmissionRepository(first.data)
	secondRepository := NewGenerationSubmissionRepository(second.data)
	jobID := "job-" + uuid.NewString()

	if err := first.runner.WithinTx(first.ctx, func(txCtx context.Context) error {
		return firstRepository.MarkSubmitted(txCtx, generation.SubmittedCommand{
			EventID: first.eventID, LeaseToken: first.leaseToken, JobID: jobID, At: first.now,
		})
	}); err != nil {
		t.Fatalf("首个 MarkSubmitted() error = %v", err)
	}
	if err := second.runner.WithinTx(second.ctx, func(txCtx context.Context) error {
		return secondRepository.MarkSubmitted(txCtx, generation.SubmittedCommand{
			EventID: second.eventID, LeaseToken: second.leaseToken, JobID: jobID, At: second.now,
		})
	}); !errors.Is(err, generation.ErrSubmissionConflict) {
		t.Fatalf("重复 JobID MarkSubmitted() error = %v, want ErrSubmissionConflict", err)
	}

	var secondStep model.CreationStepDocument
	if err := second.steps.FindOne(second.ctx, bson.D{{Key: "_id", Value: second.stepID}}).Decode(&secondStep); err != nil {
		t.Fatalf("读取第二步骤: %v", err)
	}
	if secondStep.SubmitStatus != string(creations.StepSubmitStatusReady) || secondStep.ExternalExecutionID != "" {
		t.Fatalf("重复 JobID 后第二步骤 = %#v, want untouched ready step", secondStep)
	}
	var secondEvent model.OutboxEventDocument
	if err := second.events.FindOne(second.ctx, bson.D{{Key: "_id", Value: second.eventID}}).Decode(&secondEvent); err != nil {
		t.Fatalf("读取第二 outbox 事件: %v", err)
	}
	if secondEvent.DeliveryStatus != string(outbox.DeliveryStatusDispatching) || secondEvent.LeaseToken != second.leaseToken || secondEvent.JobID != "" {
		t.Fatalf("重复 JobID 后第二 outbox 事件 = %#v, want untouched dispatching event", secondEvent)
	}
}

func TestMongoSubmission已领取记录只返回技术事实(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	repository := NewGenerationSubmissionRepository(fixture.data)
	fixture.seedDispatching()

	record, err := repository.ClaimedSubmission(fixture.ctx, fixture.eventID)
	if err != nil {
		t.Fatalf("ClaimedSubmission() error = %v", err)
	}
	if record.EventID != fixture.eventID || record.CreationID != fixture.creationID || record.StepID != fixture.stepID || record.LeaseToken != fixture.leaseToken {
		t.Fatalf("ClaimedSubmission() record = %#v, want matching event, creation, step and lease", record)
	}
	if len(record.ExecutionPayload) == 0 {
		t.Fatal("ClaimedSubmission() 缺少执行技术载荷")
	}
	record.ExecutionPayload[0] = '!'
	var event model.OutboxEventDocument
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.eventID}}).Decode(&event); err != nil {
		t.Fatalf("读取 outbox 事件: %v", err)
	}
	if event.Payload[0] == '!' {
		t.Fatal("ClaimedSubmission() 暴露了可变持久化载荷")
	}
}

func TestMongoSubmissionReconcileCAS允许后续领取(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	repository := NewGenerationSubmissionRepository(fixture.data)
	fixture.seedDispatching()

	if err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return repository.MarkReconciling(txCtx, generation.ReconcilingCommand{
			EventID: fixture.eventID, LeaseToken: fixture.leaseToken, At: fixture.now,
		})
	}); err != nil {
		t.Fatalf("MarkReconciling() error = %v", err)
	}

	var event model.OutboxEventDocument
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.eventID}}).Decode(&event); err != nil {
		t.Fatalf("读取 reconciling 事件: %v", err)
	}
	if event.DeliveryStatus != string(outbox.DeliveryStatusReconciling) || event.LeaseToken != "" || !event.NextAttemptAt.Equal(fixture.now) {
		t.Fatalf("reconciling 事件 = %#v, want immediately claimable without lease", event)
	}
	var step model.CreationStepDocument
	if err := fixture.steps.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.stepID}}).Decode(&step); err != nil {
		t.Fatalf("读取 reconciling 步骤: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusReconciling) {
		t.Fatalf("reconciling step status = %q, want reconciling", step.SubmitStatus)
	}
	reclaimed, err := NewOutboxRepository(fixture.data).Claim(
		fixture.ctx,
		"worker-2",
		fixture.now,
		fixture.now.Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("Task2 Claim() reconciling event error = %v", err)
	}
	if reclaimed == nil || reclaimed.ID != fixture.eventID || reclaimed.DeliveryStatus != outbox.DeliveryStatusDispatching || reclaimed.LeaseToken == "" {
		t.Fatalf("Task2 Claim() reconciling event = %#v, want re-claimed dispatching event", reclaimed)
	}
}

// TestMongoSubmission过期租约接管后只查询不重复提交覆盖崩溃恢复的完整边界：
// 新工作者接管旧 dispatching 租约后，只能按稳定步骤标识查中台，绝不能再次创建技术任务。
func TestMongoSubmission过期租约接管后只查询不重复提交(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	fixture.seedDispatching()
	workerNow := fixture.now.Add(2 * time.Minute)

	var postCalls, lookupCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/executions":
			postCalls++
			writer.WriteHeader(http.StatusInternalServerError)
		case "/api/v2/executions/lookup":
			lookupCalls++
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"success":true,"data":{"jobId":"job-recovered","status":"accepted"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	client, err := platform.NewClientWithCallbackOrigin(
		server.URL,
		"test-generation-request-hmac-key-12345",
		"https://api.example.test",
		server.Client(),
		func() time.Time { return workerNow },
		func() string { return "nonce-recovery-test" },
	)
	if err != nil {
		t.Fatalf("创建中台客户端: %v", err)
	}
	workerInstance := worker.NewGenerationSubmissionWorker(
		NewOutboxRepository(fixture.data),
		NewGenerationSubmissionRepository(fixture.data),
		ledger.NewUsecase(NewLedgerRepository(fixture.data), fixture.runner),
		fixture.runner,
		client,
		nil,
		"worker-recovery",
		func() time.Time { return workerNow },
	)

	if err := workerInstance.DeliverOnce(fixture.ctx, fixture.eventID); err != nil {
		t.Fatalf("DeliverOnce() error = %v", err)
	}
	if postCalls != 0 || lookupCalls != 1 {
		t.Fatalf("中台调用 POST/Lookup = %d/%d，过期租约恢复只能查询一次", postCalls, lookupCalls)
	}

	var event model.OutboxEventDocument
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.eventID}}).Decode(&event); err != nil {
		t.Fatalf("读取已恢复 outbox: %v", err)
	}
	if event.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || event.JobID != "job-recovered" {
		t.Fatalf("恢复后的 outbox = %#v，want delivered/job-recovered", event)
	}
	var step model.CreationStepDocument
	if err := fixture.steps.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.stepID}}).Decode(&step); err != nil {
		t.Fatalf("读取已恢复步骤: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusSubmitted) || step.ExternalExecutionID != "job-recovered" {
		t.Fatalf("恢复后的步骤 = %#v，want submitted/job-recovered", step)
	}
}

func TestMongoSubmission拒绝CAS只写创作步骤状态(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	repository := NewGenerationSubmissionRepository(fixture.data)
	fixture.seedDispatching()

	if err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return repository.MarkRejected(txCtx, generation.RejectedCommand{
			EventID: fixture.eventID, LeaseToken: fixture.leaseToken, At: fixture.now,
		})
	}); err != nil {
		t.Fatalf("MarkRejected() error = %v", err)
	}

	var creation model.CreationDocument
	if err := fixture.creations.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.creationID}}).Decode(&creation); err != nil {
		t.Fatalf("读取失败创作: %v", err)
	}
	if creation.Status != string(creations.CreationStatusSubmissionFailed) {
		t.Fatalf("失败创作状态 = %q, want submission_failed", creation.Status)
	}
	var step model.CreationStepDocument
	if err := fixture.steps.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.stepID}}).Decode(&step); err != nil {
		t.Fatalf("读取失败步骤: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusSubmissionFailed) {
		t.Fatalf("失败步骤状态 = %q, want submission_failed", step.SubmitStatus)
	}
	var event model.OutboxEventDocument
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.eventID}}).Decode(&event); err != nil {
		t.Fatalf("读取未结案事件: %v", err)
	}
	if event.DeliveryStatus != string(outbox.DeliveryStatusDispatching) || event.LeaseToken != fixture.leaseToken {
		t.Fatalf("MarkRejected() 改写了 outbox 结案顺序: %#v", event)
	}
}

// 审核没收只收敛创作和步骤终态；账本与发件箱由工作者在同一外层事务继续结案。
func TestMongoSubmission审核没收CAS只接受持有租约的待提交步骤(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	repository := NewGenerationSubmissionRepository(fixture.data)
	fixture.seedDispatching()

	if err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
		return repository.MarkConfiscated(txCtx, generation.ConfiscatedCommand{
			EventID: fixture.eventID, LeaseToken: fixture.leaseToken, At: fixture.now,
		})
	}); err != nil {
		t.Fatalf("MarkConfiscated() error = %v", err)
	}

	var creation model.CreationDocument
	if err := fixture.creations.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.creationID}}).Decode(&creation); err != nil {
		t.Fatalf("读取没收创作: %v", err)
	}
	if creation.Status != string(creations.CreationStatusConfiscated) {
		t.Fatalf("没收创作状态 = %q, want confiscated", creation.Status)
	}
	var step model.CreationStepDocument
	if err := fixture.steps.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.stepID}}).Decode(&step); err != nil {
		t.Fatalf("读取没收步骤: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusConfiscated) {
		t.Fatalf("没收步骤状态 = %q, want confiscated", step.SubmitStatus)
	}
	var event model.OutboxEventDocument
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.eventID}}).Decode(&event); err != nil {
		t.Fatalf("读取尚待结案的 outbox: %v", err)
	}
	if event.DeliveryStatus != string(outbox.DeliveryStatusDispatching) || event.LeaseToken != fixture.leaseToken {
		t.Fatalf("MarkConfiscated() 改写了工作者的 outbox 结案顺序: %#v", event)
	}
}

func TestMongoSubmission拒绝CAS只允许一个状态迁移(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	repository := NewGenerationSubmissionRepository(fixture.data)
	fixture.seedDispatching()

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := newMongoTestContext()
			defer cancel()
			<-start
			results <- fixture.runner.WithinTx(ctx, func(txCtx context.Context) error {
				return repository.MarkRejected(txCtx, generation.RejectedCommand{
					EventID: fixture.eventID, LeaseToken: fixture.leaseToken, At: fixture.now,
				})
			})
		}()
	}
	close(start)
	group.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, generation.ErrSubmissionConflict):
			conflicts++
		default:
			t.Fatalf("并发 MarkRejected() error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("并发 MarkRejected() successes/conflicts = %d/%d, want 1/1", successes, conflicts)
	}
}

func TestMongoSubmission命令冲突矩阵CAS(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*submissionMongoFixture)
		call    func(generation.SubmissionStore, *submissionMongoFixture, context.Context) error
	}{
		{
			name: "submitted 错误租约",
			call: func(repository generation.SubmissionStore, fixture *submissionMongoFixture, ctx context.Context) error {
				return repository.MarkSubmitted(ctx, generation.SubmittedCommand{EventID: fixture.eventID, LeaseToken: "wrong-lease", JobID: "job-1", At: fixture.now})
			},
		},
		{
			name: "reconciling 错误租约",
			call: func(repository generation.SubmissionStore, fixture *submissionMongoFixture, ctx context.Context) error {
				return repository.MarkReconciling(ctx, generation.ReconcilingCommand{EventID: fixture.eventID, LeaseToken: "wrong-lease", At: fixture.now})
			},
		},
		{
			name: "rejected 错误租约",
			call: func(repository generation.SubmissionStore, fixture *submissionMongoFixture, ctx context.Context) error {
				return repository.MarkRejected(ctx, generation.RejectedCommand{EventID: fixture.eventID, LeaseToken: "wrong-lease", At: fixture.now})
			},
		},
		{
			name: "submitted 已结案事件",
			prepare: func(fixture *submissionMongoFixture) {
				fixture.setEventStatus(outbox.DeliveryStatusDelivered)
			},
			call: func(repository generation.SubmissionStore, fixture *submissionMongoFixture, ctx context.Context) error {
				return repository.MarkSubmitted(ctx, generation.SubmittedCommand{EventID: fixture.eventID, LeaseToken: fixture.leaseToken, JobID: "job-1", At: fixture.now})
			},
		},
		{
			name: "reconciling 已提交步骤",
			prepare: func(fixture *submissionMongoFixture) {
				fixture.setStepStatus(creations.StepSubmitStatusSubmitted)
			},
			call: func(repository generation.SubmissionStore, fixture *submissionMongoFixture, ctx context.Context) error {
				return repository.MarkReconciling(ctx, generation.ReconcilingCommand{EventID: fixture.eventID, LeaseToken: fixture.leaseToken, At: fixture.now})
			},
		},
		{
			name: "rejected 已失败创作",
			prepare: func(fixture *submissionMongoFixture) {
				fixture.setCreationStatus(creations.CreationStatusSubmissionFailed)
			},
			call: func(repository generation.SubmissionStore, fixture *submissionMongoFixture, ctx context.Context) error {
				return repository.MarkRejected(ctx, generation.RejectedCommand{EventID: fixture.eventID, LeaseToken: fixture.leaseToken, At: fixture.now})
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSubmissionMongoFixture(t)
			fixture.seedDispatching()
			if testCase.prepare != nil {
				testCase.prepare(fixture)
			}
			repository := NewGenerationSubmissionRepository(fixture.data)
			err := fixture.runner.WithinTx(fixture.ctx, func(txCtx context.Context) error {
				return testCase.call(repository, fixture, txCtx)
			})
			if !errors.Is(err, generation.ErrSubmissionConflict) {
				t.Fatalf("submission command error = %v, want ErrSubmissionConflict", err)
			}
		})
	}
}

type submissionMongoFixture struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	data      *Data
	runner    *MongoTxRunner
	database  *mongo.Database
	creations *mongo.Collection
	steps     *mongo.Collection
	events    *mongo.Collection
	users     *mongo.Collection

	now        time.Time
	userID     string
	creationID string
	stepID     string
	eventID    string
	leaseToken string
}

func newSubmissionMongoFixture(t *testing.T) *submissionMongoFixture {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		cancel()
		t.Fatalf("初始化本地 schema: %v", err)
	}
	fixture := &submissionMongoFixture{
		t:          t,
		ctx:        ctx,
		cancel:     cancel,
		data:       &Data{client: client, database: database},
		runner:     NewMongoTxRunner(client),
		database:   database,
		creations:  database.Collection(schema.CollectionCreations),
		steps:      database.Collection(schema.CollectionCreationSteps),
		events:     database.Collection(schema.CollectionOutboxEvents),
		users:      database.Collection(schema.CollectionUsers),
		now:        time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC),
		userID:     uuid.NewString(),
		creationID: uuid.NewString(),
		stepID:     "step-" + uuid.NewString(),
		leaseToken: "lease-" + uuid.NewString(),
	}
	fixture.eventID = outbox.SubmissionEventID(fixture.stepID)
	t.Cleanup(cancel)
	return fixture
}

func (fixture *submissionMongoFixture) seedDispatching() {
	fixture.t.Helper()
	reservationID := "reservation:" + fixture.creationID
	reserveEntryID := "reserve:" + fixture.creationID
	// 发件箱保存 Snapshot 输出的冻结技术载荷；worker 通过同一 DTO 还原中台提交合同。
	snapshot, err := executionv2.Compile(
		executionv2.CapabilityTextToImage,
		"ps-image-v1",
		[]byte(`{"prompt":"test","assets":[],"parameters":{}}`),
	)
	if err != nil {
		fixture.t.Fatalf("编译测试 execution.v2 快照: %v", err)
	}
	payload, err := snapshot.MarshalSubmissionPayload()
	if err != nil {
		fixture.t.Fatalf("序列化测试 execution.v2 快照: %v", err)
	}
	for _, document := range []struct {
		collection *mongo.Collection
		id         string
	}{
		{fixture.database.Collection(schema.CollectionAccounts), fixture.userID},
		{fixture.users, fixture.userID},
		{fixture.database.Collection(schema.CollectionReservations), reservationID},
		{fixture.database.Collection(schema.CollectionLedgerEntries), reserveEntryID},
		{fixture.creations, fixture.creationID},
		{fixture.steps, fixture.stepID},
		{fixture.events, fixture.eventID},
	} {
		collection := document.collection
		id := document.id
		fixture.t.Cleanup(func() {
			ctx, cancel := newMongoTestContext()
			defer cancel()
			if _, err := collection.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
				fixture.t.Errorf("按测试 _id 清理 %q/%q: %v", collection.Name(), id, err)
			}
		})
	}
	if _, err := fixture.database.Collection(schema.CollectionAccounts).InsertOne(fixture.ctx, model.AccountDocument{ID: fixture.userID, DiamondBalance: 20, CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("种入测试账户: %v", err)
	}
	if _, err := fixture.users.InsertOne(fixture.ctx, model.UserDocument{ID: fixture.userID, AccountStatus: string(identity.AccountStatusNormal), BindingState: string(identity.BindingStateBound), Timezone: "Asia/Shanghai", SessionVersion: 1, ContentAccess: identity.ContentAccessStandard, CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("种入测试用户: %v", err)
	}
	if _, err := fixture.database.Collection(schema.CollectionReservations).InsertOne(fixture.ctx, model.ReservationDocument{ID: reservationID, CreationID: fixture.creationID, UserID: fixture.userID, PriceDiamonds: 20, ReservedDiamonds: 20, BenefitSource: string(ledger.BenefitSourceDiamonds), Status: string(ledger.ReservationStatusReserved), CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("种入测试预留: %v", err)
	}
	if _, err := fixture.database.Collection(schema.CollectionLedgerEntries).InsertOne(fixture.ctx, model.LedgerEntryDocument{ID: reserveEntryID, IdempotencyKey: reserveEntryID, AccountID: fixture.userID, CreationID: fixture.creationID, DeltaDiamonds: -20, Reason: "generation_reserved", ReservationID: reservationID, CreatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("种入测试账本分录: %v", err)
	}
	if _, err := fixture.creations.InsertOne(fixture.ctx, model.CreationDocument{ID: fixture.creationID, IdempotencyKey: uuid.NewString(), UserID: fixture.userID, TemplateID: "template-1", TemplateVersion: 1, RequestFingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Status: string(creations.CreationStatusPendingSubmission), Version: 1, CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("种入测试创作: %v", err)
	}
	if _, err := fixture.steps.InsertOne(fixture.ctx, model.CreationStepDocument{ID: fixture.stepID, CreationID: fixture.creationID, Sequence: 1, Atom: string(creations.AtomTextToImage), SubmitStatus: string(creations.StepSubmitStatusReady), CreatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("种入测试步骤: %v", err)
	}
	if _, err := fixture.events.InsertOne(fixture.ctx, model.OutboxEventDocument{ID: fixture.eventID, AggregateID: fixture.creationID, EventType: string(outbox.EventTypeGenerationSubmission), Payload: payload, DeliveryStatus: string(outbox.DeliveryStatusDispatching), AttemptCount: 1, NextAttemptAt: fixture.now, LeaseToken: fixture.leaseToken, LeaseUntil: fixture.now.Add(time.Minute), LeaseOwner: "worker-1", CreatedAt: fixture.now, UpdatedAt: fixture.now}); err != nil {
		fixture.t.Fatalf("种入测试 outbox: %v", err)
	}
}

func (fixture *submissionMongoFixture) setEventStatus(status outbox.DeliveryStatus) {
	fixture.t.Helper()
	if _, err := fixture.events.UpdateOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.eventID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "delivery_status", Value: string(status)}}}}); err != nil {
		fixture.t.Fatalf("设置测试 outbox 状态: %v", err)
	}
}

func (fixture *submissionMongoFixture) setStepStatus(status creations.StepSubmitStatus) {
	fixture.t.Helper()
	if _, err := fixture.steps.UpdateOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.stepID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(status)}}}}); err != nil {
		fixture.t.Fatalf("设置测试步骤状态: %v", err)
	}
}

func (fixture *submissionMongoFixture) setCreationStatus(status creations.CreationStatus) {
	fixture.t.Helper()
	if _, err := fixture.creations.UpdateOne(fixture.ctx, bson.D{{Key: "_id", Value: fixture.creationID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: string(status)}}}}); err != nil {
		fixture.t.Fatalf("设置测试创作状态: %v", err)
	}
}
