package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/schema"
	b2b "ai-business-service/internal/integrations/polarstarb2b"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func providerIntentStore(t *testing.T, f *submissionMongoFixture) generation.ProviderSubmissionStore {
	t.Helper()
	s, ok := NewGenerationSubmissionRepository(f.data).(generation.ProviderSubmissionStore)
	if !ok {
		t.Fatal("submission repository does not persist a provider intent atomically")
	}
	return s
}

func TestProviderIntentReadDistinguishesUnpreparedFromCorrupt(t *testing.T) {
	for _, state := range []string{"ready", "missing-step", "submitting", "job-bound", "terminal", "quarantined", "local", "corrupt"} {
		t.Run(state, func(t *testing.T) {
			f, _ := seedProviderIntent(t)
			fields := bson.M{}
			switch state {
			case "missing-step":
				if _, err := f.steps.DeleteOne(f.ctx, bson.M{"_id": f.stepID}); err != nil {
					t.Fatal(err)
				}
			case "submitting":
				fields["submit_status"] = "submitting"
			case "job-bound":
				fields["external_execution_id"] = "job-" + f.stepID
			case "terminal":
				fields["terminal_status"] = "completed"
			case "quarantined":
				fields["terminal_quarantined"] = true
			case "local":
				fields["provider"] = "local_execution_v2"
			case "corrupt":
				fields["submission_intent"] = bson.M{"digest": "broken"}
			}
			if len(fields) > 0 {
				if _, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID}, bson.M{"$set": fields}); err != nil {
					t.Fatal(err)
				}
			}
			intent, err := providerIntentStore(t, f).ReadProviderSubmission(f.ctx, f.stepID)
			if state == "ready" {
				if intent != nil || err != nil {
					t.Fatalf("unprepared ready step: %#v / %v", intent, err)
				}
			} else if !errors.Is(err, generation.ErrSubmissionConflict) {
				t.Fatalf("unsafe absence accepted: %#v / %v", intent, err)
			}
		})
	}
}

func TestProviderPreparedIntentCanRecoverAndReconcile(t *testing.T) {
	f, c := seedProviderIntent(t)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return providerIntentStore(t, f).PrepareProviderSubmission(ctx, c) }); err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationSubmissionRepository(f.data)
	if record, err := repository.ClaimedSubmission(f.ctx, f.eventID); err != nil || record == nil {
		t.Fatalf("prepared intent cannot recover: %#v / %v", record, err)
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		return repository.MarkReconciling(ctx, generation.ReconcilingCommand{EventID: f.eventID, LeaseToken: f.leaseToken, At: f.now, NextAttemptAt: f.now.Add(time.Minute)})
	}); err != nil {
		t.Fatalf("unknown submitted result cannot reconcile: %v", err)
	}
}

func seedProviderIntent(t *testing.T) (*submissionMongoFixture, generation.PrepareProviderSubmissionCommand) {
	t.Helper()
	f := newSubmissionMongoFixture(t)
	f.seedDispatching()
	route := creations.ExecutionRoute{Provider: "polarstar_b2b_v2", AccountRef: "test-b2b", ContractVersion: "b2b.job.v2", MappingVersion: "mapping-1"}
	_, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID}, bson.M{"$set": bson.M{"provider": route.Provider, "account_ref": route.AccountRef, "contract_version": route.ContractVersion, "mapping_version": route.MappingVersion}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"externalId": f.stepID, "idempotencyKey": "cling-step:" + f.stepID, "capability": "text_to_image", "model": "ps-image-v1", "input": map[string]any{"prompt": "fixture"}, "callbackUrl": nil, "callbackPolicy": "disabled", "resultUrlPolicy": "permanent"})
	sum := sha256.Sum256(payload)
	c := generation.PrepareProviderSubmissionCommand{EventID: f.eventID, CreationID: f.creationID, LeaseToken: f.leaseToken, LeaseOwner: "worker-1", Fence: 1, At: f.now,
		Request: generation.FrozenProviderRequest{Route: route, StepID: f.stepID, Capability: "text_to_image", IdempotencyKey: "cling-step:" + f.stepID, Payload: payload, Digest: hex.EncodeToString(sum[:])}}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		// The normal fixture tracks only its submission event. Tests that bind a
		// job after cancellation also create this stable cancel event, which must
		// be cleaned by its own deterministic ID; otherwise a later untyped Claim
		// can consume it and make an unrelated backoff assertion flaky.
		for _, eventID := range []string{
			generation.ProviderInboxRecoveryEventID(f.stepID),
			generation.ProviderCancelEventID(f.stepID),
		} {
			if _, err := f.events.DeleteOne(ctx, bson.M{"_id": eventID}); err != nil {
				t.Error(err)
			}
		}
	})
	return f, c
}

func TestProviderIntentAtomicPreparationAndRecovery(t *testing.T) {
	f, c := seedProviderIntent(t)
	s := providerIntentStore(t, f)
	if err := s.PrepareProviderSubmission(f.ctx, c); err == nil {
		t.Fatal("preparation outside transaction allowed")
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return s.PrepareProviderSubmission(ctx, c) }); err != nil {
		t.Fatal(err)
	}
	// New repository instance simulates restart; recovery owns the frozen bytes.
	recovered, err := providerIntentStore(t, f).ReadProviderSubmission(f.ctx, f.stepID)
	if err != nil || recovered == nil {
		t.Fatalf("restore intent: %v", err)
	}
	if recovered.Request.Digest != c.Request.Digest || string(recovered.Request.Payload) != string(c.Request.Payload) || recovered.Request.Route != c.Request.Route || recovered.Fence != 1 {
		t.Fatal("frozen identity changed")
	}
	route := recovered.Request.Route
	request, err := b2b.RestoreRequest(b2b.Route{StepID: f.stepID, Provider: route.Provider, AccountRef: route.AccountRef, ContractVersion: route.ContractVersion, MappingVersion: route.MappingVersion}, recovered.Request.Payload, recovered.Request.Digest)
	if err != nil || request.Digest() != c.Request.Digest {
		t.Fatalf("persisted request cannot be restored: %v", err)
	}
	var ev bson.M
	if err := f.events.FindOne(f.ctx, bson.M{"_id": "generation.reconcile:" + f.stepID}).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	if ev["event_type"] != "generation.reconcile" || ev["delivery_status"] != "pending" {
		t.Fatal("missing durable recovery event")
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return s.PrepareProviderSubmission(ctx, c) }); !errors.Is(err, generation.ErrSubmissionConflict) {
		t.Fatalf("second permission: %v", err)
	}
	count, err := f.events.CountDocuments(f.ctx, bson.M{"_id": "generation.reconcile:" + f.stepID})
	if err != nil || count != 1 {
		t.Fatalf("recovery count %d/%v", count, err)
	}
	// No in-memory notification is needed: after the original submit lease,
	// another process can claim the durable reconcile event by its stable ID.
	queue := NewOutboxRepository(f.data)
	eventID := "generation.reconcile:" + f.stepID
	early, err := queue.ClaimByIDAndType(f.ctx, "recovery-worker", eventID, outbox.EventType("generation.reconcile"), f.now, f.now.Add(time.Minute))
	if err != nil || early != nil {
		t.Fatalf("early recovery claim: %v", err)
	}
	claimed, err := queue.ClaimByIDAndType(f.ctx, "recovery-worker", eventID, outbox.EventType("generation.reconcile"), f.now.Add(time.Minute), f.now.Add(2*time.Minute))
	if err != nil || claimed == nil || claimed.ID != eventID {
		t.Fatalf("durable recovery not claimable: %v", err)
	}
}

func TestProviderIntentRejectsStaleOrChangedIdentity(t *testing.T) {
	for _, name := range []string{"token", "owner", "fence", "expired", "account", "capability", "digest", "key", "blocked", "legacy", "refunded", "publication-blocked"} {
		t.Run(name, func(t *testing.T) {
			f, c := seedProviderIntent(t)
			s := providerIntentStore(t, f)
			switch name {
			case "token":
				c.LeaseToken = "other"
			case "owner":
				c.LeaseOwner = "other"
			case "fence":
				c.Fence = 2
			case "expired":
				c.At = f.now.Add(time.Minute)
			case "account":
				c.Request.Route.AccountRef = "other"
			case "capability":
				c.Request.Capability = "image_to_video"
			case "digest":
				c.Request.Digest = "wrong"
			case "key":
				c.Request.IdempotencyKey = "other"
			case "blocked":
				f.setStepStatus(creations.StepSubmitStatusBlocked)
			case "refunded", "publication-blocked":
				field, value := "status", "reversed"
				if name == "publication-blocked" {
					field, value = "publication_state", "blocked"
				}
				_, err := f.database.Collection(schema.CollectionReservations).UpdateOne(f.ctx, bson.M{"creation_id": f.creationID}, bson.M{"$set": bson.M{field: value}})
				if err != nil {
					t.Fatal(err)
				}
			case "legacy":
				_, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID}, bson.M{"$unset": bson.M{"provider": "", "account_ref": "", "contract_version": "", "mapping_version": ""}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return s.PrepareProviderSubmission(ctx, c) }); err == nil {
				t.Fatal("unsafe intent accepted")
			}
			count, err := f.events.CountDocuments(f.ctx, bson.M{"_id": "generation.reconcile:" + f.stepID})
			if err != nil || count != 0 {
				t.Fatal("rejected intent produced recovery event")
			}
		})
	}
}

func TestProviderIntentRecoveryConflictRollsBackAllWrites(t *testing.T) {
	f, c := seedProviderIntent(t)
	s := providerIntentStore(t, f)
	_, err := f.events.InsertOne(f.ctx, bson.M{"_id": "generation.reconcile:" + f.stepID, "event_type": "wrong", "aggregate_id": "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return s.PrepareProviderSubmission(ctx, c) }); err == nil {
		t.Fatal("conflicting durable event accepted")
	}
	var step bson.M
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if step["submission_intent"] != nil || step["submit_status"] != string(creations.StepSubmitStatusReady) {
		t.Fatal("partial step write escaped transaction")
	}
	var event bson.M
	if err := f.events.FindOne(f.ctx, bson.M{"_id": f.eventID}).Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event["delivery_status"] != string(outbox.DeliveryStatusDispatching) || event["request_digest"] != nil {
		t.Fatal("partial submission write escaped transaction")
	}
	var reservation bson.M
	if err := f.database.Collection(schema.CollectionReservations).FindOne(f.ctx, bson.M{"creation_id": f.creationID}).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation["publication_version"] != nil {
		t.Fatal("partial publication gate write escaped transaction")
	}
}

func TestProviderIntentConcurrentPreparationHasOneGrant(t *testing.T) {
	f, c := seedProviderIntent(t)
	s := providerIntentStore(t, f)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := newMongoTestContext()
			defer cancel()
			<-start
			results <- f.runner.WithinTx(ctx, func(ctx context.Context) error { return s.PrepareProviderSubmission(ctx, c) })
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, generation.ErrSubmissionConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("grants=%d, want 1", success)
	}
	var reservation bson.M
	if err := f.database.Collection(schema.CollectionReservations).FindOne(f.ctx, bson.M{"creation_id": f.creationID}).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation["publication_version"] != int64(1) {
		t.Fatalf("gate version = %v", reservation["publication_version"])
	}
}

func providerReauthorizationStore(t *testing.T, f *submissionMongoFixture) generation.ProviderReauthorizationStore {
	t.Helper()
	s, ok := NewGenerationSubmissionRepository(f.data).(generation.ProviderReauthorizationStore)
	if !ok {
		t.Fatal("submission repository does not grant provider reauthorization")
	}
	return s
}

// reauthorizationCommand 复用 seedProviderIntent 已经建立的那套租约身份，
// 使每个子用例只需要改一处就能表达「这一项对不上」。
func reauthorizationCommand(f *submissionMongoFixture) generation.ReauthorizeProviderSubmissionCommand {
	return generation.ReauthorizeProviderSubmissionCommand{
		EventID: f.eventID, CreationID: f.creationID, StepID: f.stepID,
		LeaseToken: f.leaseToken, LeaseOwner: "worker-1", Fence: 1, At: f.now,
		Reason: generation.ReauthorizationReasonLookupNotFound,
	}
}

func seedPreparedProviderIntent(t *testing.T) (*submissionMongoFixture, generation.PrepareProviderSubmissionCommand) {
	t.Helper()
	f, c := seedProviderIntent(t)
	s := providerIntentStore(t, f)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return s.PrepareProviderSubmission(ctx, c) }); err != nil {
		t.Fatal(err)
	}
	return f, c
}

// providerIntentAudit 只读取断言需要的那一小块。
//
// 刻意用类型化结构而不是 map 断言：驱动对嵌套文档的默认解码类型不是 bson.M，
// 断言失败时 ok 为 false，检查会被**静默跳过** —— 那是典型的假绿。
type providerIntentAudit struct {
	Intent *struct {
		Reauthorization *struct {
			Reason string `bson:"reason"`
			Fence  int32  `bson:"fence"`
		} `bson:"reauthorization"`
	} `bson:"submission_intent"`
}

func readProviderIntentAudit(t *testing.T, f *submissionMongoFixture) *providerIntentAudit {
	t.Helper()
	var doc providerIntentAudit
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Intent == nil {
		t.Fatal("提交意图不存在：重新授权既不该新建也不该删除它")
	}
	return &doc
}

// 重新授权必须与提交准备一样是「原子且只有一次」的。
//
// 它是一条刻意打破「授权只发一次」的路径，因此边界要比常规路径更窄：
// 没有事务就拒绝、第二次必须返回哨兵、冻结字节一律不得被改写。
func TestProviderReauthorizationHasExactlyOneGrant(t *testing.T) {
	f, c := seedPreparedProviderIntent(t)
	store := providerReauthorizationStore(t, f)
	command := reauthorizationCommand(f)

	// 没有事务就没有租约护栏：额度可能被任何读到这条记录的进程花掉。
	if err := store.ReauthorizeProviderSubmission(f.ctx, command); err == nil {
		t.Fatal("reauthorization outside transaction allowed")
	}
	// 事务外被拒绝的调用不得留下审计痕迹。
	if audit := readProviderIntentAudit(t, f); audit.Intent.Reauthorization != nil {
		t.Fatal("事务外被拒绝的调用仍写下了审计")
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return store.ReauthorizeProviderSubmission(ctx, command) }); err != nil {
		t.Fatal(err)
	}
	// 审计事实必须真的落进 submission_intent，而不是只回了个 nil。
	audit := readProviderIntentAudit(t, f)
	if audit.Intent.Reauthorization == nil || audit.Intent.Reauthorization.Reason != generation.ReauthorizationReasonLookupNotFound {
		t.Fatalf("重新授权审计未落库: %#v", audit.Intent.Reauthorization)
	}
	if audit.Intent.Reauthorization.Fence != command.Fence {
		t.Fatalf("审计围栏 = %d, want %d", audit.Intent.Reauthorization.Fence, command.Fence)
	}
	// 额度只有一次：第二次必须返回哨兵，而不是再发一次。
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return store.ReauthorizeProviderSubmission(ctx, command) }); !errors.Is(err, generation.ErrReauthorizationExhausted) {
		t.Fatalf("第二次重新授权 = %v, want ErrReauthorizationExhausted", err)
	}
	// 冻结字节是重发的唯一真相源，绝不能被这次授权改写。
	recovered, err := providerIntentStore(t, f).ReadProviderSubmission(f.ctx, f.stepID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Request.Digest != c.Request.Digest || string(recovered.Request.Payload) != string(c.Request.Payload) {
		t.Fatal("重新授权改写了冻结字节")
	}
}

// 重新授权必须拒绝过期租约与已经绑定过外部任务号的步骤：
// 前者防止过期工作者花掉额度，后者防止对已经受理的任务再发一次。
func TestProviderReauthorizationRejectsStaleLeaseAndBoundJob(t *testing.T) {
	for _, name := range []string{"token", "owner", "fence", "expired", "bound"} {
		t.Run(name, func(t *testing.T) {
			f, _ := seedPreparedProviderIntent(t)
			command := reauthorizationCommand(f)
			switch name {
			case "token":
				command.LeaseToken = "other"
			case "owner":
				command.LeaseOwner = "other"
			case "fence":
				command.Fence = 2
			case "expired":
				command.At = f.now.Add(time.Minute)
			case "bound":
				if _, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID},
					bson.M{"$set": bson.M{"external_execution_id": "job-already-bound"}}); err != nil {
					t.Fatal(err)
				}
			}
			store := providerReauthorizationStore(t, f)
			err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error { return store.ReauthorizeProviderSubmission(ctx, command) })
			if err == nil {
				t.Fatal("身份错配或已绑定的步骤仍拿到了重新授权")
			}
			// 已绑定与额度用完在存储层归一到同一个哨兵：两者都必须回到对账、
			// 绝不重发，因此调用方不需要、也不应该区分它们。
			if name == "bound" && !errors.Is(err, generation.ErrReauthorizationExhausted) {
				t.Fatalf("已绑定的返回 = %v, want ErrReauthorizationExhausted", err)
			}
			// 被拒绝的调用不得留下任何审计痕迹，否则额度会被误判成已用完。
			if audit := readProviderIntentAudit(t, f); audit.Intent.Reauthorization != nil {
				t.Fatalf("被拒绝的调用仍写下了审计: %#v", audit.Intent.Reauthorization)
			}
		})
	}
}

// 并发重新授权只能有一个成功：额度是「至多一次」，不是「每个工作者一次」。
func TestProviderReauthorizationRejectsClosedPublication(t *testing.T) {
	for _, state := range []string{"refunded", "blocked", "terminal", "quarantined", "invalid-route", "corrupt-intent"} {
		t.Run(state, func(t *testing.T) {
			f, _ := seedPreparedProviderIntent(t)
			collection, filter, fields := f.steps, bson.M{"_id": f.stepID}, bson.M{}
			switch state {
			case "refunded", "blocked":
				collection = f.database.Collection(schema.CollectionReservations)
				filter = bson.M{"creation_id": f.creationID}
				if state == "refunded" {
					fields["status"] = "reversed"
				} else {
					fields["publication_state"] = "blocked"
				}
			case "terminal":
				fields["terminal_version"] = int64(1)
				fields["terminal_status"] = "completed"
			case "quarantined":
				fields["terminal_quarantined"] = true
			case "invalid-route":
				fields["provider"] = "local_execution_v2"
			case "corrupt-intent":
				fields["submission_intent.digest"] = "corrupt"
			}
			if _, err := collection.UpdateOne(f.ctx, filter, bson.M{"$set": fields}); err != nil {
				t.Fatal(err)
			}
			store := providerReauthorizationStore(t, f)
			err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
				return store.ReauthorizeProviderSubmission(ctx, reauthorizationCommand(f))
			})
			if err == nil {
				t.Fatal("closed publication received submission permission")
			}
			if audit := readProviderIntentAudit(t, f); audit.Intent.Reauthorization != nil {
				t.Fatal("rejected authorization persisted audit")
			}
			var event bson.M
			if err := f.events.FindOne(f.ctx, bson.M{"_id": f.eventID}).Decode(&event); err != nil {
				t.Fatal(err)
			}
			if _, exists := event["reauthorization_version"]; exists {
				t.Fatal("rejected transaction retained lease write")
			}
		})
	}
}

func TestProviderReauthorizationWritesLeaseAndPublicationFence(t *testing.T) {
	f, _ := seedPreparedProviderIntent(t)
	store := providerReauthorizationStore(t, f)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		return store.ReauthorizeProviderSubmission(ctx, reauthorizationCommand(f))
	}); err != nil {
		t.Fatal(err)
	}
	count, err := f.events.CountDocuments(f.ctx, bson.M{"_id": f.eventID, "reauthorization_version": int64(1), "lease_token": f.leaseToken, "attempt_count": 1})
	if err != nil || count != 1 {
		t.Fatalf("lease fence missing: %d / %v", count, err)
	}
	count, err = f.database.Collection(schema.CollectionReservations).CountDocuments(f.ctx, bson.M{"creation_id": f.creationID, "publication_version": int64(2), "publication_state": "open", "status": "reserved"})
	if err != nil || count != 1 {
		t.Fatalf("publication fence missing: %d / %v", count, err)
	}
}

func TestProviderReauthorizationConcurrentHasOneGrant(t *testing.T) {
	f, _ := seedPreparedProviderIntent(t)
	store := providerReauthorizationStore(t, f)
	command := reauthorizationCommand(f)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := newMongoTestContext()
			defer cancel()
			<-start
			results <- f.runner.WithinTx(ctx, func(ctx context.Context) error { return store.ReauthorizeProviderSubmission(ctx, command) })
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		// 并发失败既可能是条件写未命中（哨兵），也可能是事务写冲突。
		// 两者都不允许再发一次，因此只要求「不是别的错误」。
		if !errors.Is(err, generation.ErrReauthorizationExhausted) && !errors.Is(err, generation.ErrSubmissionConflict) {
			t.Fatalf("并发重新授权 = %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("grants=%d, want 1", success)
	}
}
