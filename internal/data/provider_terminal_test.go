package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func seedProviderTerminal(t *testing.T) (*submissionMongoFixture, generation.ProviderTerminalFact) {
	t.Helper()
	f, command := seedProviderIntent(t)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		return providerIntentStore(t, f).PrepareProviderSubmission(ctx, command)
	}); err != nil {
		t.Fatal(err)
	}
	jobID := "job-" + f.stepID
	if _, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID}, bson.M{"$set": bson.M{"external_execution_id": jobID, "submit_status": "submitted"}}); err != nil {
		t.Fatal(err)
	}
	fact := generation.ProviderTerminalFact{
		Provider: command.Request.Route.Provider, AccountRef: command.Request.Route.AccountRef,
		StepID: f.stepID, ExternalExecutionID: jobID, Capability: command.Request.Capability,
		Status: generation.ProviderTerminalCompleted, ResultRef: "https://results.example.test/" + f.stepID + ".png", AttemptFence: int64(command.Fence),
		PayloadDigest: strings.Repeat("b", 64), ObservedAt: f.now,
	}
	fact.TerminalDigest = generation.ProviderTerminalSummaryDigest(fact.Status, fact.ResultRef)
	return f, fact
}

func applyProviderTerminalTx(f *submissionMongoFixture, fact generation.ProviderTerminalFact) (generation.ProviderTerminalApplyResult, error) {
	var result generation.ProviderTerminalApplyResult
	err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		var err error
		result, err = NewGenerationCallbackRepository(f.data).ApplyProviderTerminal(ctx, fact)
		return err
	})
	return result, err
}

func TestProviderTerminalMongoSemanticReplayAndConflictPersistence(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("first apply: %s / %v", result, err)
	}
	replay := fact
	replay.ResultRef = fact.ResultRef + "?signature=rotated&expires=999"
	replay.TerminalDigest = generation.ProviderTerminalSummaryDigest(replay.Status, replay.ResultRef)
	replay.PayloadDigest = strings.Repeat("c", 64)
	if result, err := applyProviderTerminalTx(f, replay); err != nil || result != generation.ProviderTerminalNoop {
		t.Fatalf("raw transport digest changed semantic replay: %s / %v", result, err)
	}
	businessQuery := fact
	businessQuery.ResultRef = fact.ResultRef + "?token=business-variant"
	businessQuery.TerminalDigest = generation.ProviderTerminalSummaryDigest(businessQuery.Status, businessQuery.ResultRef)
	if result, err := applyProviderTerminalTx(f, businessQuery); err != nil || result != generation.ProviderTerminalQuarantined {
		t.Fatalf("business query change must persist quarantine: %s / %v", result, err)
	}
	conflict := fact
	conflict.Status = generation.ProviderTerminalFailed
	conflict.ResultRef = ""
	conflict.TerminalDigest = generation.ProviderTerminalSummaryDigest(conflict.Status, conflict.ResultRef)
	if result, err := applyProviderTerminalTx(f, conflict); err != nil || result != generation.ProviderTerminalQuarantined {
		t.Fatalf("conflict must commit quarantine with nil transaction error: %s / %v", result, err)
	}
	var step bson.M
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if step["terminal_version"] != int64(1) || step["terminal_status"] != "completed" || step["terminal_digest"] != fact.TerminalDigest || step["terminal_payload_digest"] != fact.PayloadDigest || step["terminal_quarantined"] != true || step["terminal_quarantine_reason"] != generation.ErrProviderTerminalConflict.Error() {
		t.Fatalf("winner changed or conflict evidence rolled back: %#v", step)
	}
	if result, err := applyProviderTerminalTx(f, replay); err != nil || result != generation.ProviderTerminalQuarantined {
		t.Fatalf("quarantine must prevent automatic resume: %s / %v", result, err)
	}
}

func TestProviderTerminalCompleted在同一事务调度稳定素材事件(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	fact.ResultRef = "https://results.example.test/output.png"
	fact.TerminalDigest = generation.ProviderTerminalSummaryDigest(fact.Status, fact.ResultRef)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("first apply: %s / %v", result, err)
	}

	var event model.OutboxEventDocument
	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&event); err != nil {
		t.Fatalf("read materialization event: %v", err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatalf("parse materialization payload: %v", err)
	}
	if event.AggregateID != f.creationID || event.EventType != generation.ProviderResultMaterializeEventType || event.DeliveryStatus != string(outbox.DeliveryStatusPending) ||
		payload.CreationID != f.creationID || payload.StepID != fact.StepID || payload.Provider != fact.Provider || payload.AccountRef != fact.AccountRef ||
		payload.JobID != fact.ExternalExecutionID || payload.ResultRef != fact.ResultRef || payload.TerminalVersion != 1 || payload.TerminalDigest != fact.TerminalDigest {
		t.Fatalf("materialization event = %#v / payload=%#v", event, payload)
	}

	replay := fact
	replay.PayloadDigest = strings.Repeat("c", 64)
	if result, err := applyProviderTerminalTx(f, replay); err != nil || result != generation.ProviderTerminalNoop {
		t.Fatalf("semantic replay: %s / %v", result, err)
	}
	var count int64
	if count, err = f.events.CountDocuments(f.ctx, bson.M{"_id": eventID}); err != nil || count != 1 {
		t.Fatalf("materialization event count = %d / %v, want 1", count, err)
	}
}

// 失败或取消的 B2B 终态不能只停止对账：预留仍然处于 reserved 就会让
// 用户额度永久冻结。终态 CAS 只负责持久化供应商事实；它在同一事务创建
// 稳定结算事件，由独立 Worker 在后续事务里把账本、步骤、创作和事件一起
// 收敛。这样既不在 callback 路径里做账本 I/O，也没有“已失败但未退款”的
// 崩溃窗口。
func TestProviderTerminalFailureMongo原子结算冲正账本与状态(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	fact.Status = generation.ProviderTerminalFailed
	fact.ResultRef = ""
	fact.TerminalDigest = generation.ProviderTerminalSummaryDigest(fact.Status, "")
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply failed terminal: %s / %v", result, err)
	}

	eventID := generation.ProviderTerminalSettlementEventID(fact.StepID)
	var event model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&event); err != nil {
		t.Fatalf("read terminal settlement event: %v", err)
	}
	payload, err := generation.ParseProviderTerminalSettlementEventPayload(event.Payload)
	if err != nil {
		t.Fatalf("parse terminal settlement event: %v", err)
	}
	if event.AggregateID != f.creationID || event.EventType != generation.ProviderTerminalSettlementEventType ||
		event.DeliveryStatus != string(outbox.DeliveryStatusPending) || payload.CreationID != f.creationID ||
		payload.StepID != fact.StepID || payload.Provider != fact.Provider || payload.AccountRef != fact.AccountRef ||
		payload.JobID != fact.ExternalExecutionID || payload.Capability != fact.Capability || payload.Status != generation.ProviderTerminalFailed ||
		payload.TerminalVersion != 1 || payload.TerminalDigest != fact.TerminalDigest {
		t.Fatalf("terminal settlement event = %#v / payload=%#v", event, payload)
	}

	queue := NewOutboxRepository(f.data)
	claimed, err := queue.ClaimByIDAndType(f.ctx, "terminal-settler", eventID, outbox.EventType(generation.ProviderTerminalSettlementEventType), f.now, f.now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("claim terminal settlement: %#v / %v", claimed, err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderTerminalSettlement(f.ctx, payload)
	if err != nil {
		t.Fatalf("load terminal settlement target: %v", err)
	}
	ledgerUsecase := ledger.NewUsecaseWithClock(NewLedgerRepository(f.data), f.runner, func() time.Time { return f.now })
	err = f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		if _, err := ledgerUsecase.ReverseInTx(tx, payload.CreationID, ledger.ReversalReasonGenerationFailed, f.now); err != nil {
			return err
		}
		return repository.SettleProviderTerminal(tx, generation.ProviderTerminalSettlement{
			EventID: eventID, LeaseToken: claimed.LeaseToken, Target: target, SettledAt: f.now,
		})
	})
	if err != nil {
		t.Fatalf("settle terminal failure: %v", err)
	}

	var step model.CreationStepDocument
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusGenerationFailed) {
		t.Fatalf("step status = %q, want generation_failed", step.SubmitStatus)
	}
	var creation model.CreationDocument
	if err := f.database.Collection(schema.CollectionCreations).FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusGenerationFailed) {
		t.Fatalf("creation status = %q, want generation_failed", creation.Status)
	}
	var reservation model.ReservationDocument
	if err := f.database.Collection(schema.CollectionReservations).FindOne(f.ctx, bson.M{"creation_id": f.creationID}).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.Status != string(ledger.ReservationStatusReversed) || reservation.PublicationState != "reversed" || reservation.PublicationVersion != 2 {
		t.Fatalf("settled reservation = %#v", reservation)
	}
	var delivered model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || delivered.LeaseToken != "" || delivered.LeaseOwner != "" || !delivered.LeaseUntil.IsZero() {
		t.Fatalf("settlement event not delivered: %#v", delivered)
	}
	assetCount, err := f.database.Collection(schema.CollectionAssets).CountDocuments(f.ctx, bson.M{"owner_id": f.stepID})
	if err != nil || assetCount != 0 {
		t.Fatalf("failure must not create assets: %d / %v", assetCount, err)
	}
	entryCount, err := f.database.Collection(schema.CollectionLedgerEntries).CountDocuments(f.ctx, bson.M{"creation_id": f.creationID})
	if err != nil || entryCount != 2 {
		t.Fatalf("ledger entries = %d / %v, want reserve + one reverse", entryCount, err)
	}
}

func TestProviderTerminalCancelling创作仍由供应商失败终态唯一结算(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	fact.Status = generation.ProviderTerminalCancelled
	fact.ResultRef = ""
	fact.TerminalDigest = generation.ProviderTerminalSummaryDigest(fact.Status, "")
	if _, err := f.creations.UpdateOne(f.ctx, bson.M{"_id": f.creationID}, bson.M{"$set": bson.M{"status": string(creations.CreationStatusCancelling)}}); err != nil {
		t.Fatalf("mark fixture cancelling: %v", err)
	}
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply cancelled terminal: %s / %v", result, err)
	}

	eventID := generation.ProviderTerminalSettlementEventID(fact.StepID)
	claimed, err := NewOutboxRepository(f.data).ClaimByIDAndType(f.ctx, "terminal-settler", eventID, outbox.EventType(generation.ProviderTerminalSettlementEventType), f.now, f.now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("claim settlement: %#v / %v", claimed, err)
	}
	payload, err := generation.ParseProviderTerminalSettlementEventPayload(claimed.Payload)
	if err != nil {
		t.Fatalf("parse settlement payload: %v", err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderTerminalSettlement(f.ctx, payload)
	if err != nil {
		t.Fatalf("LoadProviderTerminalSettlement() error = %v", err)
	}
	ledgerUsecase := ledger.NewUsecaseWithClock(NewLedgerRepository(f.data), f.runner, func() time.Time { return f.now })
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		if _, err := ledgerUsecase.ReverseInTx(ctx, payload.CreationID, ledger.ReversalReasonGenerationFailed, f.now); err != nil {
			return err
		}
		return repository.SettleProviderTerminal(ctx, generation.ProviderTerminalSettlement{EventID: eventID, LeaseToken: claimed.LeaseToken, Target: target, SettledAt: f.now})
	}); err != nil {
		t.Fatalf("SettleProviderTerminal() error = %v", err)
	}
	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusGenerationFailed) {
		t.Fatalf("creation status = %q, want generation_failed", creation.Status)
	}
}

func TestProviderTerminalCancelling创作仍由供应商完成终态发布(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	if _, err := f.creations.UpdateOne(f.ctx, bson.M{"_id": f.creationID}, bson.M{"$set": bson.M{"status": string(creations.CreationStatusCancelling)}}); err != nil {
		t.Fatalf("mark fixture cancelling: %v", err)
	}
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply completed terminal: %s / %v", result, err)
	}
	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	claimed, err := NewOutboxRepository(f.data).ClaimByIDAndType(f.ctx, "result-materializer", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("claim materialization: %#v / %v", claimed, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(claimed.Payload)
	if err != nil {
		t.Fatalf("parse materialization payload: %v", err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil {
		t.Fatalf("LoadProviderResultMaterialization() error = %v", err)
	}
	contentSHA256 := strings.Repeat("a", 64)
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		return repository.PublishProviderResultMaterialization(ctx, generation.ProviderResultPublication{
			EventID: claimed.ID, LeaseToken: claimed.LeaseToken, Target: target,
			StorageURL: "https://media.example.test/results/" + contentSHA256 + ".png", ContentType: "image/png", ContentLength: 42,
			ContentSHA256: contentSHA256, PublishedAt: f.now,
		})
	}); err != nil {
		t.Fatalf("PublishProviderResultMaterialization() error = %v", err)
	}
	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusSucceeded) {
		t.Fatalf("creation status = %q, want succeeded", creation.Status)
	}
}

// 供应商 completed 只创建素材工作项，不得直接对用户宣称作品可用。
// 素材已安全落入 R2 后，发布资产、步骤、创作、预留 publication gate 和
// 对应工作项必须在同一 Mongo 事务收敛；否则任一崩溃窗口都可能出现退款后作品
// 或用户看得到没有落盘的供应商 URL。
func TestProviderResultMaterializationMongo原子发布稳定资产与账本Gate(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply terminal: %s / %v", result, err)
	}

	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	queue := NewOutboxRepository(f.data)
	event, err := queue.ClaimByIDAndType(f.ctx, "result-materializer", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim materialization event: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatalf("parse materialization payload: %v", err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil {
		t.Fatalf("load materialization target: %v", err)
	}
	if target.Payload != payload || target.Sequence != 1 {
		t.Fatalf("materialization target = %#v, want frozen first-step payload", target)
	}
	// 模拟素材 Worker 已因 R2 故障耗尽自动预算，随后经受审计重驱成功的状态。
	// 发布必须在同一事务清掉该补传标记，否则作品已可用却长期显示待补传。
	if result, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID}, bson.M{"$set": bson.M{
		"r2_upload_pending": true, "r2_upload_pending_at": f.now, "r2_upload_pending_reason": "material_upload_budget_exhausted",
	}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("seed r2 upload pending: %v", err)
	}

	contentSHA256 := strings.Repeat("d", 64)
	storageURL := "https://media.example.test/results/tenant/" + contentSHA256 + ".png"
	err = f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		return repository.PublishProviderResultMaterialization(tx, generation.ProviderResultPublication{
			EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
			StorageURL: storageURL, ContentType: "image/png", ContentLength: 42,
			ContentSHA256: contentSHA256, PublishedAt: f.now,
		})
	})
	if err != nil {
		t.Fatalf("publish materialization: %v", err)
	}

	var asset model.AssetDocument
	if err := f.database.Collection(schema.CollectionAssets).FindOne(f.ctx, bson.M{"_id": "asset:" + f.stepID + ":result"}).Decode(&asset); err != nil {
		t.Fatalf("read published asset: %v", err)
	}
	if asset.OwnerType != "creation_step" || asset.OwnerID != f.stepID || asset.AssetKind != "result" || asset.StorageKey != storageURL || asset.Status != "available" || asset.ContentType != "image/png" || asset.ByteSize != 42 || asset.ContentSHA256 != contentSHA256 {
		t.Fatalf("published asset = %#v", asset)
	}
	var step model.CreationStepDocument
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
		t.Fatalf("read published step: %v", err)
	}
	if step.SubmitStatus != string(creations.StepSubmitStatusSucceeded) || step.R2UploadPending ||
		!step.R2UploadPendingAt.IsZero() || step.R2UploadPendingReason != "" {
		t.Fatalf("published step must clear r2 pending state: %#v", step)
	}
	var creation model.CreationDocument
	if err := f.database.Collection(schema.CollectionCreations).FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatalf("read published creation: %v", err)
	}
	if creation.Status != string(creations.CreationStatusSucceeded) {
		t.Fatalf("creation status = %q, want succeeded", creation.Status)
	}
	var reservation model.ReservationDocument
	if err := f.database.Collection(schema.CollectionReservations).FindOne(f.ctx, bson.M{"creation_id": f.creationID}).Decode(&reservation); err != nil {
		t.Fatalf("read publication gate: %v", err)
	}
	if reservation.Status != "reserved" || reservation.PublicationState != "published" || reservation.PublicationVersion != 2 {
		t.Fatalf("reservation publication gate = %#v", reservation)
	}
	var delivered model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&delivered); err != nil {
		t.Fatalf("read delivered materialization event: %v", err)
	}
	if delivered.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || delivered.LeaseToken != "" || delivered.LeaseOwner != "" || !delivered.LeaseUntil.IsZero() {
		t.Fatalf("materialization event not atomically delivered: %#v", delivered)
	}
}

// Two materializers may have read the same event before either enters the
// publication transaction (for example after a worker hand-off).  The event
// lease is the ownership hint, while the reservation publication gate and
// unique result asset are the final fence: one transaction may publish, and
// the other must observe a conflict without exposing a second asset.
func TestProviderResultMaterializationMongo同一事件同一Fence并发发布最多一个资产(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply terminal: %s / %v", result, err)
	}

	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	queue := NewOutboxRepository(f.data)
	event, err := queue.ClaimByIDAndType(f.ctx, "result-materializer", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim materialization event: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	publication := generation.ProviderResultPublication{
		EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
		StorageURL: "https://media.example.test/results/concurrent.png", ContentType: "image/png",
		ContentLength: 42, ContentSHA256: strings.Repeat("c", 64), PublishedAt: f.now,
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- f.runner.WithinTx(f.ctx, func(tx context.Context) error {
				return repository.PublishProviderResultMaterialization(tx, publication)
			})
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded, conflicted := 0, 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, generation.ErrProviderResultPublicationConflict) {
			conflicted++
		} else {
			t.Fatalf("concurrent publication error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent publication outcomes: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	if count, err := f.database.Collection(schema.CollectionAssets).CountDocuments(f.ctx, bson.M{"_id": "asset:" + f.stepID + ":result"}); err != nil || count != 1 {
		t.Fatalf("visible result assets = %d / %v, want exactly one", count, err)
	}
	var settled model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&settled); err != nil {
		t.Fatal(err)
	}
	if settled.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || settled.LeaseToken != "" {
		t.Fatalf("event settlement = %#v, want one delivered winner", settled)
	}
}

// An immutable R2 object may already exist when the Mongo publication
// transaction aborts.  The event must remain retryable; replaying the same
// publication must converge to one intermediate asset and one second-step
// submission event, never a duplicate generation.
func TestProviderResultMaterializationMongo发布事务失败后重试不重复资产或生成(t *testing.T) {
	f, fact, secondID, deferred := seedTwoStepProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply first terminal: %s / %v", result, err)
	}
	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	queue := NewOutboxRepository(f.data)
	event, err := queue.ClaimByIDAndType(f.ctx, "result-materializer", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim materialization event: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil || target.NextB2B == nil {
		t.Fatalf("load two-step target: %#v / %v", target, err)
	}
	storageURL := "https://media.example.test/results/immutable-first-frame.png"
	recipe, err := deferred.BindOwnedOpeningFrame(storageURL)
	if err != nil {
		t.Fatal(err)
	}
	publication := generation.ProviderResultPublication{
		EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
		StorageURL: storageURL, ContentType: "image/png", ContentLength: 42,
		ContentSHA256: strings.Repeat("b", 64), PublishedAt: f.now,
		NextB2B: &generation.ProviderB2BSecondStepActivation{StepID: secondID, Route: target.NextB2B.Route, Recipe: recipe},
	}
	transactionErr := errors.New("simulated Mongo publication failure")
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		if err := repository.PublishProviderResultMaterialization(tx, publication); err != nil {
			return err
		}
		return transactionErr
	}); !errors.Is(err, transactionErr) {
		t.Fatalf("aborted publication error = %v, want injected failure", err)
	}
	if count, err := f.database.Collection(schema.CollectionAssets).CountDocuments(f.ctx, bson.M{"_id": "asset:" + f.stepID + ":intermediate_result"}); err != nil || count != 0 {
		t.Fatalf("aborted publication left intermediate asset: count=%d err=%v", count, err)
	}
	if count, err := f.events.CountDocuments(f.ctx, bson.M{"_id": outbox.SubmissionEventID(secondID)}); err != nil || count != 0 {
		t.Fatalf("aborted publication left second submission: count=%d err=%v", count, err)
	}
	var inFlight model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&inFlight); err != nil {
		t.Fatal(err)
	}
	if inFlight.DeliveryStatus != string(outbox.DeliveryStatusDispatching) || inFlight.LeaseToken != event.LeaseToken {
		t.Fatalf("aborted event = %#v, want same retryable lease", inFlight)
	}

	if err := queue.Requeue(f.ctx, event.ID, event.LeaseToken, f.now); err != nil {
		t.Fatalf("requeue aborted publication: %v", err)
	}
	retried, err := queue.ClaimByIDAndType(f.ctx, "result-materializer-retry", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || retried == nil {
		t.Fatalf("claim retry event: %#v / %v", retried, err)
	}
	publication.EventID, publication.LeaseToken = retried.ID, retried.LeaseToken
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		return repository.PublishProviderResultMaterialization(tx, publication)
	}); err != nil {
		t.Fatalf("retry publication: %v", err)
	}
	if count, err := f.database.Collection(schema.CollectionAssets).CountDocuments(f.ctx, bson.M{"_id": "asset:" + f.stepID + ":intermediate_result"}); err != nil || count != 1 {
		t.Fatalf("retry intermediate assets = %d / %v, want one", count, err)
	}
	if count, err := f.events.CountDocuments(f.ctx, bson.M{"_id": outbox.SubmissionEventID(secondID)}); err != nil || count != 1 {
		t.Fatalf("retry second submission events = %d / %v, want one", count, err)
	}
	var settled model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&settled); err != nil {
		t.Fatal(err)
	}
	if settled.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || settled.LeaseToken != "" {
		t.Fatalf("retry event settlement = %#v, want delivered", settled)
	}
}

func TestProviderResultMaterializationMongo过期租约不得发布成果(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply terminal: %s / %v", result, err)
	}

	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	event, err := NewOutboxRepository(f.data).ClaimByIDAndType(f.ctx, "result-materializer", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim materialization event: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatalf("parse materialization payload: %v", err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil {
		t.Fatalf("load materialization target: %v", err)
	}
	publishedAt := f.now.Add(2 * time.Minute)
	publication := generation.ProviderResultPublication{
		EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
		StorageURL: "https://media.example.test/results/expired.png", ContentType: "image/png", ContentLength: 42,
		ContentSHA256: strings.Repeat("e", 64), PublishedAt: publishedAt,
	}
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		return repository.PublishProviderResultMaterialization(ctx, publication)
	}); !errors.Is(err, generation.ErrProviderResultPublicationConflict) {
		t.Fatalf("expired lease PublishProviderResultMaterialization() error = %v, want conflict", err)
	}
	assetCount, err := f.database.Collection(schema.CollectionAssets).CountDocuments(f.ctx, bson.M{"owner_id": f.stepID})
	if err != nil || assetCount != 0 {
		t.Fatalf("expired lease published an asset: count=%d err=%v", assetCount, err)
	}
	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusPendingSubmission) {
		t.Fatalf("expired lease changed creation status: %#v", creation)
	}
	var pending model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&pending); err != nil {
		t.Fatal(err)
	}
	if pending.DeliveryStatus != string(outbox.DeliveryStatusDispatching) || pending.LeaseToken != event.LeaseToken {
		t.Fatalf("expired lease changed event outside a successful fence: %#v", pending)
	}
}

// A two-step B2B video must not be declared successful when its first image is
// materialized. The transaction below proves the only allowed hand-off: store
// the verified R2 frame as an intermediate asset, consume the unbound recipe,
// make step two ready and enqueue its exact opening_frame product.
func TestProviderResultMaterializationMongo两步B2B首帧原子激活第二步(t *testing.T) {
	f, fact, secondID, deferred := seedTwoStepProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply first terminal: %s / %v", result, err)
	}
	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	queue := NewOutboxRepository(f.data)
	event, err := queue.ClaimByIDAndType(f.ctx, "result-materializer", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim first materialization: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil || target.NextB2B == nil || target.NextB2B.StepID != secondID || target.NextB2B.Deferred.Digest != deferred.Digest {
		t.Fatalf("two-step materialization target = %#v / %v", target, err)
	}
	if result, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID}, bson.M{"$set": bson.M{
		"r2_upload_pending": true, "r2_upload_pending_at": f.now, "r2_upload_pending_reason": "material_upload_budget_exhausted",
	}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("seed first-stage r2 upload pending: %v", err)
	}
	contentSHA256 := strings.Repeat("e", 64)
	storageURL := "https://media.example.test/results/tenant/" + contentSHA256 + ".png"
	secondRecipe, err := deferred.BindOwnedOpeningFrame(storageURL)
	if err != nil {
		t.Fatal(err)
	}
	publication := generation.ProviderResultPublication{
		EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
		StorageURL: storageURL, ContentType: "image/png", ContentLength: 42, ContentSHA256: contentSHA256, PublishedAt: f.now,
		NextB2B: &generation.ProviderB2BSecondStepActivation{StepID: secondID, Route: target.NextB2B.Route, Recipe: secondRecipe},
	}
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		return repository.PublishProviderResultMaterialization(tx, publication)
	}); err != nil {
		t.Fatalf("activate second B2B step: %v", err)
	}

	var first, second model.CreationStepDocument
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if first.SubmitStatus != string(creations.StepSubmitStatusSucceeded) || first.R2UploadPending ||
		!first.R2UploadPendingAt.IsZero() || first.R2UploadPendingReason != "" ||
		second.SubmitStatus != string(creations.StepSubmitStatusReady) || second.ExternalExecutionID != "" {
		t.Fatalf("two-step state = first:%#v second:%#v", first, second)
	}
	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusPendingSubmission) {
		t.Fatalf("creation prematurely succeeded: %#v", creation)
	}
	var reservation model.ReservationDocument
	if err := f.database.Collection(schema.CollectionReservations).FindOne(f.ctx, bson.M{"creation_id": f.creationID}).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.Status != string(ledger.ReservationStatusReserved) || reservation.PublicationState == "published" || reservation.PublicationState == "reversed" {
		t.Fatalf("first image closed/refunded reservation gate: %#v", reservation)
	}
	var intermediate model.AssetDocument
	if err := f.database.Collection(schema.CollectionAssets).FindOne(f.ctx, bson.M{"_id": "asset:" + f.stepID + ":intermediate_result"}).Decode(&intermediate); err != nil {
		t.Fatal(err)
	}
	if intermediate.AssetKind != callbackAssetKindIntermediate || intermediate.StorageKey != storageURL || intermediate.ContentSHA256 != contentSHA256 {
		t.Fatalf("intermediate asset = %#v", intermediate)
	}
	var recipe model.DeferredRecipeDocument
	if err := f.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&recipe); err != nil {
		t.Fatal(err)
	}
	if recipe.Status != string(creations.DeferredRecipeStatusConsumed) || recipe.Protocol != string(creations.DeferredRecipeProtocolB2B) {
		t.Fatalf("deferred recipe = %#v", recipe)
	}
	var secondEvent model.OutboxEventDocument
	secondEventID := outbox.SubmissionEventID(secondID)
	if err := f.events.FindOne(f.ctx, bson.M{"_id": secondEventID}).Decode(&secondEvent); err != nil {
		t.Fatal(err)
	}
	if secondEvent.AggregateID != f.creationID || secondEvent.EventType != string(outbox.EventTypeGenerationSubmission) || secondEvent.DeliveryStatus != string(outbox.DeliveryStatusPending) {
		t.Fatalf("second submission event = %#v", secondEvent)
	}
	storedSecondRecipe, err := creations.ParseB2BProductRecipe(secondEvent.Payload)
	if err != nil || len(storedSecondRecipe.Assets) != 1 || storedSecondRecipe.Assets[0] != (creations.B2BAsset{Role: "opening_frame", URL: storageURL}) {
		t.Fatalf("second submission recipe = %#v / %v", storedSecondRecipe, err)
	}
	var firstEvent model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&firstEvent); err != nil {
		t.Fatal(err)
	}
	if firstEvent.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || firstEvent.LeaseToken != "" {
		t.Fatalf("first materialization event not delivered: %#v", firstEvent)
	}
}

// A cancellation request can race with the first B2B stage reaching its
// canonical completed terminal.  Completed wins this particular race: once
// the frame is safely owned, the same transaction must reopen the creation
// for the exact second-stage submission.  Leaving it cancelling would make
// the submission worker reject that first second-stage POST forever.
func TestProviderResultMaterializationMongo两步B2B首帧完成赢得取消竞态并继续第二步(t *testing.T) {
	f, fact, secondID, deferred := seedTwoStepProviderTerminal(t)
	if result, err := f.creations.UpdateOne(f.ctx, bson.M{"_id": f.creationID}, bson.M{"$set": bson.M{"status": string(creations.CreationStatusCancelling)}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("mark fixture cancelling: %v", err)
	}
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply first completed terminal: %s / %v", result, err)
	}

	firstEventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	queue := NewOutboxRepository(f.data)
	event, err := queue.ClaimByIDAndType(f.ctx, "result-materializer", firstEventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim first materialization: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil || target.NextB2B == nil || target.NextB2B.StepID != secondID {
		t.Fatalf("load first materialization target: %#v / %v", target, err)
	}
	frameSHA := strings.Repeat("c", 64)
	frameURL := "https://media.example.test/results/tenant/" + frameSHA + ".png"
	secondRecipe, err := deferred.BindOwnedOpeningFrame(frameURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		return repository.PublishProviderResultMaterialization(tx, generation.ProviderResultPublication{
			EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
			StorageURL: frameURL, ContentType: "image/png", ContentLength: 42, ContentSHA256: frameSHA, PublishedAt: f.now,
			NextB2B: &generation.ProviderB2BSecondStepActivation{StepID: secondID, Route: target.NextB2B.Route, Recipe: secondRecipe},
		})
	}); err != nil {
		t.Fatalf("publish first materialization: %v", err)
	}

	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusPendingSubmission) {
		t.Fatalf("creation status = %q, want pending_submission after completed wins cancellation race", creation.Status)
	}
	var second model.CreationStepDocument
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.SubmitStatus != string(creations.StepSubmitStatusReady) {
		t.Fatalf("second submit status = %q, want ready", second.SubmitStatus)
	}
	if count, err := f.events.CountDocuments(f.ctx, bson.M{"_id": outbox.SubmissionEventID(secondID)}); err != nil || count != 1 {
		t.Fatalf("second submission event count = %d / %v, want one", count, err)
	}
}

// Two workers can both have read the same completed first-frame event before
// either starts its Mongo transaction.  The frozen target and the event lease
// are therefore not sufficient on their own: the durable transitions below
// must admit exactly one activation and leave no duplicate second submission.
func TestProviderResultMaterializationMongo两步B2B并发首帧仅激活一次(t *testing.T) {
	f, fact, secondID, deferred := seedTwoStepProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply first terminal: %s / %v", result, err)
	}
	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	event, err := NewOutboxRepository(f.data).ClaimByIDAndType(f.ctx, "result-materializer", eventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim first materialization: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderResultMaterialization(f.ctx, payload)
	if err != nil || target.NextB2B == nil || target.NextB2B.StepID != secondID {
		t.Fatalf("two-step materialization target = %#v / %v", target, err)
	}
	contentSHA256 := strings.Repeat("f", 64)
	storageURL := "https://media.example.test/results/tenant/" + contentSHA256 + ".png"
	secondRecipe, err := deferred.BindOwnedOpeningFrame(storageURL)
	if err != nil {
		t.Fatal(err)
	}
	publication := generation.ProviderResultPublication{
		EventID: event.ID, LeaseToken: event.LeaseToken, Target: target,
		StorageURL: storageURL, ContentType: "image/png", ContentLength: 42, ContentSHA256: contentSHA256, PublishedAt: f.now,
		NextB2B: &generation.ProviderB2BSecondStepActivation{StepID: secondID, Route: target.NextB2B.Route, Recipe: secondRecipe},
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- f.runner.WithinTx(f.ctx, func(tx context.Context) error {
				return repository.PublishProviderResultMaterialization(tx, publication)
			})
		}()
	}
	close(start)
	succeeded, conflicted := 0, 0
	for range 2 {
		if err := <-results; err == nil {
			succeeded++
		} else if errors.Is(err, generation.ErrProviderResultPublicationConflict) {
			conflicted++
		} else {
			t.Fatalf("concurrent activation error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("activation outcomes: succeeded=%d conflicted=%d", succeeded, conflicted)
	}

	assetCount, err := f.database.Collection(schema.CollectionAssets).CountDocuments(f.ctx, bson.M{"_id": "asset:" + f.stepID + ":intermediate_result"})
	if err != nil || assetCount != 1 {
		t.Fatalf("intermediate assets = %d / %v, want one", assetCount, err)
	}
	eventCount, err := f.events.CountDocuments(f.ctx, bson.M{"_id": outbox.SubmissionEventID(secondID)})
	if err != nil || eventCount != 1 {
		t.Fatalf("second submission events = %d / %v, want one", eventCount, err)
	}
	var second model.CreationStepDocument
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.SubmitStatus != string(creations.StepSubmitStatusReady) || second.ExternalExecutionID != "" {
		t.Fatalf("second step after concurrent activation = %#v", second)
	}
}

// A failed/cancelled first B2B stage reverses the single reservation and
// terminalizes the blocked descendant in that same settlement transaction.
// There must be no second submission event left for a later worker to claim.
func TestProviderTerminalMongo两步B2B首步失败原子终止后续步骤并冲正(t *testing.T) {
	f, fact, secondID, _ := seedTwoStepProviderTerminal(t)
	fact.Status = generation.ProviderTerminalFailed
	fact.ResultRef = ""
	fact.TerminalDigest = generation.ProviderTerminalSummaryDigest(fact.Status, "")
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply failed first terminal: %s / %v", result, err)
	}
	eventID := generation.ProviderTerminalSettlementEventID(fact.StepID)
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		_, _ = f.events.DeleteOne(ctx, bson.M{"_id": eventID})
	})
	queue := NewOutboxRepository(f.data)
	event, err := queue.ClaimByIDAndType(f.ctx, "terminal-settler", eventID, outbox.EventType(generation.ProviderTerminalSettlementEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim first terminal settlement: %#v / %v", event, err)
	}
	payload, err := generation.ParseProviderTerminalSettlementEventPayload(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderTerminalSettlement(f.ctx, payload)
	if err != nil {
		t.Fatalf("load first terminal settlement: %v", err)
	}
	ledgerUsecase := ledger.NewUsecaseWithClock(NewLedgerRepository(f.data), f.runner, func() time.Time { return f.now })
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		if _, err := ledgerUsecase.ReverseInTx(tx, f.creationID, ledger.ReversalReasonGenerationFailed, f.now); err != nil {
			return err
		}
		return repository.SettleProviderTerminal(tx, generation.ProviderTerminalSettlement{EventID: eventID, LeaseToken: event.LeaseToken, Target: target, SettledAt: f.now})
	}); err != nil {
		t.Fatalf("settle failed first terminal: %v", err)
	}

	var first, second model.CreationStepDocument
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if first.SubmitStatus != string(creations.StepSubmitStatusGenerationFailed) || second.SubmitStatus != string(creations.StepSubmitStatusGenerationFailed) || second.ExternalExecutionID != "" {
		t.Fatalf("failed two-step state = first:%#v second:%#v", first, second)
	}
	var recipe model.DeferredRecipeDocument
	if err := f.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&recipe); err != nil {
		t.Fatal(err)
	}
	if recipe.Status != string(creations.DeferredRecipeStatusAbandoned) {
		t.Fatalf("deferred recipe status = %q, want abandoned", recipe.Status)
	}
	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusGenerationFailed) {
		t.Fatalf("creation status = %q", creation.Status)
	}
	var reservation model.ReservationDocument
	if err := f.database.Collection(schema.CollectionReservations).FindOne(f.ctx, bson.M{"creation_id": f.creationID}).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.Status != string(ledger.ReservationStatusReversed) || reservation.PublicationState != "reversed" {
		t.Fatalf("reservation = %#v", reservation)
	}
	count, err := f.events.CountDocuments(f.ctx, bson.M{"_id": outbox.SubmissionEventID(secondID)})
	if err != nil || count != 0 {
		t.Fatalf("second submission event = %d / %v, want none", count, err)
	}
	var settled model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&settled); err != nil {
		t.Fatal(err)
	}
	if settled.DeliveryStatus != string(outbox.DeliveryStatusDelivered) || settled.LeaseToken != "" {
		t.Fatalf("settlement event = %#v", settled)
	}
}

// Only the second (video) result may close publication_state and make the
// creation user-visible. This storage-level test seeds a bound terminal after
// the first-stage transaction, then verifies the final materialization takes
// the normal final-publication path rather than treating the intermediate asset
// as a result.
func TestProviderResultMaterializationMongo两步B2B第二步成功才发布最终视频(t *testing.T) {
	f, firstFact, secondID, deferred := seedTwoStepProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, firstFact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply first terminal: %s / %v", result, err)
	}
	firstEventID := generation.ProviderResultMaterializeEventID(firstFact.StepID)
	queue := NewOutboxRepository(f.data)
	firstEvent, err := queue.ClaimByIDAndType(f.ctx, "result-materializer", firstEventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || firstEvent == nil {
		t.Fatalf("claim first materialization: %#v / %v", firstEvent, err)
	}
	firstPayload, err := generation.ParseProviderResultMaterializeEventPayload(firstEvent.Payload)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	firstTarget, err := repository.LoadProviderResultMaterialization(f.ctx, firstPayload)
	if err != nil || firstTarget.NextB2B == nil {
		t.Fatalf("load first target: %#v / %v", firstTarget, err)
	}
	frameSHA := strings.Repeat("f", 64)
	frameURL := "https://media.example.test/results/tenant/" + frameSHA + ".png"
	secondRecipe, err := deferred.BindOwnedOpeningFrame(frameURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		return repository.PublishProviderResultMaterialization(tx, generation.ProviderResultPublication{
			EventID: firstEvent.ID, LeaseToken: firstEvent.LeaseToken, Target: firstTarget,
			StorageURL: frameURL, ContentType: "image/png", ContentLength: 42, ContentSHA256: frameSHA, PublishedAt: f.now,
			NextB2B: &generation.ProviderB2BSecondStepActivation{StepID: secondID, Route: firstTarget.NextB2B.Route, Recipe: secondRecipe},
		})
	}); err != nil {
		t.Fatalf("activate second B2B step: %v", err)
	}
	// A real provider-job binding marks this submission event delivered before
	// any second-stage terminal can exist. Mirror that durable fact here rather
	// than allowing the final materializer to bypass the binding gate.
	if result, err := f.events.UpdateOne(f.ctx, bson.M{"_id": outbox.SubmissionEventID(secondID)}, bson.M{"$set": bson.M{"delivery_status": string(outbox.DeliveryStatusDelivered)}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("mark second submission binding delivered: %v", err)
	}

	secondJobID := "job-final-" + secondID
	videoRef := "https://results.example.test/" + secondID + ".mp4"
	terminalDigest := generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalCompleted, videoRef)
	result, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": secondID, "submit_status": string(creations.StepSubmitStatusReady)}, bson.M{"$set": bson.M{
		"submit_status": string(creations.StepSubmitStatusSubmitted), "external_execution_id": secondJobID,
		"terminal_version": int64(1), "terminal_status": string(generation.ProviderTerminalCompleted), "terminal_result_ref": videoRef,
		"terminal_digest": terminalDigest,
	}})
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("seed bound second terminal: %v", err)
	}
	secondEventPayload, err := generation.MarshalProviderResultMaterializeEventPayload(generation.ProviderResultMaterializeEventPayload{
		CreationID: f.creationID, StepID: secondID, Provider: firstFact.Provider, AccountRef: firstFact.AccountRef,
		JobID: secondJobID, Capability: string(creations.AtomImageToVideo), ResultRef: videoRef, TerminalVersion: 1, TerminalDigest: terminalDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondResultEventID := generation.ProviderResultMaterializeEventID(secondID)
	if _, err := f.events.InsertOne(f.ctx, model.OutboxEventDocument{ID: secondResultEventID, AggregateID: f.creationID, EventType: generation.ProviderResultMaterializeEventType, Payload: secondEventPayload, DeliveryStatus: string(outbox.DeliveryStatusPending), NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		_, _ = f.events.DeleteOne(ctx, bson.M{"_id": secondResultEventID})
		_, _ = f.database.Collection(schema.CollectionAssets).DeleteOne(ctx, bson.M{"_id": "asset:" + secondID + ":result"})
	})
	claimedSecond, err := queue.ClaimByIDAndType(f.ctx, "result-materializer", secondResultEventID, outbox.EventType(generation.ProviderResultMaterializeEventType), f.now, f.now.Add(time.Minute))
	if err != nil || claimedSecond == nil {
		t.Fatalf("claim final materialization: %#v / %v", claimedSecond, err)
	}
	finalPayload, err := generation.ParseProviderResultMaterializeEventPayload(claimedSecond.Payload)
	if err != nil {
		t.Fatal(err)
	}
	finalTarget, err := repository.LoadProviderResultMaterialization(f.ctx, finalPayload)
	if err != nil || finalTarget.NextB2B != nil || finalTarget.Sequence != 2 {
		t.Fatalf("load final target: %#v / %v", finalTarget, err)
	}
	videoSHA := strings.Repeat("a", 64)
	videoURL := "https://media.example.test/results/tenant/" + videoSHA + ".mp4"
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		return repository.PublishProviderResultMaterialization(tx, generation.ProviderResultPublication{
			EventID: claimedSecond.ID, LeaseToken: claimedSecond.LeaseToken, Target: finalTarget,
			StorageURL: videoURL, ContentType: "video/mp4", ContentLength: 88, ContentSHA256: videoSHA, PublishedAt: f.now,
		})
	}); err != nil {
		t.Fatalf("publish final video: %v", err)
	}
	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusSucceeded) {
		t.Fatalf("creation status = %q, want succeeded", creation.Status)
	}
	var reservation model.ReservationDocument
	if err := f.database.Collection(schema.CollectionReservations).FindOne(f.ctx, bson.M{"creation_id": f.creationID}).Decode(&reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.PublicationState != "published" || reservation.Status != string(ledger.ReservationStatusReserved) {
		t.Fatalf("final reservation gate = %#v", reservation)
	}
	var finalAsset model.AssetDocument
	if err := f.database.Collection(schema.CollectionAssets).FindOne(f.ctx, bson.M{"_id": "asset:" + secondID + ":result"}).Decode(&finalAsset); err != nil {
		t.Fatal(err)
	}
	if finalAsset.AssetKind != callbackAssetKindResult || finalAsset.StorageKey != videoURL || finalAsset.ContentType != "video/mp4" {
		t.Fatalf("final video asset = %#v", finalAsset)
	}
}

func TestProviderTerminalMongo两步B2B第二步失败保留首帧并原子冲正(t *testing.T) {
	f, firstFact, secondID, _ := seedTwoStepProviderTerminal(t)
	secondJobID := "job-failed-" + secondID
	if result, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": f.stepID}, bson.M{"$set": bson.M{"submit_status": string(creations.StepSubmitStatusSucceeded)}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("seed succeeded first step: %v", err)
	}
	secondDigest := generation.ProviderTerminalSummaryDigest(generation.ProviderTerminalFailed, "")
	if result, err := f.steps.UpdateOne(f.ctx, bson.M{"_id": secondID}, bson.M{"$set": bson.M{
		"submit_status": string(creations.StepSubmitStatusSubmitted), "external_execution_id": secondJobID,
		"terminal_version": int64(1), "terminal_status": string(generation.ProviderTerminalFailed), "terminal_digest": secondDigest,
	}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("seed failed second terminal: %v", err)
	}
	if result, err := f.database.Collection(schema.CollectionGenerationStepRecipes).UpdateOne(f.ctx, bson.M{"_id": secondID}, bson.M{"$set": bson.M{"status": string(creations.DeferredRecipeStatusConsumed)}}); err != nil || result.MatchedCount != 1 {
		t.Fatalf("seed consumed second recipe: %v", err)
	}
	secondSubmissionID := outbox.SubmissionEventID(secondID)
	if _, err := f.events.InsertOne(f.ctx, model.OutboxEventDocument{ID: secondSubmissionID, AggregateID: f.creationID, EventType: string(outbox.EventTypeGenerationSubmission), Payload: []byte(`{"productKey":"video-standard","input":{"prompt":"camera move","durationSeconds":5},"assets":[{"role":"opening_frame","url":"https://media.example.test/frame.png"}]}`), DeliveryStatus: string(outbox.DeliveryStatusDelivered), NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	fact := generation.ProviderTerminalSettlementEventPayload{CreationID: f.creationID, StepID: secondID, Provider: firstFact.Provider, AccountRef: firstFact.AccountRef, JobID: secondJobID, Capability: string(creations.AtomImageToVideo), Status: generation.ProviderTerminalFailed, TerminalVersion: 1, TerminalDigest: secondDigest}
	settlementPayload, err := generation.MarshalProviderTerminalSettlementEventPayload(fact)
	if err != nil {
		t.Fatal(err)
	}
	settlementID := generation.ProviderTerminalSettlementEventID(secondID)
	if _, err := f.events.InsertOne(f.ctx, model.OutboxEventDocument{ID: settlementID, AggregateID: f.creationID, EventType: generation.ProviderTerminalSettlementEventType, Payload: settlementPayload, DeliveryStatus: string(outbox.DeliveryStatusPending), NextAttemptAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		_, _ = f.events.DeleteOne(ctx, bson.M{"_id": secondSubmissionID})
		_, _ = f.events.DeleteOne(ctx, bson.M{"_id": settlementID})
	})
	queue := NewOutboxRepository(f.data)
	event, err := queue.ClaimByIDAndType(f.ctx, "terminal-settler", settlementID, outbox.EventType(generation.ProviderTerminalSettlementEventType), f.now, f.now.Add(time.Minute))
	if err != nil || event == nil {
		t.Fatalf("claim failed second settlement: %#v / %v", event, err)
	}
	repository := NewGenerationCallbackRepository(f.data)
	target, err := repository.LoadProviderTerminalSettlement(f.ctx, fact)
	if err != nil {
		t.Fatalf("load failed second settlement: %v", err)
	}
	ledgerUsecase := ledger.NewUsecaseWithClock(NewLedgerRepository(f.data), f.runner, func() time.Time { return f.now })
	if err := f.runner.WithinTx(f.ctx, func(tx context.Context) error {
		if _, err := ledgerUsecase.ReverseInTx(tx, f.creationID, ledger.ReversalReasonGenerationFailed, f.now); err != nil {
			return err
		}
		return repository.SettleProviderTerminal(tx, generation.ProviderTerminalSettlement{EventID: settlementID, LeaseToken: event.LeaseToken, Target: target, SettledAt: f.now})
	}); err != nil {
		t.Fatalf("settle failed second terminal: %v", err)
	}
	var first, second model.CreationStepDocument
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&second); err != nil {
		t.Fatal(err)
	}
	if first.SubmitStatus != string(creations.StepSubmitStatusSucceeded) || second.SubmitStatus != string(creations.StepSubmitStatusGenerationFailed) {
		t.Fatalf("second failure changed graph incorrectly: first=%#v second=%#v", first, second)
	}
	var recipe model.DeferredRecipeDocument
	if err := f.database.Collection(schema.CollectionGenerationStepRecipes).FindOne(f.ctx, bson.M{"_id": secondID}).Decode(&recipe); err != nil {
		t.Fatal(err)
	}
	if recipe.Status != string(creations.DeferredRecipeStatusConsumed) {
		t.Fatalf("second failure must retain consumed audit recipe, got %q", recipe.Status)
	}
	var creation model.CreationDocument
	if err := f.creations.FindOne(f.ctx, bson.M{"_id": f.creationID}).Decode(&creation); err != nil {
		t.Fatal(err)
	}
	if creation.Status != string(creations.CreationStatusGenerationFailed) {
		t.Fatalf("creation status = %q", creation.Status)
	}
}

func seedTwoStepProviderTerminal(t *testing.T) (*submissionMongoFixture, generation.ProviderTerminalFact, string, creations.DeferredB2BImageToVideo) {
	t.Helper()
	f, fact := seedProviderTerminal(t)
	secondID := "step-video-" + f.stepID
	route := creations.ExecutionRoute{Provider: fact.Provider, AccountRef: fact.AccountRef, ContractVersion: creations.B2BContractVersion, MappingVersion: "mapping-1"}
	deferred, err := creations.CompileDeferredB2BImageToVideo(creations.PublishedB2BProductRecipe{
		TemplateID: "template-video-1", TemplateVersion: 1, Atom: creations.AtomImageToVideo,
		ProductKey: "video-standard", Input: json.RawMessage(`{"durationSeconds":5,"prompt":"camera move"}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b2bPayload, err := deferred.Recipe.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.steps.InsertOne(f.ctx, model.CreationStepDocument{
		ID: secondID, CreationID: f.creationID, Sequence: 2, Atom: string(creations.AtomImageToVideo),
		Provider: route.Provider, AccountRef: route.AccountRef, ContractVersion: route.ContractVersion, MappingVersion: route.MappingVersion,
		SubmitStatus: string(creations.StepSubmitStatusBlocked), CreatedAt: f.now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.Collection(schema.CollectionGenerationStepRecipes).InsertOne(f.ctx, model.DeferredRecipeDocument{
		ID: secondID, StepID: secondID, CreationID: f.creationID, Atom: string(creations.AtomImageToVideo),
		Protocol: string(creations.DeferredRecipeProtocolB2B), B2BRecipe: b2bPayload, Digest: deferred.Digest,
		Status: string(creations.DeferredRecipeStatusPending), CreatedAt: f.now, UpdatedAt: f.now,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := newMongoTestContext()
		defer cancel()
		_, _ = f.steps.DeleteOne(ctx, bson.M{"_id": secondID})
		_, _ = f.database.Collection(schema.CollectionGenerationStepRecipes).DeleteOne(ctx, bson.M{"_id": secondID})
		_, _ = f.events.DeleteOne(ctx, bson.M{"_id": outbox.SubmissionEventID(secondID)})
		_, _ = f.database.Collection(schema.CollectionAssets).DeleteOne(ctx, bson.M{"_id": "asset:" + f.stepID + ":intermediate_result"})
	})
	return f, fact, secondID, *deferred
}

// 已完成终态后来收到相反结论时，统一 terminal CAS 会留下 quarantine 证据。
// 素材 Worker 必须在下载前止步，不能因为先到的 event 仍合法就把有争议的结果
// 发布给用户。
func TestProviderResultMaterializationMongo隔离终态不允许读取或发布(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("apply completed terminal: %s / %v", result, err)
	}
	conflict := fact
	conflict.Status = generation.ProviderTerminalFailed
	conflict.ResultRef = ""
	conflict.TerminalDigest = generation.ProviderTerminalSummaryDigest(conflict.Status, "")
	if result, err := applyProviderTerminalTx(f, conflict); err != nil || result != generation.ProviderTerminalQuarantined {
		t.Fatalf("apply conflicting terminal: %s / %v", result, err)
	}
	var event model.OutboxEventDocument
	if err := f.events.FindOne(f.ctx, bson.M{"_id": generation.ProviderResultMaterializeEventID(f.stepID)}).Decode(&event); err != nil {
		t.Fatalf("read scheduled materialization event: %v", err)
	}
	payload, err := generation.ParseProviderResultMaterializeEventPayload(event.Payload)
	if err != nil {
		t.Fatalf("parse materialization event: %v", err)
	}
	if _, err := NewGenerationCallbackRepository(f.data).LoadProviderResultMaterialization(f.ctx, payload); !errors.Is(err, generation.ErrProviderResultPublicationConflict) {
		t.Fatalf("quarantined terminal target = %v, want publication conflict", err)
	}
}

func TestProviderTerminalMongoStaleFenceDoesNotPoisonSlot(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	stale := fact
	stale.AttemptFence++
	if _, err := applyProviderTerminalTx(f, stale); !errors.Is(err, generation.ErrProviderTerminalFence) {
		t.Fatalf("stale fence accepted: %v", err)
	}
	// Deliberately commit the caller transaction after inspecting rejection:
	// storage must not have mutated the pending slot at all.
	if err := f.runner.WithinTx(f.ctx, func(ctx context.Context) error {
		_, err := NewGenerationCallbackRepository(f.data).ApplyProviderTerminal(ctx, stale)
		if !errors.Is(err, generation.ErrProviderTerminalFence) {
			return errors.New("expected stale fence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var step bson.M
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if step["terminal_quarantined"] == true || step["terminal_status"] != nil {
		t.Fatal("stale fence poisoned pending slot")
	}
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("valid terminal blocked by stale worker: %s / %v", result, err)
	}
}

func TestProviderTerminalMongoStopsReconciliationAndOldLease(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	queue := NewOutboxRepository(f.data)
	eventID := "generation.reconcile:" + f.stepID
	claimed, err := queue.ClaimByIDAndType(f.ctx, "lookup-worker", eventID, outbox.EventType("generation.reconcile"), f.now.Add(time.Minute), f.now.Add(2*time.Minute))
	if err != nil || claimed == nil {
		t.Fatalf("claim recovery: %v", err)
	}
	if result, err := applyProviderTerminalTx(f, fact); err != nil || result != generation.ProviderTerminalApplied {
		t.Fatalf("terminal: %s / %v", result, err)
	}
	var event bson.M
	if err := f.events.FindOne(f.ctx, bson.M{"_id": eventID}).Decode(&event); err != nil {
		t.Fatal(err)
	}
	if event["delivery_status"] != string(outbox.DeliveryStatusDelivered) || event["terminal_version"] != int64(1) || event["lease_token"] != nil || event["lease_owner"] != nil || event["lease_until"] != nil {
		t.Fatalf("terminal retained active reconciliation: %#v", event)
	}
	if err := queue.Requeue(f.ctx, eventID, claimed.LeaseToken, f.now.Add(3*time.Minute)); !errors.Is(err, outbox.ErrLeaseConflict) {
		t.Fatalf("old lookup lease revived recovery: %v", err)
	}
	if event, err := queue.ClaimByIDAndType(f.ctx, "other-worker", eventID, outbox.EventType("generation.reconcile"), f.now.Add(4*time.Minute), f.now.Add(5*time.Minute)); err != nil || event != nil {
		t.Fatalf("terminal recovery claimable: %#v / %v", event, err)
	}
	count, err := f.database.Collection(schema.CollectionAssets).CountDocuments(f.ctx, bson.M{"owner_id": f.stepID})
	if err != nil || count != 0 {
		t.Fatalf("terminal fact published material: %d / %v", count, err)
	}
	count, err = f.database.Collection(schema.CollectionLedgerEntries).CountDocuments(f.ctx, bson.M{"creation_id": f.creationID})
	if err != nil || count != 1 {
		t.Fatalf("terminal fact changed ledger: %d / %v", count, err)
	}
}

func TestProviderTerminalMongoConcurrentCompletedFailedSingleWinner(t *testing.T) {
	f, completed := seedProviderTerminal(t)
	failed := completed
	failed.Status = generation.ProviderTerminalFailed
	failed.ResultRef = ""
	failed.TerminalDigest = generation.ProviderTerminalSummaryDigest(failed.Status, failed.ResultRef)
	type outcome struct {
		result generation.ProviderTerminalApplyResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, fact := range []generation.ProviderTerminalFact{completed, failed} {
		go func(fact generation.ProviderTerminalFact) {
			<-start
			result, err := applyProviderTerminalTx(f, fact)
			results <- outcome{result, err}
		}(fact)
	}
	close(start)
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.result == generation.ProviderTerminalApplied {
			wins++
		}
		if got.result == generation.ProviderTerminalQuarantined {
			conflicts++
		}
	}
	var step bson.M
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if wins != 1 || conflicts != 1 || step["terminal_version"] != int64(1) || step["terminal_quarantined"] != true {
		t.Fatalf("wins=%d conflicts=%d step=%#v", wins, conflicts, step)
	}
}

func TestProviderTerminalMongoRecoveryConflictRollsBackTerminal(t *testing.T) {
	f, fact := seedProviderTerminal(t)
	if _, err := f.events.UpdateOne(f.ctx, bson.M{"_id": "generation.reconcile:" + f.stepID}, bson.M{"$set": bson.M{"event_type": "wrong"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyProviderTerminalTx(f, fact); err == nil {
		t.Fatal("conflicting recovery event accepted")
	}
	var step bson.M
	if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
		t.Fatal(err)
	}
	if step["terminal_version"] != int64(0) || step["terminal_status"] != nil {
		t.Fatal("partial terminal write escaped failed recovery transaction")
	}
}

func TestProviderTerminalMongoRejectsInconsistentRecoveryIdentity(t *testing.T) {
	for _, field := range []string{"aggregate_id", "payload"} {
		t.Run(field, func(t *testing.T) {
			f, fact := seedProviderTerminal(t)
			if _, err := f.events.UpdateOne(f.ctx, bson.M{"_id": generation.ProviderInboxRecoveryEventID(f.stepID)}, bson.M{"$set": bson.M{field: "wrong"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := applyProviderTerminalTx(f, fact); !errors.Is(err, generation.ErrProviderTerminalConflict) {
				t.Fatalf("inconsistent recovery %s accepted: %v", field, err)
			}
			var step bson.M
			if err := f.steps.FindOne(f.ctx, bson.M{"_id": f.stepID}).Decode(&step); err != nil {
				t.Fatal(err)
			}
			if step["terminal_version"] != int64(0) || step["terminal_status"] != nil {
				t.Fatalf("failed terminal escaped transaction: %#v", step)
			}
		})
	}
}
