package data

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/biz/redrive"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var redriveAuditNow = time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)

// 重驱审计的端到端测试只把「事件 CAS + 审计写入」接到真实 Mongo 上，
// 门禁与账本用替身：本文件要验的是**同事务**语义，门禁自身已有独立覆盖。

type redriveGateStub struct{ err error }

func (stub redriveGateStub) LoadProviderResultMaterialization(context.Context, generation.ProviderResultMaterializeEventPayload) (generation.ProviderResultMaterializationTarget, error) {
	return generation.ProviderResultMaterializationTarget{}, stub.err
}

func (stub redriveGateStub) LoadProviderTerminalSettlement(context.Context, generation.ProviderTerminalSettlementEventPayload) (generation.ProviderTerminalSettlementTarget, error) {
	return generation.ProviderTerminalSettlementTarget{}, stub.err
}

type redriveLedgerStub struct{ status ledger.ReservationStatus }

func (stub redriveLedgerStub) FindReservation(context.Context, string) (*ledger.Reservation, error) {
	return &ledger.Reservation{CreationID: "creation-1", Status: stub.status}, nil
}

// failingRedriveAuditWriter 模拟审计集合不可用：它必须让整个事务回滚。
type failingRedriveAuditWriter struct{ err error }

func (writer failingRedriveAuditWriter) FindAudit(context.Context, redrive.AuditKey) (redrive.Audit, bool, error) {
	return redrive.Audit{}, false, nil
}

func (writer failingRedriveAuditWriter) WriteAudit(context.Context, redrive.Audit) error {
	return writer.err
}

// redriveEventSnapshot 只取断言需要的字段，避免依赖内部文档结构。
type redriveEventSnapshot struct {
	DeliveryStatus     string    `bson:"delivery_status"`
	AttentionReason    string    `bson:"attention_reason"`
	Payload            []byte    `bson:"payload"`
	AttemptCount       int32     `bson:"attempt_count"`
	RedriveCount       int32     `bson:"redrive_count"`
	RedriveStartedAt   time.Time `bson:"redrive_started_at"`
	RedriveAttemptBase int32     `bson:"redrive_attempt_base"`
}

type redriveFixture struct {
	database *mongo.Database
	storage  *Data
	eventID  string
	key      string
}

// redrivePayloadMarker 只出现在事件 payload 的 ResultRef 里，用于断言审计不携带载荷。
const redrivePayloadMarker = "payload-must-not-leak"

// newRedriveAttentionEvent 造一个停在 needs_attention 的真实事件，并返回带清理的夹具。
//
// 事件直接插入而不是走 outbox.NewPending：NewPending 固定写生成提交类型，
// 而重驱只接受 result-materialize / terminal-settle 两类事件；生产侧
// scheduleProviderResultMaterialization 同样是直接插入文档。
func newRedriveAttentionEvent(t *testing.T) *redriveFixture {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}
	storage := &Data{client: client, database: database}
	repository := NewOutboxRepository(storage)

	stepID := "step-" + uuid.NewString()
	eventID := generation.ProviderResultMaterializeEventID(stepID)
	if eventID == "" {
		t.Fatalf("非法步骤 ID %q 无法派生事件 ID", stepID)
	}
	resultRef := "https://results.example.test/" + redrivePayloadMarker + ".png"
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(generation.ProviderResultMaterializeEventPayload{
		CreationID: "creation-" + uuid.NewString(), StepID: stepID, Provider: creations.PolarStarB2BProvider,
		AccountRef: "account-main", JobID: "job-" + uuid.NewString(), Capability: "text_to_image",
		ResultRef: resultRef, TerminalVersion: 1,
		TerminalDigest: generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, resultRef),
	})
	if err != nil {
		t.Fatalf("构造物化 payload: %v", err)
	}

	document := model.OutboxEventDocument{
		ID: eventID, AggregateID: "creation-1", EventType: generation.ProviderResultMaterializeEventType,
		Payload: payload, DeliveryStatus: string(outbox.DeliveryStatusPending), AttemptCount: 0,
		NextAttemptAt: redriveAuditNow, CreatedAt: redriveAuditNow, UpdatedAt: redriveAuditNow,
	}
	if _, err := database.Collection(schema.CollectionOutboxEvents).InsertOne(ctx, document); err != nil {
		t.Fatalf("插入发件箱事件: %v", err)
	}
	claimed, err := repository.ClaimByIDAndType(ctx, "worker-1", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), redriveAuditNow, redriveAuditNow.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("ClaimByIDAndType() event/error = %#v / %v", claimed, err)
	}
	if err := repository.MarkNeedsAttention(ctx, eventID, claimed.LeaseToken, outbox.AttentionReasonMaterialUploadBudget, redriveAuditNow.Add(time.Minute)); err != nil {
		t.Fatalf("MarkNeedsAttention() error = %v", err)
	}

	fixture := &redriveFixture{database: database, storage: storage, eventID: eventID, key: "ticket-" + uuid.NewString()}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := database.Collection(schema.CollectionOutboxEvents).DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("清理发件箱事件 %q: %v", eventID, err)
		}
		if _, err := database.Collection(schema.CollectionAdminAudit).DeleteMany(cleanupContext, bson.D{{Key: "target_id", Value: eventID}}); err != nil {
			t.Errorf("清理重驱审计 %q: %v", eventID, err)
		}
	})
	return fixture
}

// command 返回夹具**稳定**的请求：同一 key 重复调用就是重放。
func (fixture *redriveFixture) command() outbox.RedriveCommand {
	return fixture.commandWithKey(fixture.key)
}

func (fixture *redriveFixture) commandWithKey(key string) outbox.RedriveCommand {
	return outbox.RedriveCommand{
		EventID: fixture.eventID, ExpectedRedriveCount: 0,
		ActorID: "admin-1", Reason: "provider 已恢复", Key: key, At: redriveAuditNow,
	}
}

func (fixture *redriveFixture) usecase(audits redrive.AuditWriter) *redrive.Usecase {
	return redrive.NewUsecaseWithClock(
		NewRedriveEventStore(fixture.storage), redriveGateStub{},
		redriveLedgerStub{status: ledger.ReservationStatusReserved},
		audits, NewTxRunner(fixture.storage), func() time.Time { return redriveAuditNow },
	)
}

func (fixture *redriveFixture) event(t *testing.T) redriveEventSnapshot {
	t.Helper()
	ctx, cancel := newMongoTestContext()
	defer cancel()
	var document redriveEventSnapshot
	if err := fixture.database.Collection(schema.CollectionOutboxEvents).FindOne(ctx, bson.D{{Key: "_id", Value: fixture.eventID}}).Decode(&document); err != nil {
		t.Fatalf("读取发件箱事件: %v", err)
	}
	return document
}

func (fixture *redriveFixture) auditCount(t *testing.T) int64 {
	t.Helper()
	ctx, cancel := newMongoTestContext()
	defer cancel()
	count, err := fixture.database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.D{{Key: "target_id", Value: fixture.eventID}})
	if err != nil {
		t.Fatalf("统计重驱审计: %v", err)
	}
	return count
}

func TestMongoRedrive重驱与审计同事务成功(t *testing.T) {
	fixture := newRedriveAttentionEvent(t)
	usecase := fixture.usecase(NewRedriveAuditWriter(fixture.storage))
	before := fixture.event(t)

	result, err := usecase.Redrive(context.Background(), fixture.command())
	if err != nil {
		t.Fatalf("Redrive() error = %v", err)
	}
	if result.Replayed || result.RedriveCount != 1 {
		t.Fatalf("result = %#v", result)
	}

	stored := fixture.event(t)
	if stored.DeliveryStatus != string(outbox.DeliveryStatusPending) || stored.RedriveCount != 1 {
		t.Fatalf("事件未离开人工关注: %#v", stored)
	}
	if stored.AttentionReason != "" {
		t.Fatalf("attention_reason 未清空: %q", stored.AttentionReason)
	}
	// 冻结事实：payload 与 attempt_count 一律不得被重驱改写。
	if string(stored.Payload) != string(before.Payload) {
		t.Fatalf("payload 被改写:\n before=%s\n after =%s", before.Payload, stored.Payload)
	}
	if stored.AttemptCount != before.AttemptCount {
		t.Fatalf("attempt_count 被改写: %d → %d", before.AttemptCount, stored.AttemptCount)
	}
	if !strings.Contains(string(stored.Payload), redrivePayloadMarker) {
		t.Fatal("夹具 payload 缺少标记，载荷泄漏断言会失去意义")
	}
	if !stored.RedriveStartedAt.Equal(redriveAuditNow) || stored.RedriveAttemptBase != stored.AttemptCount {
		t.Fatalf("窗口未按重驱时刻开启: %#v", stored)
	}
	if fixture.auditCount(t) != 1 {
		t.Fatal("同事务必须留下恰好一条审计")
	}

	// 审计记录不得携带事件载荷。
	ctx, cancel := newMongoTestContext()
	defer cancel()
	var raw bson.M
	if err := fixture.database.Collection(schema.CollectionAdminAudit).FindOne(ctx, bson.D{{Key: "target_id", Value: fixture.eventID}}).Decode(&raw); err != nil {
		t.Fatalf("读取审计原始文档: %v", err)
	}
	rendered := strings.Join(flattenAuditDocument(raw), " ")
	for _, marker := range []string{redrivePayloadMarker, "payload", "resultRef", "results.example.test"} {
		if strings.Contains(rendered, marker) {
			t.Fatalf("审计文档出现载荷痕迹 %q: %s", marker, rendered)
		}
	}
	// 审计必须自足到能回放结果：序号、窗口基线与时刻都要在。
	for _, required := range []string{"admin-1", "outbox.redrive", "redrive_count", "window_attempt_base"} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("审计缺少回放所需字段 %q: %s", required, rendered)
		}
	}

	// 重放：同一请求不得产生第二次重驱、也不得新增审计。
	replay, err := usecase.Redrive(context.Background(), fixture.command())
	if err != nil {
		t.Fatalf("重放 Redrive() error = %v", err)
	}
	if !replay.Replayed {
		t.Fatal("重放结果必须标记 Replayed")
	}
	if stored := fixture.event(t); stored.RedriveCount != 1 {
		t.Fatalf("重放推进了次数: %d", stored.RedriveCount)
	}
	if fixture.auditCount(t) != 1 {
		t.Fatal("重放不得新增审计")
	}
}

func TestMongoRedrive审计写入失败时事件CAS必须回滚(t *testing.T) {
	fixture := newRedriveAttentionEvent(t)
	usecase := fixture.usecase(failingRedriveAuditWriter{err: errors.New("audit collection unavailable")})

	if _, err := usecase.Redrive(context.Background(), fixture.command()); err == nil {
		t.Fatal("审计写入失败必须让整体失败")
	}
	// 这就是「同一事务闭环」的可执行证据：事件必须仍然停在 needs_attention，
	// 不能出现「事件被重驱了但没有审计」的半完成状态。
	stored := fixture.event(t)
	if stored.DeliveryStatus != string(outbox.DeliveryStatusNeedsAttention) || stored.RedriveCount != 0 {
		t.Fatalf("审计失败后事件未被回滚: %#v", stored)
	}
	if !stored.RedriveStartedAt.IsZero() {
		t.Fatalf("审计失败后窗口不应被推进: %v", stored.RedriveStartedAt)
	}
	if fixture.auditCount(t) != 0 {
		t.Fatal("审计失败不得留下记录")
	}
}

func TestMongoRedrive并发同令牌只允许一次重驱(t *testing.T) {
	fixture := newRedriveAttentionEvent(t)
	usecase := fixture.usecase(NewRedriveAuditWriter(fixture.storage))

	const attempts = 4
	// 各请求的幂等键不同，因此拼的是同一个次数令牌：只能有一个赢。
	commands := make([]outbox.RedriveCommand, attempts)
	for index := range commands {
		commands[index] = fixture.commandWithKey("ticket-" + uuid.NewString())
	}
	results := make([]error, attempts)
	var wait sync.WaitGroup
	wait.Add(attempts)
	for index := 0; index < attempts; index++ {
		go func(index int) {
			defer wait.Done()
			_, results[index] = usecase.Redrive(context.Background(), commands[index])
		}(index)
	}
	wait.Wait()

	succeeded := 0
	for index, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, outbox.ErrRedriveConflict) && !errors.Is(err, outbox.ErrRedriveNotEligible) &&
			!errors.Is(err, redrive.ErrRedriveAuditConflict) {
			t.Fatalf("并发失败 %d 的错误不在允许集合内: %v", index, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("成功次数 = %d, want 恰好 1（次数令牌必须挡住并发）", succeeded)
	}
	if stored := fixture.event(t); stored.RedriveCount != 1 {
		t.Fatalf("并发后 redrive_count = %d, want 1", stored.RedriveCount)
	}
	if fixture.auditCount(t) != 1 {
		t.Fatal("并发后必须恰好一条审计")
	}
}

// flattenAuditDocument 把 BSON 文档摊平成可搜索的文本，用于断言「没有载荷痕迹」。
func flattenAuditDocument(document bson.M) []string {
	rendered := make([]string, 0, len(document)*2)
	for key, value := range document {
		rendered = append(rendered, key)
		switch typed := value.(type) {
		case string:
			rendered = append(rendered, typed)
		case []byte:
			rendered = append(rendered, string(typed))
		case bson.M:
			rendered = append(rendered, flattenAuditDocument(typed)...)
		case bson.D:
			nested := bson.M{}
			for _, element := range typed {
				nested[element.Key] = element.Value
			}
			rendered = append(rendered, flattenAuditDocument(nested)...)
		}
	}
	return rendered
}
