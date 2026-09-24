package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestRun在隔离Mongo执行真实重驱闭环覆盖命令入口到 Mongo 事务的整条链路。
// 它不使用业务 Mongo：缺少 CLING_TEST_MONGO_URI 时跳过，隔离脚本会提供一个
// 临时 rs0。事件、创作、步骤、预留和审计均用随机 ID，并在测试结束时精确删除。
func TestRun在隔离Mongo执行真实重驱闭环(t *testing.T) {
	uri := strings.TrimSpace(os.Getenv("CLING_TEST_MONGO_URI"))
	if uri == "" {
		t.Skip("set CLING_TEST_MONGO_URI to run the redrive CLI integration drill")
	}

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect isolated MongoDB: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	database := client.Database("cling_main")
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("initialize isolated MongoDB schema: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	creationID := "creation-" + uuid.NewString()
	stepID := "step-" + uuid.NewString()
	jobID := "job-" + uuid.NewString()
	resultRef := "https://results.example.test/" + stepID + ".png"
	terminalDigest := generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, resultRef)
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(generation.ProviderResultMaterializeEventPayload{
		CreationID: creationID, StepID: stepID, Provider: creations.PolarStarB2BProvider,
		AccountRef: "account-main", JobID: jobID, Capability: "text_to_image", ResultRef: resultRef,
		TerminalVersion: 1, TerminalDigest: terminalDigest,
	})
	if err != nil {
		t.Fatalf("marshal materialization event payload: %v", err)
	}
	eventID := generation.ProviderResultMaterializeEventID(stepID)
	if eventID == "" {
		t.Fatalf("derive event ID from step %q", stepID)
	}

	collections := []string{
		schema.CollectionAdminAudit, schema.CollectionOutboxEvents, schema.CollectionCreationSteps,
		schema.CollectionReservations, schema.CollectionCreations,
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, collection := range collections {
			var filter bson.D
			switch collection {
			case schema.CollectionAdminAudit:
				filter = bson.D{{Key: "target_id", Value: eventID}}
			case schema.CollectionOutboxEvents:
				filter = bson.D{{Key: "_id", Value: eventID}}
			case schema.CollectionCreationSteps:
				filter = bson.D{{Key: "_id", Value: stepID}}
			case schema.CollectionReservations, schema.CollectionCreations:
				filter = bson.D{{Key: "creation_id", Value: creationID}}
				if collection == schema.CollectionCreations {
					filter = bson.D{{Key: "_id", Value: creationID}}
				}
			}
			if _, err := database.Collection(collection).DeleteMany(cleanupContext, filter); err != nil {
				t.Errorf("cleanup %s: %v", collection, err)
			}
		}
	})

	if _, err := database.Collection(schema.CollectionCreations).InsertOne(ctx, model.CreationDocument{
		ID: creationID, Status: string(creations.CreationStatusPendingSubmission), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed creation: %v", err)
	}
	if _, err := database.Collection(schema.CollectionCreationSteps).InsertOne(ctx, model.CreationStepDocument{
		ID: stepID, CreationID: creationID, Sequence: 1, Atom: string(creations.AtomTextToImage),
		Provider: creations.PolarStarB2BProvider, AccountRef: "account-main", ExternalExecutionID: jobID,
		TerminalResultRef: resultRef, SubmitStatus: string(creations.StepSubmitStatusSubmitted),
	}); err != nil {
		t.Fatalf("seed creation step: %v", err)
	}
	if _, err := database.Collection(schema.CollectionCreationSteps).UpdateOne(ctx, bson.D{{Key: "_id", Value: stepID}}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "terminal_version", Value: int64(1)},
		{Key: "terminal_status", Value: string(generation.ProviderTerminalCompleted)},
		{Key: "terminal_digest", Value: terminalDigest},
	}}}); err != nil {
		t.Fatalf("seed terminal fact: %v", err)
	}
	if _, err := database.Collection(schema.CollectionReservations).InsertOne(ctx, model.ReservationDocument{
		ID: "reservation-" + uuid.NewString(), CreationID: creationID, UserID: "user-1",
		Status: string(ledger.ReservationStatusReserved), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	if _, err := database.Collection(schema.CollectionOutboxEvents).InsertOne(ctx, model.OutboxEventDocument{
		ID: eventID, AggregateID: creationID, EventType: generation.ProviderResultMaterializeEventType, Payload: payload,
		DeliveryStatus:  string(outbox.DeliveryStatusNeedsAttention),
		AttentionReason: string(outbox.AttentionReasonMaterialUploadBudget), AttemptCount: 5,
		CreatedAt: now.Add(-25 * time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed needs_attention event: %v", err)
	}

	command := outbox.RedriveCommand{
		EventID: eventID, ExpectedRedriveCount: 0, ActorID: "local-operator",
		Reason: "隔离环境重驱演练", Key: "drill-" + uuid.NewString(),
	}
	if err := run("../../configs/config.yaml", command); err != nil {
		t.Fatalf("run redrive-attention: %v", err)
	}

	var event model.OutboxEventDocument
	if err := database.Collection(schema.CollectionOutboxEvents).FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&event); err != nil {
		t.Fatalf("read redriven event: %v", err)
	}
	if event.DeliveryStatus != string(outbox.DeliveryStatusPending) || event.RedriveCount != 1 ||
		event.RedriveStartedAt.IsZero() || event.RedriveAttemptBase != event.AttemptCount || event.AttentionReason != "" {
		t.Fatalf("redriven event = %#v", event)
	}
	if string(event.Payload) != string(payload) || !event.CreatedAt.Equal(now.Add(-25*time.Hour)) {
		t.Fatal("CLI 重驱不得改写 payload 或 created_at 冻结事实")
	}
	count, err := database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.D{{Key: "target_id", Value: eventID}})
	if err != nil {
		t.Fatalf("count redrive audits: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit count = %d, want 1", count)
	}

	// 同一个人工请求是重放，而不是第二次重驱。这里再次走命令入口，确保
	// CLI 的时间盖章不会破坏确定性审计键。
	if err := run("../../configs/config.yaml", command); err != nil {
		t.Fatalf("replay redrive-attention: %v", err)
	}
	if err := database.Collection(schema.CollectionOutboxEvents).FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&event); err != nil {
		t.Fatalf("read replayed event: %v", err)
	}
	if event.RedriveCount != 1 {
		t.Fatalf("replay redrive_count = %d, want 1", event.RedriveCount)
	}
	count, err = database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.D{{Key: "target_id", Value: eventID}})
	if err != nil {
		t.Fatalf("count replay audits: %v", err)
	}
	if count != 1 {
		t.Fatalf("replay audit count = %d, want 1", count)
	}
}
