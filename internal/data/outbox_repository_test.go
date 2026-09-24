package data

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type outboxLeaseRenewer interface {
	RenewLease(context.Context, string, string, time.Time, time.Time) error
}

func TestMongoOutbox保留原始JSON载荷字节(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	payload := []byte(`{"prompt":"x","assets":[]}`)
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), payload, now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})

	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	event.Payload[0] = '!'
	claimed, err := repository.Claim(ctx, "worker-1", now, now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("Claim() event/error = %#v / %v", claimed, err)
	}
	if !bytes.Equal(claimed.Payload, payload) {
		t.Fatal("Claim() 未保留原始 JSON 载荷字节")
	}
}

func TestMongoOutbox续租只延展活跃匹配租约且保留冻结事实(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	renewer, ok := repository.(outboxLeaseRenewer)
	if !ok {
		t.Fatal("outbox repository does not expose lease renewal")
	}
	now := time.Date(2026, time.September, 21, 10, 0, 0, 0, time.UTC)
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	payload := []byte(`{"prompt":"frozen","idempotencyKey":"cling-step:lease"}`)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), payload, now)
	if err != nil {
		t.Fatal(err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupCtx, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})
	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	claimed, err := repository.ClaimByIDAndType(ctx, "worker-1", eventID, outbox.EventTypeGenerationSubmission, now, now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("ClaimByIDAndType() event/error = %#v / %v", claimed, err)
	}

	renewedAt := now.Add(20 * time.Second)
	renewedUntil := now.Add(2 * time.Minute)
	if err := renewer.RenewLease(ctx, eventID, claimed.LeaseToken, renewedAt, renewedUntil); err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}
	var stored model.OutboxEventDocument
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&stored); err != nil {
		t.Fatalf("读取续租事件: %v", err)
	}
	if string(stored.Payload) != string(payload) || stored.AttemptCount != claimed.AttemptCount || stored.LeaseToken != claimed.LeaseToken || stored.LeaseOwner != claimed.LeaseOwner || !stored.LeaseUntil.Equal(renewedUntil) || !stored.UpdatedAt.Equal(renewedAt) {
		t.Fatalf("续租改写了冻结事实或未精确延展租约: %#v", stored)
	}
	if err := renewer.RenewLease(ctx, eventID, "wrong-token", renewedAt.Add(time.Second), renewedUntil.Add(time.Minute)); !errors.Is(err, outbox.ErrLeaseConflict) {
		t.Fatalf("错误 token RenewLease() error = %v, want ErrLeaseConflict", err)
	}
	if err := renewer.RenewLease(ctx, eventID, claimed.LeaseToken, renewedUntil.Add(time.Second), renewedUntil.Add(2*time.Minute)); !errors.Is(err, outbox.ErrLeaseConflict) {
		t.Fatalf("过期租约 RenewLease() error = %v, want ErrLeaseConflict", err)
	}
}

func TestMongoOutbox重驱开启新窗口且不改写冻结事实(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	now := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"prompt":"冻结事实","jobId":"job-1","idempotencyKey":"cling-step:step-1"}`)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), payload, now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})

	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	claimed, err := repository.ClaimByIDAndType(ctx, "worker-1", eventID, outbox.EventTypeGenerationSubmission, now, now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("ClaimByIDAndType() event/error = %#v / %v", claimed, err)
	}
	// 让 AttemptCount 非零，才能验证重驱窗口的基数确实是「重驱那一刻的计数」。
	if err := repository.Requeue(ctx, eventID, claimed.LeaseToken, now); err != nil {
		t.Fatalf("Requeue() error = %v", err)
	}
	claimed, err = repository.ClaimByIDAndType(ctx, "worker-1", eventID, outbox.EventTypeGenerationSubmission, now, now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("第二次 ClaimByIDAndType() event/error = %#v / %v", claimed, err)
	}
	attentionAt := now.Add(time.Minute)
	if err := repository.MarkNeedsAttention(ctx, eventID, claimed.LeaseToken, outbox.AttentionReasonMaterialUploadBudget, attentionAt); err != nil {
		t.Fatalf("MarkNeedsAttention() error = %v", err)
	}

	var before model.OutboxEventDocument
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&before); err != nil {
		t.Fatalf("读取重驱前事件: %v", err)
	}

	redrivenAt := attentionAt.Add(30 * time.Hour)
	redriven, err := repository.RedriveAttention(ctx, outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: 0, ActorID: "admin-1", Reason: "provider 已恢复", Key: "ticket-42", At: redrivenAt,
	})
	if err != nil {
		t.Fatalf("RedriveAttention() error = %v", err)
	}
	if redriven.DeliveryStatus != outbox.DeliveryStatusPending || redriven.RedriveCount != 1 {
		t.Fatalf("重驱后事件 = %#v", redriven)
	}

	var after model.OutboxEventDocument
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&after); err != nil {
		t.Fatalf("读取重驱后事件: %v", err)
	}
	// 冻结事实：payload、attempt_count、created_at 一律不得被重驱改写。
	if string(after.Payload) != string(before.Payload) {
		t.Fatalf("payload 被重驱改写:\n before=%s\n after =%s", before.Payload, after.Payload)
	}
	if after.AttemptCount != before.AttemptCount {
		t.Fatalf("attempt_count 被改写: %d → %d", before.AttemptCount, after.AttemptCount)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("created_at 被改写: %v → %v", before.CreatedAt, after.CreatedAt)
	}
	if after.RedriveAttemptBase != before.AttemptCount {
		t.Fatalf("redrive_attempt_base = %d, want 重驱时的 attempt_count %d", after.RedriveAttemptBase, before.AttemptCount)
	}
	if !after.RedriveStartedAt.Equal(redrivenAt) || !after.NextAttemptAt.Equal(redrivenAt) || !after.UpdatedAt.Equal(redrivenAt) {
		t.Fatalf("重驱未开启新窗口: %#v", after)
	}
	if after.AttentionReason != "" {
		t.Fatalf("attention_reason 未清空: %q", after.AttentionReason)
	}
	if after.LeaseToken != "" || after.LeaseOwner != "" || !after.LeaseUntil.IsZero() {
		t.Fatalf("重驱不应留下租约: %#v", after)
	}

	// 重驱的全部意义：事件必须重新可被自动路径领取。
	reclaimed, err := repository.ClaimByIDAndType(ctx, "worker-2", eventID, outbox.EventTypeGenerationSubmission, redrivenAt, redrivenAt.Add(time.Minute))
	if err != nil || reclaimed == nil {
		t.Fatalf("重驱后事件不可领取: %#v / %v", reclaimed, err)
	}
	if err := repository.MarkNeedsAttention(ctx, eventID, reclaimed.LeaseToken, outbox.AttentionReasonEventAge, redrivenAt.Add(time.Minute)); err != nil {
		t.Fatalf("第二次 MarkNeedsAttention() error = %v", err)
	}

	// 次数令牌已过期：操作者看到的还是 0，但库里已经是 1 → 冲突，不得静默重驱。
	if _, err := repository.RedriveAttention(ctx, outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: 0, ActorID: "admin-1", Reason: "provider 已恢复", Key: "ticket-42", At: redrivenAt.Add(2 * time.Minute),
	}); err != outbox.ErrRedriveConflict {
		t.Fatalf("过期次数令牌 error = %v, want ErrRedriveConflict", err)
	}

	// 第二次重驱是合法的新请求（键因次数不同而不同），成功并到达 V1 上限。
	if _, err := repository.RedriveAttention(ctx, outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: 1, ActorID: "admin-1", Reason: "再次确认 provider 正常", Key: "ticket-42", At: redrivenAt.Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("第二次 RedriveAttention() error = %v", err)
	}
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&after); err != nil {
		t.Fatalf("读取第二次重驱后事件: %v", err)
	}
	if after.RedriveCount != outbox.MaxRedriveCount || after.DeliveryStatus != string(outbox.DeliveryStatusPending) {
		t.Fatalf("第二次重驱后 = %#v", after)
	}

	// 已达 V1 上限：先把事件放回 needs_attention，再验证不再可重驱。
	reclaimed, err = repository.ClaimByIDAndType(ctx, "worker-3", eventID, outbox.EventTypeGenerationSubmission, redrivenAt.Add(5*time.Minute), redrivenAt.Add(6*time.Minute))
	if err != nil || reclaimed == nil {
		t.Fatalf("第三次领取失败: %#v / %v", reclaimed, err)
	}
	if err := repository.MarkNeedsAttention(ctx, eventID, reclaimed.LeaseToken, outbox.AttentionReasonEventAge, redrivenAt.Add(6*time.Minute)); err != nil {
		t.Fatalf("第三次 MarkNeedsAttention() error = %v", err)
	}
	if _, err := repository.RedriveAttention(ctx, outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: outbox.MaxRedriveCount, ActorID: "admin-1", Reason: "第三次", Key: "ticket-43", At: redrivenAt.Add(7 * time.Minute),
	}); err != outbox.ErrRedriveNotEligible {
		t.Fatalf("达到上限后 error = %v, want ErrRedriveNotEligible", err)
	}
}

func TestMongoOutbox重驱拒绝非人工关注事件(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	now := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), []byte(`{"prompt":"x"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})
	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// pending 事件仍被自动路径持有：重驱它等于抢占，必须拒绝。
	if _, err := repository.RedriveAttention(ctx, outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: 0, ActorID: "admin-1", Reason: "抢跑", Key: "ticket-1", At: now,
	}); err != outbox.ErrRedriveNotEligible {
		t.Fatalf("pending 事件 error = %v, want ErrRedriveNotEligible", err)
	}
	// 非法请求必须在接触存储前被拒。
	if _, err := repository.RedriveAttention(ctx, outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: 0, ActorID: "", Reason: "匿名", Key: "ticket-1", At: now,
	}); err != outbox.ErrInvalidRedriveCommand {
		t.Fatalf("缺操作者 error = %v, want ErrInvalidRedriveCommand", err)
	}
	// 不存在的事件是冲突而不是资格问题。
	if _, err := repository.RedriveAttention(ctx, outbox.RedriveCommand{
		EventID: outbox.SubmissionEventID("step-" + uuid.NewString()), ExpectedRedriveCount: 0,
		ActorID: "admin-1", Reason: "不存在", Key: "ticket-1", At: now,
	}); err != outbox.ErrRedriveConflict {
		t.Fatalf("不存在事件 error = %v, want ErrRedriveConflict", err)
	}
}

func TestMongoOutbox人工关注是终态且保留固定原因(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	now := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), []byte(`{"prompt":"x"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})

	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	// 按 _id 精确领取，避免与同库其他测试的事件互相干扰。
	claimed, err := repository.ClaimByIDAndType(ctx, "worker-1", eventID, outbox.EventTypeGenerationSubmission, now, now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("ClaimByIDAndType() event/error = %#v / %v", claimed, err)
	}

	attentionAt := now.Add(time.Minute)
	if err := repository.MarkNeedsAttention(ctx, eventID, claimed.LeaseToken, outbox.AttentionReasonMaterialUploadBudget, attentionAt); err != nil {
		t.Fatalf("MarkNeedsAttention() error = %v", err)
	}

	// needs_attention 必须是终态：若仍可领取，预算耗尽的事件会被无限重新领取，
	// 等于重试预算从未生效。
	if again, err := repository.ClaimByIDAndType(ctx, "worker-2", eventID, outbox.EventTypeGenerationSubmission, attentionAt.Add(time.Minute), attentionAt.Add(2*time.Minute)); err != nil || again != nil {
		t.Fatalf("needs_attention 事件被再次领取: %#v / %v", again, err)
	}

	var stored model.OutboxEventDocument
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&stored); err != nil {
		t.Fatalf("读取发件箱事件: %v", err)
	}
	if stored.DeliveryStatus != string(outbox.DeliveryStatusNeedsAttention) {
		t.Fatalf("delivery_status = %q, want needs_attention", stored.DeliveryStatus)
	}
	if stored.AttentionReason != string(outbox.AttentionReasonMaterialUploadBudget) ||
		!outbox.ValidAttentionReason(outbox.AttentionReason(stored.AttentionReason)) {
		t.Fatalf("attention_reason = %q, want 已登记的固定原因", stored.AttentionReason)
	}
	if stored.LeaseToken != "" || stored.LeaseOwner != "" || !stored.LeaseUntil.IsZero() {
		t.Fatalf("人工关注未清除租约: %#v", stored)
	}
	if !stored.UpdatedAt.Equal(attentionAt) {
		t.Fatalf("updated_at = %v, want %v", stored.UpdatedAt, attentionAt)
	}
}

func TestMongoOutboxMarkNeedsAttention拒绝错误租约与非法原因(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	now := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), []byte(`{"prompt":"x"}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})
	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	claimed, err := repository.ClaimByIDAndType(ctx, "worker-1", eventID, outbox.EventTypeGenerationSubmission, now, now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("ClaimByIDAndType() event/error = %#v / %v", claimed, err)
	}

	// 自由文本原因必须被拒绝：关注原因会出现在运营可见的记录里，
	// 允许自由文本等于允许把 provider 地址或对象 key 写进去。
	if err := repository.MarkNeedsAttention(ctx, eventID, claimed.LeaseToken, outbox.AttentionReason("upload failed: https://provider.example/x.png"), now.Add(time.Minute)); !errors.Is(err, outbox.ErrInvalidEvent) {
		t.Fatalf("非法原因的 MarkNeedsAttention() error = %v, want ErrInvalidEvent", err)
	}
	if err := repository.MarkNeedsAttention(ctx, eventID, "wrong-lease", outbox.AttentionReasonEventAge, now.Add(time.Minute)); !errors.Is(err, outbox.ErrLeaseConflict) {
		t.Fatalf("错误租约的 MarkNeedsAttention() error = %v, want ErrLeaseConflict", err)
	}

	var stored model.OutboxEventDocument
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&stored); err != nil {
		t.Fatalf("读取发件箱事件: %v", err)
	}
	if stored.DeliveryStatus != string(outbox.DeliveryStatusDispatching) || stored.AttentionReason != "" {
		t.Fatalf("被拒的调用改动了事件: %#v", stored)
	}
}

func TestMongoOutbox并发领取租约和结案(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	data := &Data{client: client, database: database}
	repository := NewOutboxRepository(data)
	stepID := "step-" + uuid.NewString()
	eventID := outbox.SubmissionEventID(stepID)
	payload := []byte(`{"secret":"must-not-leak"}`)
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), payload, now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})

	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if err := repository.Enqueue(ctx, event); !errors.Is(err, outbox.ErrEventAlreadyExists) {
		t.Fatalf("重复 Enqueue() error = %v, want ErrEventAlreadyExists", err)
	}
	wrongType, err := repository.ClaimByType(ctx, "worker-filter", outbox.EventType("other.event"), now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("非目标类型 ClaimByType() error = %v", err)
	}
	if wrongType != nil {
		t.Fatalf("非目标类型 ClaimByType() event = %#v, want nil", wrongType)
	}

	type claimResult struct {
		event *outbox.Event
		err   error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		workers.Add(1)
		go func(workerID string) {
			defer workers.Done()
			claimContext, claimCancel := newMongoTestContext()
			defer claimCancel()
			<-start
			claimed, claimErr := repository.Claim(claimContext, workerID, now, now.Add(time.Minute))
			results <- claimResult{event: claimed, err: claimErr}
		}("worker-" + string(rune('1'+index)))
	}
	close(start)
	workers.Wait()
	close(results)

	var claimed *outbox.Event
	for result := range results {
		if result.err != nil {
			t.Fatalf("Claim() error = %v", result.err)
		}
		if result.event != nil {
			if claimed != nil {
				t.Fatalf("两个并发 Claim() 都取得事件: %#v / %#v", claimed, result.event)
			}
			claimed = result.event
		}
	}
	if claimed == nil {
		t.Fatal("并发 Claim() 未取得任何事件")
	}
	if claimed.DeliveryStatus != outbox.DeliveryStatusDispatching || claimed.LeaseToken == "" || claimed.AttemptCount != 1 {
		t.Fatalf("Claim() event = %#v, want dispatching event with lease token and one attempt", claimed)
	}

	wrongTokenErr := repository.MarkDelivered(ctx, eventID, "wrong-lease-token", now.Add(time.Second))
	if !errors.Is(wrongTokenErr, outbox.ErrLeaseConflict) {
		t.Fatalf("错误租约 MarkDelivered() error = %v, want ErrLeaseConflict", wrongTokenErr)
	}
	if wrongTokenErr != nil && strings.Contains(wrongTokenErr.Error(), "must-not-leak") {
		t.Fatalf("错误租约错误泄露载荷: %q", wrongTokenErr)
	}
	if err := repository.MarkDelivered(ctx, eventID, claimed.LeaseToken, now.Add(time.Second)); err != nil {
		t.Fatalf("正确租约 MarkDelivered() error = %v", err)
	}
	claimedAgain, err := repository.Claim(ctx, "worker-3", now.Add(2*time.Second), now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("已投递后 Claim() error = %v", err)
	}
	if claimedAgain != nil {
		t.Fatalf("已投递事件再次被领取: %#v", claimedAgain)
	}
}

// TestMongoOutbox按ID领取不占用其他事件验证单次投递的领取条件在数据库层原子生效。
func TestMongoOutbox按ID领取不占用其他事件(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	// 该测试与其他包共用本机 cling_main。选择独立的历史时间窗，并先按 ID
	// 领取自身事件，避免并行测试的待投递事件被误领，进而污染回调等无关用例。
	now := time.Date(1970, time.January, 2, 0, 0, 0, 0, time.UTC)
	firstID := outbox.SubmissionEventID("step-first-" + uuid.NewString())
	targetID := outbox.SubmissionEventID("step-target-" + uuid.NewString())
	collection := database.Collection(schema.CollectionOutboxEvents)
	for _, eventID := range []string{firstID, targetID} {
		id := eventID
		t.Cleanup(func() {
			cleanupContext, cleanupCancel := newMongoTestContext()
			defer cleanupCancel()
			if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: id}}); err != nil {
				t.Errorf("按测试 _id 清理发件箱事件 %q: %v", id, err)
			}
		})
		event, err := outbox.NewPending(id, "creation-"+uuid.NewString(), []byte(`{"technical":"payload"}`), now)
		if err != nil {
			t.Fatalf("创建测试 outbox 事件: %v", err)
		}
		if err := repository.Enqueue(ctx, event); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
	}

	claimed, err := repository.ClaimByIDAndType(ctx, "worker-target", targetID, outbox.EventTypeGenerationSubmission, now, now.Add(time.Minute))
	if err != nil || claimed == nil || claimed.ID != targetID || claimed.AttemptCount != 1 {
		t.Fatalf("ClaimByIDAndType() event/error = %#v / %v，want only target claimed once", claimed, err)
	}
	var untouched model.OutboxEventDocument
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: firstID}}).Decode(&untouched); err != nil {
		t.Fatalf("读取未目标事件: %v", err)
	}
	if untouched.DeliveryStatus != string(outbox.DeliveryStatusPending) || untouched.AttemptCount != 0 || untouched.LeaseToken != "" {
		t.Fatalf("按 ID 领取影响了其他事件: %#v", untouched)
	}
}

func TestMongoOutbox过期Dispatching租约接管后进入对账(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	// 该测试与其他包共用本机 cling_main。选择独立的历史时间窗，并先按 ID
	// 领取自身事件，避免并行测试的待投递事件被误领，进而污染回调等无关用例。
	now := time.Date(1970, time.January, 2, 0, 0, 0, 0, time.UTC)
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), []byte(`{"execution":"technical"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})
	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	first, err := repository.ClaimByIDAndType(ctx, "worker-crashed", eventID, outbox.EventTypeGenerationSubmission, now, now.Add(time.Minute))
	if err != nil || first == nil || first.DeliveryStatus != outbox.DeliveryStatusDispatching {
		t.Fatalf("首次 ClaimByIDAndType() event/error = %#v / %v", first, err)
	}

	takeoverAt := now.Add(2 * time.Minute)
	second, err := repository.ClaimByType(ctx, "worker-recovery", outbox.EventTypeGenerationSubmission, takeoverAt, takeoverAt.Add(time.Minute))
	if err != nil {
		t.Fatalf("过期租约 ClaimByType() error = %v", err)
	}
	if second == nil || second.DeliveryStatus != outbox.DeliveryStatusReconciling || second.LeaseToken == "" || second.LeaseToken == first.LeaseToken || second.AttemptCount != 2 {
		t.Fatalf("接管事件 = %#v, want reconciling with new lease and second attempt", second)
	}
}

func TestMongoOutboxEnqueue拒绝Pending不可信错误摘要(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	sensitiveError := "https://example.invalid/generate?prompt=secret&key=private"
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event := &outbox.Event{
		ID:             eventID,
		AggregateID:    "creation-" + uuid.NewString(),
		EventType:      outbox.EventTypeGenerationSubmission,
		Payload:        []byte(`{"prompt":"x","assets":[]}`),
		DeliveryStatus: outbox.DeliveryStatusPending,
		NextAttemptAt:  now,
		LastError:      sensitiveError,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})

	repository := NewOutboxRepository(&Data{client: client, database: database})
	err := repository.Enqueue(ctx, event)
	if !errors.Is(err, outbox.ErrInvalidEvent) {
		t.Fatalf("Enqueue() error = %v, want ErrInvalidEvent", err)
	}
	if err != nil && strings.Contains(err.Error(), sensitiveError) {
		t.Fatalf("Enqueue() error 泄露不可信错误摘要: %q", err)
	}
	count, err := collection.CountDocuments(ctx, bson.D{{Key: "_id", Value: eventID}})
	if err != nil {
		t.Fatalf("统计不可信错误摘要事件: %v", err)
	}
	if count != 0 {
		t.Fatalf("不可信错误摘要事件写入数量 = %d, want 0", count)
	}
}

func TestMongoOutboxEnqueue拒绝直构超限载荷(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	payload := []byte(`{"padding":"` + strings.Repeat("x", 1<<20) + `"}`)
	if len(payload) <= 1<<20 {
		t.Fatalf("测试载荷长度 = %d, want > 1MiB", len(payload))
	}
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event := &outbox.Event{
		ID:             eventID,
		AggregateID:    "creation-" + uuid.NewString(),
		EventType:      outbox.EventTypeGenerationSubmission,
		Payload:        payload,
		DeliveryStatus: outbox.DeliveryStatusPending,
		NextAttemptAt:  now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})

	repository := NewOutboxRepository(&Data{client: client, database: database})
	if err := repository.Enqueue(ctx, event); !errors.Is(err, outbox.ErrInvalidEvent) {
		t.Fatalf("Enqueue() error = %v, want ErrInvalidEvent", err)
	}
	count, err := collection.CountDocuments(ctx, bson.D{{Key: "_id", Value: eventID}})
	if err != nil {
		t.Fatalf("统计超限事件: %v", err)
	}
	if count != 0 {
		t.Fatalf("超限事件写入数量 = %d, want 0", count)
	}
}

func TestMongoOutbox重新入队和失败要求准确租约(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewOutboxRepository(&Data{client: client, database: database})
	eventID := outbox.SubmissionEventID("step-" + uuid.NewString())
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	event, err := outbox.NewPending(eventID, "creation-"+uuid.NewString(), []byte(`{}`), now)
	if err != nil {
		t.Fatalf("NewPending() error = %v", err)
	}
	collection := database.Collection(schema.CollectionOutboxEvents)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: eventID}}); err != nil {
			t.Errorf("按测试 _id 清理发件箱事件 %q: %v", eventID, err)
		}
	})
	if err := repository.Enqueue(ctx, event); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	claimed, err := repository.Claim(ctx, "worker-1", now, now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("Claim() event/error = %#v / %v", claimed, err)
	}

	retryAt := now.Add(2 * time.Minute)
	if err := repository.Requeue(ctx, eventID, "wrong-lease-token", retryAt); !errors.Is(err, outbox.ErrLeaseConflict) {
		t.Fatalf("错误租约 Requeue() error = %v, want ErrLeaseConflict", err)
	}
	if err := repository.Requeue(ctx, eventID, claimed.LeaseToken, retryAt); err != nil {
		t.Fatalf("正确租约 Requeue() error = %v", err)
	}

	var stored model.OutboxEventDocument
	if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&stored); err != nil {
		t.Fatalf("读取重新入队事件: %v", err)
	}
	if stored.DeliveryStatus != string(outbox.DeliveryStatusPending) || stored.LeaseToken != "" || stored.LeaseOwner != "" || !stored.LeaseUntil.IsZero() {
		t.Fatalf("重新入队后事件 = %#v, want pending without lease", stored)
	}
	if !stored.NextAttemptAt.Equal(retryAt) || stored.LastError != outbox.RequeueLastError {
		t.Fatalf("重新入队时间或错误摘要非法: %#v", stored)
	}

	if unavailable, err := repository.Claim(ctx, "worker-2", now.Add(time.Minute), now.Add(3*time.Minute)); err != nil || unavailable != nil {
		t.Fatalf("尚未到重试时刻 Claim() event/error = %#v / %v", unavailable, err)
	}
	reclaimed, err := repository.Claim(ctx, "worker-2", retryAt, retryAt.Add(time.Minute))
	if err != nil || reclaimed == nil {
		t.Fatalf("到重试时刻 Claim() event/error = %#v / %v", reclaimed, err)
	}
	if err := repository.MarkFailed(ctx, eventID, "wrong-lease-token", retryAt.Add(time.Second)); !errors.Is(err, outbox.ErrLeaseConflict) {
		t.Fatalf("错误租约 MarkFailed() error = %v, want ErrLeaseConflict", err)
	}
	if err := repository.MarkFailed(ctx, eventID, reclaimed.LeaseToken, retryAt.Add(time.Second)); err != nil {
		t.Fatalf("正确租约 MarkFailed() error = %v", err)
	}
	if unavailable, err := repository.Claim(ctx, "worker-3", retryAt.Add(2*time.Second), retryAt.Add(4*time.Minute)); err != nil || unavailable != nil {
		t.Fatalf("失败事件再次 Claim() event/error = %#v / %v", unavailable, err)
	}
}
