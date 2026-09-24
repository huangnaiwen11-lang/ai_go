package data

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// seedConsumerInbox 通过 ACK 侧入口写入一条真实投递，再返回消费侧使用的键。
// 刻意不直接 InsertOne：消费侧必须能读到「ACK 真正落下的那条记录」，否则
// 两侧的字段名一旦漂移，测试夹具会替实现把问题掩盖过去。
func seedConsumerInbox(t *testing.T, fixture *submissionMongoFixture) generation.ProviderInboxKey {
	t.Helper()
	store := NewGenerationProviderInboxRepository(fixture.data)
	record, err := generation.NewProviderInboxRecord(
		"test-b2b", "delivery-"+fixture.stepID, fixture.stepID, "job-"+fixture.stepID, "",
		[]byte(`{"jobId":"job-1","status":"completed","resultUrl":"https://example.invalid/r.png"}`), 1, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		_, applyErr := store.ApplyAndSchedule(ctx, record)
		return applyErr
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		_, _ = fixture.database.Collection(schema.CollectionGenerationProviderInbox).DeleteMany(ctx, bson.D{{Key: "step_id", Value: fixture.stepID}})
		_, _ = fixture.events.DeleteMany(ctx, bson.D{{Key: "aggregate_id", Value: fixture.stepID}})
	})
	return generation.ProviderInboxKey{Source: generation.ProviderInboxSource, AccountRef: "test-b2b", DeliveryID: record.DeliveryID}
}

func consumerTerminalDigest() string {
	return generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, "https://example.invalid/r.png")
}

func readConsumerInboxDocument(t *testing.T, fixture *submissionMongoFixture) model.ProviderInboxDocument {
	t.Helper()
	var document model.ProviderInboxDocument
	if err := fixture.database.Collection(schema.CollectionGenerationProviderInbox).
		FindOne(fixture.ctx, bson.D{{Key: "step_id", Value: fixture.stepID}}).Decode(&document); err != nil {
		t.Fatalf("读回 inbox 文档: %v", err)
	}
	return document
}

func TestMongoProviderInboxConsumer读回ACK写入的投递事实(t *testing.T) {
	fixture, _ := seedPreparedProviderIntent(t)
	key := seedConsumerInbox(t, fixture)

	record, err := NewGenerationProviderInboxConsumerStore(fixture.data).ReadProviderInbox(fixture.ctx, key)
	if err != nil {
		t.Fatalf("读回投递: %v", err)
	}
	if record == nil {
		t.Fatal("ACK 已写入的投递读不回来")
	}
	if record.StepID != fixture.stepID || record.AccountRef != "test-b2b" || record.Status != generation.ProviderInboxPending {
		t.Fatalf("投递事实不符: %#v", record)
	}
	if record.PayloadDigest == "" || len(record.Payload) == 0 {
		t.Fatalf("投递原文与摘要缺失: %#v", record)
	}
}

func TestMongoProviderInboxConsumer未找到投递返回空而不是故障(t *testing.T) {
	fixture := newSubmissionMongoFixture(t)
	store := NewGenerationProviderInboxConsumerStore(fixture.data)
	missing := generation.ProviderInboxKey{Source: generation.ProviderInboxSource, AccountRef: "test-b2b", DeliveryID: "delivery-" + fixture.stepID}
	record, err := store.ReadProviderInbox(fixture.ctx, missing)
	if err != nil || record != nil {
		t.Fatalf("未找到的投递 = %#v, %v；期望 (nil, nil)", record, err)
	}
	// 非法身份必须在触达存储之前就被拒绝，不能退化成一次必然为空的查询。
	if _, err := store.ReadProviderInbox(fixture.ctx, generation.ProviderInboxKey{Source: generation.ProviderInboxSource, AccountRef: " test-b2b", DeliveryID: "delivery-x"}); !errors.Is(err, generation.ErrInvalidProviderInboxKey) {
		t.Fatalf("非法键错误 = %v，期望 ErrInvalidProviderInboxKey", err)
	}
}

func TestMongoProviderInboxConsumer把投递推进为已消费(t *testing.T) {
	fixture, _ := seedPreparedProviderIntent(t)
	key := seedConsumerInbox(t, fixture)
	store := NewGenerationProviderInboxConsumerStore(fixture.data)
	digest := consumerTerminalDigest()
	at := fixture.now.Add(time.Minute)

	if err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		return store.MarkProviderInboxApplied(ctx, key, digest, at)
	}); err != nil {
		t.Fatalf("推进投递: %v", err)
	}

	document := readConsumerInboxDocument(t, fixture)
	if document.Status != string(generation.ProviderInboxApplied) || document.TerminalDigest != digest {
		t.Fatalf("投递未推进: %#v", document)
	}
	if !document.UpdatedAt.Equal(at.UTC()) {
		t.Fatalf("业务时刻未落库: %v，期望 %v", document.UpdatedAt, at.UTC())
	}
	// 推进后仍必须能通过领域校验：写坏摘要会让后续读回整体失败。
	if _, err := store.ReadProviderInbox(fixture.ctx, key); err != nil {
		t.Fatalf("推进后读回失败: %v", err)
	}
}

func TestMongoProviderInboxConsumer重复消费幂等且不改写摘要(t *testing.T) {
	fixture, _ := seedPreparedProviderIntent(t)
	key := seedConsumerInbox(t, fixture)
	store := NewGenerationProviderInboxConsumerStore(fixture.data)
	digest := consumerTerminalDigest()
	first := fixture.now.Add(time.Minute)
	second := first.Add(time.Hour)

	mark := func(d string, at time.Time) error {
		return fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
			return store.MarkProviderInboxApplied(ctx, key, d, at)
		})
	}
	if err := mark(digest, first); err != nil {
		t.Fatal(err)
	}
	if err := mark(digest, second); err != nil {
		t.Fatalf("同结论重放应当幂等成功: %v", err)
	}
	document := readConsumerInboxDocument(t, fixture)
	if document.TerminalDigest != digest || !document.UpdatedAt.Equal(first.UTC()) {
		t.Fatalf("重放改写了已消费记录: %#v", document)
	}

	// 同一投递被消费出第二个结论：必须报冲突，且不能覆盖先落地的摘要。
	if err := mark(generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalFailed, ""), second); !errors.Is(err, generation.ErrProviderInboxConflict) {
		t.Fatalf("异结论重放错误 = %v，期望 ErrProviderInboxConflict", err)
	}
	if after := readConsumerInboxDocument(t, fixture); after.TerminalDigest != digest || !after.UpdatedAt.Equal(first.UTC()) {
		t.Fatalf("冲突覆盖了已消费记录: %#v", after)
	}
}

func TestMongoProviderInboxConsumer拒绝已被隔离的投递(t *testing.T) {
	fixture, _ := seedPreparedProviderIntent(t)
	key := seedConsumerInbox(t, fixture)
	// ACK 侧判定字节冲突后会把记录置为 quarantined；消费侧不得把它拉回来。
	if _, err := fixture.database.Collection(schema.CollectionGenerationProviderInbox).UpdateOne(
		fixture.ctx,
		bson.D{{Key: "step_id", Value: fixture.stepID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: string(generation.ProviderInboxQuarantined)}}}},
	); err != nil {
		t.Fatal(err)
	}
	store := NewGenerationProviderInboxConsumerStore(fixture.data)
	err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		return store.MarkProviderInboxApplied(ctx, key, consumerTerminalDigest(), fixture.now.Add(time.Minute))
	})
	if !errors.Is(err, generation.ErrProviderInboxConflict) {
		t.Fatalf("隔离投递被消费: %v", err)
	}
	if document := readConsumerInboxDocument(t, fixture); document.Status != string(generation.ProviderInboxQuarantined) {
		t.Fatalf("隔离状态被改写: %#v", document)
	}
}

func TestMongoProviderInboxConsumer要求事务与合法摘要(t *testing.T) {
	fixture, _ := seedPreparedProviderIntent(t)
	key := seedConsumerInbox(t, fixture)
	store := NewGenerationProviderInboxConsumerStore(fixture.data)
	digest := consumerTerminalDigest()
	at := fixture.now.Add(time.Minute)

	// 没有事务就没有推进权限：否则投递会被标成已消费而终态并未落地。
	if err := store.MarkProviderInboxApplied(fixture.ctx, key, digest, at); err == nil {
		t.Fatal("缺少事务的推进被接受")
	}
	for name, testCase := range map[string]struct {
		digest string
		at     time.Time
	}{
		"空摘要":   {"", at},
		"大写摘要":  {strings.ToUpper(digest), at},
		"长度不足":  {digest[:63], at},
		"非十六进制": {strings.Repeat("z", 64), at},
		"零业务时刻": {digest, time.Time{}},
	} {
		t.Run(name, func(t *testing.T) {
			err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
				return store.MarkProviderInboxApplied(ctx, key, testCase.digest, testCase.at)
			})
			if err == nil {
				t.Fatal("非法入参被接受")
			}
		})
	}
	if document := readConsumerInboxDocument(t, fixture); document.Status != string(generation.ProviderInboxPending) || document.TerminalDigest != "" {
		t.Fatalf("非法入参改动了投递: %#v", document)
	}
}

func TestMongoProviderInboxConsumer推进随事务回滚(t *testing.T) {
	fixture, _ := seedPreparedProviderIntent(t)
	key := seedConsumerInbox(t, fixture)
	store := NewGenerationProviderInboxConsumerStore(fixture.data)
	abort := errors.New("forced abort")

	err := fixture.runner.WithinTx(fixture.ctx, func(ctx context.Context) error {
		if markErr := store.MarkProviderInboxApplied(ctx, key, consumerTerminalDigest(), fixture.now.Add(time.Minute)); markErr != nil {
			return markErr
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("事务未按预期中止: %v", err)
	}
	document := readConsumerInboxDocument(t, fixture)
	if document.Status != string(generation.ProviderInboxPending) || document.TerminalDigest != "" {
		t.Fatalf("投递推进逃逸了回滚: %#v", document)
	}
}

func TestMongoProviderInboxConsumer未装配时拒绝(t *testing.T) {
	store := NewGenerationProviderInboxConsumerStore(&Data{})
	if _, err := store.ReadProviderInbox(context.Background(), generation.ProviderInboxKey{Source: generation.ProviderInboxSource, AccountRef: "test-b2b", DeliveryID: "delivery-x"}); err == nil {
		t.Fatal("未装配的存储被接受")
	}
}
