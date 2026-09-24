package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func inboxEventID(record generation.ProviderInboxRecord) string {
	sum := sha256.Sum256([]byte(record.Source + "\x00" + record.AccountRef + "\x00" + record.DeliveryID))
	return "generation.inbox.consume:" + hex.EncodeToString(sum[:])
}

func TestMongoProviderInboxReplayAfterConsumption(t *testing.T) {
	f, _ := seedPreparedProviderIntent(t)
	ack := generation.NewProviderCallbackUsecaseWithClock(NewGenerationProviderInboxRepository(f.data), f.runner, func() time.Time { return f.now })
	fact := generation.ProviderDeliveryFact{AccountRef: "test-b2b", DeliveryID: "delivery-" + f.stepID, StepID: f.stepID, JobID: "job-1", Attempt: 1, Payload: []byte(`{"status":"completed"}`)}
	if result, err := ack.Handle(f.ctx, fact); err != nil || result != generation.InboxApplyInserted {
		t.Fatalf("first ACK: %s / %v", result, err)
	}
	key := generation.ProviderInboxKey{Source: generation.ProviderInboxSource, AccountRef: fact.AccountRef, DeliveryID: fact.DeliveryID}
	digest := generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, "https://cdn.example.com/result.png")
	consumer := NewGenerationProviderInboxConsumerStore(f.data)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return consumer.MarkProviderInboxApplied(ctx, key, digest, f.now) }); err != nil {
		t.Fatal(err)
	}
	fact.Attempt = 2
	if result, err := ack.Handle(f.ctx, fact); err != nil || result != generation.InboxApplyNoop {
		t.Fatalf("consumed delivery replay: %s / %v", result, err)
	}
	record, err := consumer.ReadProviderInbox(f.ctx, key)
	if err != nil || record.Status != generation.ProviderInboxApplied || record.TerminalDigest != digest {
		t.Fatalf("consumption changed: %#v / %v", record, err)
	}
	fact.Payload = []byte(`{"status":"failed"}`)
	if result, err := ack.Handle(f.ctx, fact); err != nil || result != generation.InboxApplyQuarantined {
		t.Fatalf("changed body: %s / %v", result, err)
	}
	record, err = consumer.ReadProviderInbox(f.ctx, key)
	if err != nil || record.Status != generation.ProviderInboxQuarantined || record.TerminalDigest != digest {
		t.Fatalf("conflict not durable: %#v / %v", record, err)
	}
}

// ACK 前必须先把 delivery 归属到一条已冻结的 B2B 提交。签名只能证明报文来自
// 某个租户，不能证明它属于当前步骤；否则一个合法租户的串号回调会留下永久事实。
func TestMongoProviderInboxRejectsUnfrozenStepBeforeACK(t *testing.T) {
	f := newSubmissionMongoFixture(t)
	f.seedDispatching() // 这是 local execution.v2 步骤，没有 B2B 冻结意图。
	store := NewGenerationProviderInboxRepository(f.data)
	record, err := generation.NewProviderInboxRecord("test-b2b", "delivery-"+f.stepID, f.stepID, "job-early", "", []byte(`{"status":"completed"}`), 1, f.now)
	if err != nil {
		t.Fatal(err)
	}
	err = f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		_, applyErr := store.ApplyAndSchedule(ctx, record)
		return applyErr
	})
	if !errors.Is(err, generation.ErrProviderInboxStepMismatch) {
		t.Fatalf("unfrozen callback error = %v, want ErrProviderInboxStepMismatch", err)
	}
	count, err := f.database.Collection(schema.CollectionGenerationProviderInbox).CountDocuments(f.ctx, bson.M{"step_id": f.stepID})
	if err != nil || count != 0 {
		t.Fatalf("unrelated delivery was ACKed: %d / %v", count, err)
	}
}

// 两个生产者共用一个按步骤去重的 reconciliation 事件；existing ID 不能成为
// “只要类型相同就信任”的通行证。否则损坏的 aggregate/payload 会让 callback
// 侧 ACK 成功，但后续 Worker 在另一条创作事实上恢复。
func TestMongoProviderInboxRejectsInconsistentExistingReconcileEvent(t *testing.T) {
	for _, field := range []string{"aggregate_id", "payload"} {
		t.Run(field, func(t *testing.T) {
			f, command := seedProviderIntent(t)
			if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
				return providerIntentStore(t, f).PrepareProviderSubmission(ctx, command)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.events.UpdateOne(f.ctx, bson.M{"_id": generation.ProviderInboxRecoveryEventID(f.stepID)}, bson.M{"$set": bson.M{field: "wrong"}}); err != nil {
				t.Fatal(err)
			}
			store := NewGenerationProviderInboxRepository(f.data)
			record, err := generation.NewProviderInboxRecord("test-b2b", "delivery-"+field+"-"+f.stepID, f.stepID, "job-early", "", []byte(`{"status":"completed"}`), 1, f.now)
			if err != nil {
				t.Fatal(err)
			}
			err = f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
				_, applyErr := store.ApplyAndSchedule(ctx, record)
				return applyErr
			})
			if !errors.Is(err, generation.ErrProviderInboxConflict) {
				t.Fatalf("inconsistent %s accepted: %v", field, err)
			}
			count, err := f.database.Collection(schema.CollectionGenerationProviderInbox).CountDocuments(f.ctx, bson.M{"step_id": f.stepID})
			if err != nil || count != 0 {
				t.Fatalf("failed ACK left inbox: %d / %v", count, err)
			}
		})
	}
}

func TestMongoProviderInboxDeliveryRecoveryDoesNotCollideWithReconcile(t *testing.T) {
	f, command := seedProviderIntent(t)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		return providerIntentStore(t, f).PrepareProviderSubmission(ctx, command)
	}); err != nil {
		t.Fatal(err)
	}
	store := NewGenerationProviderInboxRepository(f.data)
	record, err := generation.NewProviderInboxRecord("test-b2b", "delivery-a", f.stepID, "job-1", "", []byte(`{"status":"completed"}`), 1, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range []string{"delivery-a", "delivery-b"} {
		record.DeliveryID = delivery
		if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
			_, err := store.ApplyAndSchedule(ctx, record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var event model.OutboxEventDocument
		if err := f.events.FindOne(f.ctx, bson.M{"_id": inboxEventID(record)}).Decode(&event); err != nil {
			t.Fatalf("delivery ACK has no independent recovery: %v", err)
		}
		if event.EventType != "generation.inbox.consume" || event.AggregateID != f.stepID || event.DeliveryStatus != "pending" {
			t.Fatalf("unexpected event: %#v", event)
		}
		_, err = f.events.UpdateOne(f.ctx, bson.M{"_id": event.ID}, bson.M{"$set": bson.M{"delivery_status": "delivered"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
			_, err := store.ApplyAndSchedule(ctx, record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.events.FindOne(f.ctx, bson.M{"_id": event.ID}).Decode(&event); err != nil || event.DeliveryStatus != "delivered" {
			t.Fatalf("duplicate reopened recovery: %#v / %v", event, err)
		}
	}
	var reconcile model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": "generation.reconcile:" + f.stepID}).Decode(&reconcile); err != nil || reconcile.AggregateID != f.creationID || reconcile.EventType != "generation.reconcile" {
		t.Fatalf("submission recovery overwritten: %#v / %v", reconcile, err)
	}
}

func TestMongoProviderInboxRecoveryConflictRollsBack(t *testing.T) {
	for _, collision := range []string{"event_type", "aggregate_id", "payload"} {
		t.Run(collision, func(t *testing.T) {
			f, _ := seedPreparedProviderIntent(t)
			// delivery 身份参与消费事件 ID 的派生：这里必须按步骤唯一，否则
			// 前一个子用例预埋的事件会让本子用例自己的 InsertOne 撞 E11000，
			// 断言还没跑到实现就红了。
			record, err := generation.NewProviderInboxRecord("test-b2b", "delivery-"+f.stepID, f.stepID, "job-1", "", []byte(`{}`), 1, f.now)
			if err != nil {
				t.Fatal(err)
			}
			// All collisions must reject ACK, not silently reuse another delivery's event.
			event := bson.M{"_id": inboxEventID(record), "event_type": "generation.inbox.consume", "aggregate_id": f.stepID, "payload": []byte(`{}`)}
			event[collision] = "wrong"
			if _, err := f.events.InsertOne(f.ctx, event); err != nil {
				t.Fatal(err)
			}
			store := NewGenerationProviderInboxRepository(f.data)
			err = f.runner.WithinTx(f.ctx, func(ctx context.Context) error { _, err := store.ApplyAndSchedule(ctx, record); return err })
			if err == nil {
				t.Fatal("recovery identity collision accepted")
			}
			count, err := f.database.Collection(schema.CollectionGenerationProviderInbox).CountDocuments(f.ctx, bson.M{"step_id": f.stepID})
			if err != nil || count != 0 {
				t.Fatalf("inbox escaped rollback: %d / %v", count, err)
			}
		})
	}
}

func TestMongoProviderInboxRejectsLifecycleAndRollsBack(t *testing.T) {
	f, _ := seedPreparedProviderIntent(t)
	store := NewGenerationProviderInboxRepository(f.data)
	record, err := generation.NewProviderInboxRecord("test-b2b", "delivery-"+f.stepID, f.stepID, "job-1", "", []byte(`{}`), 1, f.now)
	if err != nil {
		t.Fatal(err)
	}
	record.Status = generation.ProviderInboxQuarantined
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { _, err := store.ApplyAndSchedule(ctx, record); return err }); !errors.Is(err, generation.ErrInvalidProviderInboxRecord) {
		t.Fatalf("incoming quarantine accepted: %v", err)
	}
	record.Status = generation.ProviderInboxPending
	abort := errors.New("forced abort")
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		if _, err := store.ApplyAndSchedule(ctx, record); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	for _, collection := range []string{schema.CollectionGenerationProviderInbox, schema.CollectionOutboxEvents} {
		filter := bson.M{"step_id": f.stepID}
		if collection == schema.CollectionOutboxEvents {
			filter = bson.M{"_id": inboxEventID(record)}
		}
		count, err := f.database.Collection(collection).CountDocuments(f.ctx, filter)
		if err != nil || count != 0 {
			t.Fatalf("%s escaped rollback: %d / %v", collection, count, err)
		}
	}
	// No transaction means no ACK permission.
	if _, err := store.ApplyAndSchedule(f.ctx, record); err == nil {
		t.Fatal("missing transaction accepted")
	}
}

func TestMongoProviderInboxConflictPreservesTimeAndPayload(t *testing.T) {
	f, _ := seedPreparedProviderIntent(t)
	store := NewGenerationProviderInboxRepository(f.data)
	record, err := generation.NewProviderInboxRecord("test-b2b", "delivery-"+f.stepID, f.stepID, "job-1", "", []byte(`{"status":"completed"}`), 1, f.now)
	if err != nil {
		t.Fatal(err)
	}
	record.UpdatedAt = f.now.Add(time.Minute)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { _, err := store.ApplyAndSchedule(ctx, record); return err }); err != nil {
		t.Fatal(err)
	}
	incoming, err := generation.NewProviderInboxRecord("test-b2b", record.DeliveryID, f.stepID, "job-1", "", []byte(`{"status":"failed"}`), 1, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		result, err := store.ApplyAndSchedule(ctx, incoming)
		if result != generation.InboxApplyQuarantined {
			t.Errorf("result=%s", result)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stored model.ProviderInboxDocument
	if err := f.database.Collection(schema.CollectionGenerationProviderInbox).FindOne(f.ctx, bson.M{"step_id": f.stepID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.UpdatedAt.Equal(record.UpdatedAt) || string(stored.Payload) != string(record.Payload) || stored.Status != "quarantined" {
		t.Fatalf("conflict damaged original fact: %#v", stored)
	}
}

func TestMongoProviderInbox与恢复事件同事务幂等写入(t *testing.T) {
	fixture, _ := seedPreparedProviderIntent(t)
	store := NewGenerationProviderInboxRepository(fixture.data)
	payload := []byte(`{"jobId":"job-b2b-1","status":"completed"}`)
	record, err := generation.NewProviderInboxRecord("test-b2b", "delivery-"+fixture.stepID, fixture.stepID, "job-b2b-1", "", payload, 1, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		_, _ = fixture.database.Collection(schema.CollectionGenerationProviderInbox).DeleteMany(ctx, bson.D{{Key: "step_id", Value: fixture.stepID}})
		_, _ = fixture.database.Collection(schema.CollectionOutboxEvents).DeleteOne(ctx, bson.D{{Key: "_id", Value: generation.ProviderInboxRecoveryEventID(fixture.stepID)}})
	})

	result, err := fixture.runner.WithinTxResult(fixture.ctx, func(txCtx context.Context) (any, error) {
		got, applyErr := store.ApplyAndSchedule(txCtx, record)
		return got, applyErr
	})
	if err != nil {
		t.Fatalf("first inbox apply: %v", err)
	}
	if got, ok := result.(generation.ProviderInboxApplyResult); !ok || got != generation.InboxApplyInserted {
		t.Fatalf("first inbox result = %#v", result)
	}

	var inbox bson.M
	if err := fixture.database.Collection(schema.CollectionGenerationProviderInbox).FindOne(fixture.ctx, bson.D{{Key: "step_id", Value: fixture.stepID}}).Decode(&inbox); err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if inbox["source"] != generation.ProviderInboxSource || inbox["status"] != string(generation.ProviderInboxPending) {
		t.Fatalf("unexpected inbox document: %#v", inbox)
	}
	var event bson.M
	if err := fixture.events.FindOne(fixture.ctx, bson.D{{Key: "_id", Value: generation.ProviderInboxRecoveryEventID(fixture.stepID)}}).Decode(&event); err != nil {
		t.Fatalf("read recovery event: %v", err)
	}
	if event["event_type"] != "generation.reconcile" || event["delivery_status"] != "pending" {
		t.Fatalf("unexpected recovery event: %#v", event)
	}

	record.Attempts = 9
	result, err = fixture.runner.WithinTxResult(fixture.ctx, func(txCtx context.Context) (any, error) {
		got, applyErr := store.ApplyAndSchedule(txCtx, record)
		return got, applyErr
	})
	if got, ok := result.(generation.ProviderInboxApplyResult); err != nil || !ok || got != generation.InboxApplyNoop {
		t.Fatalf("replayed inbox = %#v, %v", result, err)
	}

	record.Payload = []byte(`{"jobId":"job-b2b-1","status":"failed"}`)
	sum := sha256.Sum256(record.Payload)
	record.PayloadDigest = hex.EncodeToString(sum[:])
	result, err = fixture.runner.WithinTxResult(fixture.ctx, func(txCtx context.Context) (any, error) {
		got, applyErr := store.ApplyAndSchedule(txCtx, record)
		return got, applyErr
	})
	if got, ok := result.(generation.ProviderInboxApplyResult); err != nil || !ok || got != generation.InboxApplyQuarantined {
		t.Fatalf("conflicting inbox = %#v, %v", result, err)
	}
	var quarantined bson.M
	if err := fixture.database.Collection(schema.CollectionGenerationProviderInbox).FindOne(fixture.ctx, bson.D{{Key: "step_id", Value: fixture.stepID}}).Decode(&quarantined); err != nil {
		t.Fatal(err)
	}
	if quarantined["status"] != string(generation.ProviderInboxQuarantined) {
		t.Fatalf("inbox status = %v, want quarantined", quarantined["status"])
	}
}

// WithinTxResult keeps the test focused on the transaction contract without
// exporting a production-specific generic transaction API.
func (runner *MongoTxRunner) WithinTxResult(ctx context.Context, fn func(context.Context) (any, error)) (any, error) {
	var result any
	err := runner.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		result, err = fn(txCtx)
		return err
	})
	return result, err
}
