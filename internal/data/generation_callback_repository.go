package data

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"ai-business-service/internal/executionv2"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	generationCallbackReceiptSource = "generation.execution.v2"
	callbackVersionV2               = int64(2)
	callbackAssetOwnerType          = "creation_step"
	callbackAssetKindResult         = "result"
	// callbackAssetKindIntermediate is deliberately excluded from all public
	// works/status readers, which only expose asset_kind=result. It records a
	// verified first frame needed by the second B2B stage without presenting an
	// intermediate image as the user's finished video.
	callbackAssetKindIntermediate = "intermediate_result"
	callbackAssetStatusAvailable  = "available"
)

// mongoGenerationCallbackRepository 在调用方事务内同时实现可信步骤关联和回调终态写入。
type mongoGenerationCallbackRepository struct {
	creations    *mongo.Collection
	steps        *mongo.Collection
	recipes      *mongo.Collection
	assets       *mongo.Collection
	reservations *mongo.Collection
	receipts     *mongo.Collection
	events       *mongo.Collection
}

// NewGenerationCallbackRepository 返回生成回调的关联器和终态仓储实现。
func NewGenerationCallbackRepository(data *Data) *mongoGenerationCallbackRepository {
	if data == nil || data.database == nil {
		return &mongoGenerationCallbackRepository{}
	}
	return &mongoGenerationCallbackRepository{
		creations:    data.database.Collection(schema.CollectionCreations),
		steps:        data.database.Collection(schema.CollectionCreationSteps),
		recipes:      data.database.Collection(schema.CollectionGenerationStepRecipes),
		assets:       data.database.Collection(schema.CollectionAssets),
		reservations: data.database.Collection(schema.CollectionReservations),
		receipts:     data.database.Collection(schema.CollectionCallbackReceipts),
		events:       data.database.Collection(schema.CollectionOutboxEvents),
	}
}

// NewGenerationCallbackStore 将 Mongo 回调仓储收窄为终态写入边界。
func NewGenerationCallbackStore(repository *mongoGenerationCallbackRepository) generation.CallbackStore {
	return repository
}

// NewGenerationCallbackLinker 将 Mongo 回调仓储收窄为可信步骤关联边界。
func NewGenerationCallbackLinker(repository *mongoGenerationCallbackRepository) generation.CallbackLinker {
	return repository
}

// NewGenerationProviderTerminalStore 暴露 B2B 统一终态 CAS，供 callback、
// lookup 与 inbox consumer 组合根按需注入；旧回调 store 保持兼容不变。
func NewGenerationProviderTerminalStore(repository *mongoGenerationCallbackRepository) generation.ProviderTerminalStore {
	return repository
}

// NewGenerationProviderTerminalSettlementStore exposes the separate durable
// failure-settlement boundary. Keeping it separate from the terminal CAS makes
// it impossible for a callback acknowledgement to perform a refund directly.
func NewGenerationProviderTerminalSettlementStore(repository *mongoGenerationCallbackRepository) generation.ProviderTerminalSettlementStore {
	return repository
}

// Link 仅从已保存步骤读取创作归属，并核验任务标识和技术原子。
func (repository *mongoGenerationCallbackRepository) Link(ctx context.Context, event generation.VerifiedCallbackEvent) (generation.LinkedCallback, error) {
	if err := repository.ready(); err != nil {
		return generation.LinkedCallback{}, err
	}
	if event.StepID == "" || event.JobID == "" || event.ExternalRef != event.StepID || !callbackCapabilityMatchesAtom(event.Capability, "") {
		return generation.LinkedCallback{}, generation.ErrInvalidCallbackEvent
	}
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: event.StepID},
		{Key: "external_execution_id", Value: event.JobID},
		{Key: "atom", Value: callbackAtom(event.Capability)},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.LinkedCallback{}, generation.ErrInvalidCallbackEvent
	}
	if err != nil || step.CreationID == "" {
		if err != nil {
			return generation.LinkedCallback{}, fmt.Errorf("link generation callback step: %w", err)
		}
		return generation.LinkedCallback{}, generation.ErrInvalidCallbackEvent
	}
	return generation.NewLinkedCallback(step.CreationID, event)
}

// Complete 先写回执，再用 submitted 状态和回调版本执行终态 CAS。
func (repository *mongoGenerationCallbackRepository) Complete(ctx context.Context, command generation.CompletedCallback) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if !callbackTransactionActive(ctx) {
		return generation.ErrInvalidCallbackEvent
	}
	if !validCompletedCommand(command) {
		return generation.ErrInvalidCallbackEvent
	}
	if err := repository.insertReceipt(ctx, completedReceipt(command)); err != nil {
		return err
	}

	matched, err := repository.updateTerminalStep(ctx, command.CreationID, command.StepID, command.JobID, command.Capability, creations.StepSubmitStatusSucceeded, command.At)
	if err != nil {
		return err
	}
	if !matched {
		return repository.existingCompleted(ctx, command)
	}
	if err := repository.insertResultAsset(ctx, command.StepID, command.ResultURL, command.At); err != nil {
		return err
	}

	finalStep, err := repository.isFinalCallbackStep(ctx, command.CreationID, command.StepID)
	if err != nil {
		return err
	}
	if finalStep {
		return repository.markCreationSucceeded(ctx, command.CreationID, command.At)
	}
	return repository.activateSecondStep(ctx, command)
}

// Fail 先写回执，再以 submitted 状态将对应步骤和创作收敛到技术失败。
func (repository *mongoGenerationCallbackRepository) Fail(ctx context.Context, command generation.FailedCallback) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if !callbackTransactionActive(ctx) {
		return generation.ErrInvalidCallbackEvent
	}
	if !validFailedCommand(command) {
		return generation.ErrInvalidCallbackEvent
	}
	if err := repository.insertReceipt(ctx, failedReceipt(command)); err != nil {
		return err
	}

	matched, err := repository.updateTerminalStep(ctx, command.CreationID, command.StepID, command.JobID, command.Capability, creations.StepSubmitStatusGenerationFailed, command.At)
	if err != nil {
		return err
	}
	if !matched {
		return repository.existingFailed(ctx, command)
	}
	result, err := repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: command.CreationID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusGenerationFailed)},
		{Key: "updated_at", Value: command.At.UTC()},
	}}})
	if err != nil {
		return fmt.Errorf("mark generation failed creation: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	return nil
}

// providerTerminalStepDocument 是终态 CAS 的读投影：只取判定所需字段。
type providerTerminalStepDocument struct {
	CreationID               string                                  `bson:"creation_id"`
	Intent                   *model.ProviderSubmissionIntentDocument `bson:"submission_intent"`
	TerminalVersion          int64                                   `bson:"terminal_version"`
	TerminalStatus           string                                  `bson:"terminal_status"`
	TerminalResultRef        string                                  `bson:"terminal_result_ref"`
	TerminalDigest           string                                  `bson:"terminal_digest"`
	TerminalPayloadDigest    string                                  `bson:"terminal_payload_digest"`
	TerminalAttemptFence     int64                                   `bson:"terminal_attempt_fence"`
	TerminalLeaseToken       string                                  `bson:"terminal_lease_token"`
	TerminalConfirmedAt      time.Time                               `bson:"terminal_confirmed_at"`
	TerminalQuarantined      bool                                    `bson:"terminal_quarantined"`
	TerminalQuarantineReason string                                  `bson:"terminal_quarantine_reason"`
}

// providerTerminalCASAttempts 限制同一事务内的重读次数。CAS 未命中通常意味着
// 并发终态先落地，Mongo 会以 WriteConflict 让整个事务重试并拿到新快照。
const providerTerminalCASAttempts = 3

// ApplyProviderTerminal 是 B2B callback、lookup 和 inbox consumer 共享的
// 终态 CAS 适配器。旧 execution.v2 callback 仍走 Complete/Fail；新协议
// 不直接更新 submit_status，而是先经过此单槽位接口，防止 completed/failed
// 竞争和旧租约覆盖。
//
// 三种结果对应三种持久化动作，不能混：
//   - applied：CAS 写终态，并在同一事务里终结该步骤的对账事件；
//   - quarantined：只落 quarantine 证据并提交，领域冲突不算事务失败；
//   - 瞬时竞态（陈旧 fence/version）：不写任何东西并返回错误，交给调用方重试。
func (repository *mongoGenerationCallbackRepository) ApplyProviderTerminal(ctx context.Context, fact generation.ProviderTerminalFact) (generation.ProviderTerminalApplyResult, error) {
	if repository == nil || repository.steps == nil || repository.events == nil {
		return "", errors.New("provider terminal repository is not configured")
	}
	if err := fact.Validate(); err != nil {
		return "", err
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() {
		return "", errors.New("provider terminal requires an active transaction")
	}
	for attempt := 0; attempt < providerTerminalCASAttempts; attempt++ {
		slot, creationID, err := repository.readProviderTerminalSlot(ctx, fact)
		if err != nil {
			return "", err
		}
		updated, result, applyErr := generation.ApplyProviderTerminal(&slot, fact)
		switch result {
		case generation.ProviderTerminalNoop:
			return result, nil
		case generation.ProviderTerminalQuarantined:
			if err := repository.quarantineProviderTerminal(ctx, fact.StepID, slot.TerminalVersion, updated.QuarantineReason); err != nil {
				return "", err
			}
			return result, nil
		case generation.ProviderTerminalApplied:
			matched, err := repository.commitProviderTerminal(ctx, fact, slot, updated)
			if err != nil {
				return "", err
			}
			if !matched {
				// 并发终态已经推进版本：本轮不写任何东西，重读后再判定。
				continue
			}
			if updated.Status == generation.ProviderTerminalCompleted {
				if err := repository.scheduleProviderResultMaterialization(ctx, fact, creationID, updated.TerminalVersion); err != nil {
					return "", err
				}
			} else {
				if err := repository.scheduleProviderTerminalSettlement(ctx, fact, creationID, updated.TerminalVersion); err != nil {
					return "", err
				}
			}
			if err := repository.finalizeProviderReconciliation(ctx, fact, creationID, updated.TerminalVersion); err != nil {
				return "", err
			}
			return result, nil
		default:
			return "", applyErr
		}
	}
	return "", generation.ErrProviderTerminalVersion
}

func (repository *mongoGenerationCallbackRepository) readProviderTerminalSlot(ctx context.Context, fact generation.ProviderTerminalFact) (generation.ProviderTerminalSlot, string, error) {
	var document providerTerminalStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: fact.StepID},
		{Key: "provider", Value: fact.Provider},
		{Key: "account_ref", Value: fact.AccountRef},
		{Key: "external_execution_id", Value: fact.ExternalExecutionID},
		{Key: "atom", Value: fact.Capability},
	}).Decode(&document)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return generation.ProviderTerminalSlot{}, "", generation.ErrProviderTerminalConflict
		}
		return generation.ProviderTerminalSlot{}, "", fmt.Errorf("read provider terminal step: %w", err)
	}
	attemptFence := document.TerminalAttemptFence
	if attemptFence == 0 && document.Intent != nil {
		attemptFence = int64(document.Intent.Fence)
	}
	if attemptFence == 0 {
		// 没有冻结的提交尝试就没有可用 fence：宁可拒绝，也不能把一个无法
		// 归因到具体提交尝试的终态写进步骤。
		return generation.ProviderTerminalSlot{}, "", generation.ErrProviderTerminalFence
	}
	if document.CreationID == "" {
		return generation.ProviderTerminalSlot{}, "", generation.ErrProviderTerminalConflict
	}
	return generation.ProviderTerminalSlot{
		Provider: fact.Provider, AccountRef: fact.AccountRef, StepID: fact.StepID,
		ExternalExecutionID: fact.ExternalExecutionID, Capability: fact.Capability,
		AttemptFence: attemptFence, LeaseToken: document.TerminalLeaseToken,
		TerminalVersion: document.TerminalVersion, Status: generation.ProviderTerminalStatus(document.TerminalStatus),
		ResultRef:      document.TerminalResultRef,
		TerminalDigest: document.TerminalDigest, PayloadDigest: document.TerminalPayloadDigest,
		ConfirmedAt: document.TerminalConfirmedAt, Quarantined: document.TerminalQuarantined,
		QuarantineReason: document.TerminalQuarantineReason,
	}, document.CreationID, nil
}

// commitProviderTerminal 以 terminal_version + attempt_fence 为条件写终态。
// 返回 false 表示版本已被并发终态推进，调用方必须重读，不得重复写入。
func (repository *mongoGenerationCallbackRepository) commitProviderTerminal(ctx context.Context, fact generation.ProviderTerminalFact, slot, updated generation.ProviderTerminalSlot) (bool, error) {
	update := bson.M{
		"terminal_version":           updated.TerminalVersion,
		"terminal_status":            string(updated.Status),
		"terminal_result_ref":        fact.ResultRef,
		"terminal_digest":            updated.TerminalDigest,
		"terminal_payload_digest":    updated.PayloadDigest,
		"terminal_attempt_fence":     updated.AttemptFence,
		"terminal_confirmed_at":      updated.ConfirmedAt.UTC(),
		"terminal_quarantined":       false,
		"terminal_quarantine_reason": "",
	}
	if updated.LeaseToken != "" {
		update["terminal_lease_token"] = updated.LeaseToken
	}
	result, err := repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: fact.StepID},
		{Key: "provider", Value: fact.Provider},
		{Key: "account_ref", Value: fact.AccountRef},
		{Key: "external_execution_id", Value: fact.ExternalExecutionID},
		{Key: "atom", Value: fact.Capability},
		{Key: "terminal_version", Value: slot.TerminalVersion},
		{Key: "$or", Value: bson.A{
			bson.M{"terminal_attempt_fence": bson.M{"$exists": false}},
			bson.M{"terminal_attempt_fence": slot.AttemptFence},
		}},
	}, bson.D{{Key: "$set", Value: update}})
	if err != nil {
		return false, fmt.Errorf("apply provider terminal CAS: %w", err)
	}
	return result.MatchedCount == 1, nil
}

// scheduleProviderResultMaterialization creates the sole durable hand-off to
// the long-running materializer in the same transaction as the terminal CAS.
// A post-commit enqueue would leave a successfully completed provider job with
// no recoverable path to its user-visible asset after a process crash.
func (repository *mongoGenerationCallbackRepository) scheduleProviderResultMaterialization(ctx context.Context, fact generation.ProviderTerminalFact, creationID string, terminalVersion int64) error {
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(generation.ProviderResultMaterializeEventPayload{
		CreationID: creationID, StepID: fact.StepID, Provider: fact.Provider, AccountRef: fact.AccountRef,
		JobID: fact.ExternalExecutionID, Capability: fact.Capability, ResultRef: fact.ResultRef,
		TerminalVersion: terminalVersion, TerminalDigest: fact.TerminalDigest,
	})
	if err != nil {
		return generation.ErrProviderTerminalConflict
	}
	eventID := generation.ProviderResultMaterializeEventID(fact.StepID)
	if eventID == "" {
		return generation.ErrProviderTerminalConflict
	}
	var existing model.OutboxEventDocument
	err = repository.events.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&existing)
	if err == nil {
		if existing.AggregateID != creationID || existing.EventType != generation.ProviderResultMaterializeEventType || !bytes.Equal(existing.Payload, payload) {
			return generation.ErrProviderTerminalConflict
		}
		return nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("read provider result materialization event: %w", err)
	}
	_, err = repository.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: eventID, AggregateID: creationID, EventType: generation.ProviderResultMaterializeEventType, Payload: payload,
		DeliveryStatus: string(outbox.DeliveryStatusPending), AttemptCount: 0, NextAttemptAt: fact.ObservedAt.UTC(),
		CreatedAt: fact.ObservedAt.UTC(), UpdatedAt: fact.ObservedAt.UTC(),
	})
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrProviderTerminalConflict
	}
	if err != nil {
		return fmt.Errorf("insert provider result materialization event: %w", err)
	}
	return nil
}

// scheduleProviderTerminalSettlement creates the sole durable hand-off for a
// failed/cancelled B2B terminal. The follow-up worker reverses the frozen
// reservation and changes user-visible state; doing either from a callback or
// lookup transaction would make acknowledgement availability depend on billing
// work. The outbox insert must nevertheless share the terminal CAS transaction
// so a crash cannot strand a failed provider task without a settlement path.
func (repository *mongoGenerationCallbackRepository) scheduleProviderTerminalSettlement(ctx context.Context, fact generation.ProviderTerminalFact, creationID string, terminalVersion int64) error {
	payload, err := generation.MarshalProviderTerminalSettlementEventPayload(generation.ProviderTerminalSettlementEventPayload{
		CreationID: creationID, StepID: fact.StepID, Provider: fact.Provider, AccountRef: fact.AccountRef,
		JobID: fact.ExternalExecutionID, Capability: fact.Capability, Status: fact.Status,
		TerminalVersion: terminalVersion, TerminalDigest: fact.TerminalDigest,
	})
	if err != nil {
		return generation.ErrProviderTerminalConflict
	}
	eventID := generation.ProviderTerminalSettlementEventID(fact.StepID)
	if eventID == "" {
		return generation.ErrProviderTerminalConflict
	}
	var existing model.OutboxEventDocument
	err = repository.events.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&existing)
	if err == nil {
		if existing.AggregateID != creationID || existing.EventType != generation.ProviderTerminalSettlementEventType || !bytes.Equal(existing.Payload, payload) {
			return generation.ErrProviderTerminalConflict
		}
		return nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("read provider terminal settlement event: %w", err)
	}
	_, err = repository.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: eventID, AggregateID: creationID, EventType: generation.ProviderTerminalSettlementEventType, Payload: payload,
		DeliveryStatus: string(outbox.DeliveryStatusPending), AttemptCount: 0, NextAttemptAt: fact.ObservedAt.UTC(),
		CreatedAt: fact.ObservedAt.UTC(), UpdatedAt: fact.ObservedAt.UTC(),
	})
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrProviderTerminalConflict
	}
	if err != nil {
		return fmt.Errorf("insert provider terminal settlement event: %w", err)
	}
	return nil
}

// LoadProviderResultMaterialization resolves a result event back to the
// terminal slot that created it. The worker must call this before dialing the
// provider URL: a syntactically valid outbox payload is not enough evidence if
// a database record was repaired, quarantined, or otherwise no longer agrees.
func (repository *mongoGenerationCallbackRepository) LoadProviderResultMaterialization(ctx context.Context, payload generation.ProviderResultMaterializeEventPayload) (generation.ProviderResultMaterializationTarget, error) {
	if repository == nil || repository.creations == nil || repository.steps == nil {
		return generation.ProviderResultMaterializationTarget{}, errors.New("provider result materialization repository is not configured")
	}
	if !payload.Validate() {
		return generation.ProviderResultMaterializationTarget{}, generation.ErrInvalidProviderResultPublication
	}
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: payload.StepID},
		{Key: "creation_id", Value: payload.CreationID},
		{Key: "provider", Value: payload.Provider},
		{Key: "account_ref", Value: payload.AccountRef},
		{Key: "external_execution_id", Value: payload.JobID},
		{Key: "atom", Value: payload.Capability},
		{Key: "terminal_version", Value: payload.TerminalVersion},
		{Key: "terminal_status", Value: string(generation.ProviderTerminalCompleted)},
		{Key: "terminal_result_ref", Value: payload.ResultRef},
		{Key: "terminal_digest", Value: payload.TerminalDigest},
		// A conflicting terminal is evidence that requires operator resolution.
		// The originally completed event must not race ahead and publish a result
		// once the slot has been quarantined.
		{Key: "$or", Value: bson.A{
			bson.M{"terminal_quarantined": bson.M{"$exists": false}},
			bson.M{"terminal_quarantined": false},
		}},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ProviderResultMaterializationTarget{}, generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return generation.ProviderResultMaterializationTarget{}, fmt.Errorf("read provider result materialization step: %w", err)
	}
	var creation model.CreationDocument
	err = repository.creations.FindOne(ctx, bson.D{
		{Key: "_id", Value: payload.CreationID},
		{Key: "status", Value: preTerminalCreationStatusFilter()},
	}).Decode(&creation)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ProviderResultMaterializationTarget{}, generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return generation.ProviderResultMaterializationTarget{}, fmt.Errorf("read provider result materialization creation: %w", err)
	}
	stepCount, err := repository.steps.CountDocuments(ctx, bson.D{{Key: "creation_id", Value: payload.CreationID}})
	if err != nil {
		return generation.ProviderResultMaterializationTarget{}, fmt.Errorf("count provider result creation steps: %w", err)
	}
	target := generation.ProviderResultMaterializationTarget{Payload: payload, Sequence: step.Sequence}
	switch {
	case stepCount == 1 && step.Sequence == 1:
		// Single-step image/video results remain final publications.
	case stepCount == 2 && step.Sequence == 1:
		next, err := repository.loadB2BSecondStepTarget(ctx, payload, step)
		if err != nil {
			return generation.ProviderResultMaterializationTarget{}, err
		}
		target.NextB2B = next
	case stepCount == 2 && step.Sequence == 2:
		// The final video result publishes normally. The first-stage transition
		// has already consumed the deferred recipe and made this step ready.
	default:
		return generation.ProviderResultMaterializationTarget{}, generation.ErrProviderResultPublicationTopology
	}
	if err := target.Validate(); err != nil {
		return generation.ProviderResultMaterializationTarget{}, err
	}
	return target, nil
}

// MarkProviderResultUploadPending records, on the step row itself, that a
// completed provider result has not entered owned storage.
//
// It is intentionally outside any transaction: it is the compensating record
// written after the automatic path has already given up, so it must remain
// writable while the publish transaction is the thing that keeps failing. A
// single-document update is atomic in Mongo, and the marker is monotonic
// (absent/false -> true), so a retry can never clear an existing record.
//
// It deliberately never inserts an asset, never touches the reservation gate,
// and never writes TerminalResultRef anywhere user-visible.
func (repository *mongoGenerationCallbackRepository) MarkProviderResultUploadPending(ctx context.Context, raw generation.ProviderResultUploadPending) error {
	pending, err := raw.Normalize()
	if err != nil {
		return err
	}
	if repository == nil || repository.steps == nil {
		return errors.New("provider result materialization repository is not configured")
	}
	// The identity filter mirrors LoadProviderResultMaterialization: a marker on
	// a superseded or quarantined terminal would be a lie about which result is
	// still missing. submit_status != succeeded additionally keeps an already
	// published step from ever being re-flagged as awaiting upload.
	result, err := repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: pending.Payload.StepID},
		{Key: "creation_id", Value: pending.Payload.CreationID},
		{Key: "provider", Value: pending.Payload.Provider},
		{Key: "account_ref", Value: pending.Payload.AccountRef},
		{Key: "external_execution_id", Value: pending.Payload.JobID},
		{Key: "atom", Value: pending.Payload.Capability},
		{Key: "terminal_version", Value: pending.Payload.TerminalVersion},
		{Key: "terminal_status", Value: string(generation.ProviderTerminalCompleted)},
		{Key: "terminal_result_ref", Value: pending.Payload.ResultRef},
		{Key: "terminal_digest", Value: pending.Payload.TerminalDigest},
		{Key: "submit_status", Value: bson.D{{Key: "$ne", Value: string(creations.StepSubmitStatusSucceeded)}}},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "r2_upload_pending", Value: true},
		{Key: "r2_upload_pending_at", Value: pending.At},
		{Key: "r2_upload_pending_reason", Value: string(pending.Reason)},
	}}})
	if err != nil {
		return fmt.Errorf("mark provider result upload pending: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}
	return nil
}

// loadB2BSecondStepTarget resolves the exact blocked stage that may be
// activated when payload's first image has been copied to immutable R2. It is
// intentionally part of the materialization target load rather than a worker
// lookup: no current catalog or client-supplied URL may influence this graph.
func (repository *mongoGenerationCallbackRepository) loadB2BSecondStepTarget(ctx context.Context, payload generation.ProviderResultMaterializeEventPayload, first model.CreationStepDocument) (*generation.ProviderB2BSecondStepTarget, error) {
	var second model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: payload.CreationID}, {Key: "sequence", Value: int32(2)},
		{Key: "atom", Value: string(creations.AtomImageToVideo)}, {Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}).Decode(&second)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return nil, fmt.Errorf("read B2B blocked second step: %w", err)
	}
	route, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{
		Provider: second.Provider, AccountRef: second.AccountRef, ContractVersion: second.ContractVersion, MappingVersion: second.MappingVersion,
	})
	if err != nil || route.Provider != creations.PolarStarB2BProvider || route.AccountRef != payload.AccountRef ||
		first.Provider != payload.Provider || first.AccountRef != payload.AccountRef || route.ContractVersion != first.ContractVersion || route.MappingVersion != first.MappingVersion {
		return nil, generation.ErrProviderResultPublicationConflict
	}
	var recipe model.DeferredRecipeDocument
	err = repository.recipes.FindOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "step_id", Value: second.ID}, {Key: "creation_id", Value: payload.CreationID},
		{Key: "atom", Value: string(creations.AtomImageToVideo)}, {Key: "protocol", Value: string(creations.DeferredRecipeProtocolB2B)},
		{Key: "status", Value: string(creations.DeferredRecipeStatusPending)},
	}).Decode(&recipe)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return nil, fmt.Errorf("read B2B deferred second recipe: %w", err)
	}
	if recipe.ModelSKU != "" || len(recipe.InputTemplate) != 0 || len(recipe.B2BRecipe) == 0 || recipe.Digest == "" {
		return nil, generation.ErrProviderResultPublicationConflict
	}
	publicRecipe, err := creations.ParseB2BProductRecipe(recipe.B2BRecipe)
	if err != nil {
		return nil, generation.ErrProviderResultPublicationConflict
	}
	deferred, err := (creations.DeferredB2BImageToVideo{Recipe: publicRecipe, Digest: recipe.Digest}).Normalize()
	if err != nil {
		return nil, generation.ErrProviderResultPublicationConflict
	}
	return &generation.ProviderB2BSecondStepTarget{StepID: second.ID, Route: route, Deferred: deferred}, nil
}

// LoadProviderTerminalSettlement validates the durable failed/cancelled slot
// before a worker starts its settlement transaction. It is intentionally a
// read-only preflight: SettleProviderTerminal repeats this check in the
// transaction so a lease never becomes a refund authorization by itself.
func (repository *mongoGenerationCallbackRepository) LoadProviderTerminalSettlement(ctx context.Context, payload generation.ProviderTerminalSettlementEventPayload) (generation.ProviderTerminalSettlementTarget, error) {
	if repository == nil || repository.creations == nil || repository.steps == nil {
		return generation.ProviderTerminalSettlementTarget{}, errors.New("provider terminal settlement repository is not configured")
	}
	if !payload.Validate() {
		return generation.ProviderTerminalSettlementTarget{}, generation.ErrInvalidProviderTerminalSettlement
	}
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: payload.StepID},
		{Key: "creation_id", Value: payload.CreationID},
		{Key: "provider", Value: payload.Provider},
		{Key: "account_ref", Value: payload.AccountRef},
		{Key: "external_execution_id", Value: payload.JobID},
		{Key: "atom", Value: payload.Capability},
		{Key: "terminal_version", Value: payload.TerminalVersion},
		{Key: "terminal_status", Value: string(payload.Status)},
		{Key: "terminal_digest", Value: payload.TerminalDigest},
		{Key: "$or", Value: bson.A{
			bson.M{"terminal_quarantined": bson.M{"$exists": false}},
			bson.M{"terminal_quarantined": false},
		}},
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ProviderTerminalSettlementTarget{}, generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return generation.ProviderTerminalSettlementTarget{}, fmt.Errorf("read provider terminal settlement step: %w", err)
	}
	err = repository.creations.FindOne(ctx, bson.D{
		{Key: "_id", Value: payload.CreationID},
		{Key: "status", Value: preTerminalCreationStatusFilter()},
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ProviderTerminalSettlementTarget{}, generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return generation.ProviderTerminalSettlementTarget{}, fmt.Errorf("read provider terminal settlement creation: %w", err)
	}
	target := generation.ProviderTerminalSettlementTarget{Payload: payload}
	if err := target.Validate(); err != nil {
		return generation.ProviderTerminalSettlementTarget{}, err
	}
	return target, nil
}

// PublishProviderResultMaterialization atomically settles a verified, owned
// R2 result. A final step closes the reservation publication gate and exposes
// asset_kind=result; the first B2B text-to-video step instead records an
// intermediate R2 frame and atomically wakes the exact blocked second step.
// Neither branch ever persists the transient provider ResultRef as an asset.
func (repository *mongoGenerationCallbackRepository) PublishProviderResultMaterialization(ctx context.Context, raw generation.ProviderResultPublication) error {
	publication, err := raw.Normalize()
	if err != nil {
		return err
	}
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.assets == nil || repository.reservations == nil || repository.events == nil {
		return errors.New("provider result materialization repository is not configured")
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() {
		return generation.ErrProviderResultPublicationConflict
	}

	// Re-read inside the transaction. A worker can hold a lease while a human
	// or another terminal path changes the durable facts, so its earlier read
	// cannot be treated as an authorization to publish.
	target, err := repository.LoadProviderResultMaterialization(ctx, publication.Target.Payload)
	if err != nil {
		return err
	}
	if !target.Same(publication.Target) {
		return generation.ErrProviderResultPublicationConflict
	}
	count, err := repository.steps.CountDocuments(ctx, bson.D{{Key: "creation_id", Value: target.Payload.CreationID}})
	if err != nil {
		return fmt.Errorf("count provider result creation steps: %w", err)
	}
	switch {
	case count == 1 && target.Sequence == 1 && target.NextB2B == nil:
		return repository.publishFinalProviderResult(ctx, publication)
	case count == 2 && target.Sequence == 2 && target.NextB2B == nil:
		return repository.publishFinalProviderResult(ctx, publication)
	case count == 2 && target.Sequence == 1 && target.NextB2B != nil && publication.NextB2B != nil:
		return repository.activateB2BSecondStep(ctx, publication)
	default:
		return generation.ErrProviderResultPublicationTopology
	}
}

func (repository *mongoGenerationCallbackRepository) publishFinalProviderResult(ctx context.Context, publication generation.ProviderResultPublication) error {
	target := publication.Target
	if target.Sequence == 2 {
		if err := repository.verifyActivatedB2BSecondStep(ctx, target.Payload.CreationID, target.Payload.StepID); err != nil {
			return err
		}
	}
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(target.Payload)
	if err != nil {
		return generation.ErrProviderResultPublicationConflict
	}

	// The reservation is the shared publication/refund contention point. Its
	// status stays reserved after a successful paid result, but the gate is
	// closed as published so later settlement code cannot refund it.
	result, err := repository.reservations.UpdateOne(ctx, bson.D{
		{Key: "creation_id", Value: target.Payload.CreationID},
		{Key: "status", Value: "reserved"},
		{Key: "$or", Value: bson.A{
			bson.M{"publication_state": bson.M{"$exists": false}},
			bson.M{"publication_state": "open"},
		}},
	}, bson.D{
		{Key: "$inc", Value: bson.D{{Key: "publication_version", Value: int64(1)}}},
		{Key: "$set", Value: bson.D{{Key: "publication_state", Value: "published"}, {Key: "updated_at", Value: publication.PublishedAt}}},
	})
	if err != nil {
		return fmt.Errorf("close provider result publication gate: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}

	_, err = repository.assets.InsertOne(ctx, model.AssetDocument{
		ID: "asset:" + target.Payload.StepID + ":result", OwnerType: callbackAssetOwnerType, OwnerID: target.Payload.StepID,
		AssetKind: callbackAssetKindResult, StorageKey: publication.StorageURL, Status: callbackAssetStatusAvailable,
		ContentType: publication.ContentType, ByteSize: publication.ContentLength, ContentSHA256: publication.ContentSHA256,
		CreatedAt: publication.PublishedAt,
	})
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return fmt.Errorf("insert provider result asset: %w", err)
	}
	result, err = repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: target.Payload.StepID},
		{Key: "creation_id", Value: target.Payload.CreationID},
		{Key: "provider", Value: target.Payload.Provider},
		{Key: "account_ref", Value: target.Payload.AccountRef},
		{Key: "external_execution_id", Value: target.Payload.JobID},
		{Key: "atom", Value: target.Payload.Capability},
		{Key: "terminal_version", Value: target.Payload.TerminalVersion},
		{Key: "terminal_status", Value: string(generation.ProviderTerminalCompleted)},
		{Key: "terminal_digest", Value: target.Payload.TerminalDigest},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)},
	}, bson.D{
		{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusSucceeded)}}},
		// A successful re-materialization is the only proof that the provider
		// result is now owned and published. Clear all three compensating fields
		// in this same transaction, otherwise a recovered work remains falsely
		// visible to operators as r2UploadPending forever.
		{Key: "$unset", Value: bson.D{
			{Key: "r2_upload_pending", Value: ""},
			{Key: "r2_upload_pending_at", Value: ""},
			{Key: "r2_upload_pending_reason", Value: ""},
		}},
	})
	if err != nil {
		return fmt.Errorf("mark provider result step succeeded: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}
	result, err = repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: target.Payload.CreationID},
		{Key: "status", Value: preTerminalCreationStatusFilter()},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: string(creations.CreationStatusSucceeded)}, {Key: "updated_at", Value: publication.PublishedAt}}}})
	if err != nil {
		return fmt.Errorf("mark provider result creation succeeded: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}
	return repository.markProviderResultMaterializationDelivered(ctx, publication, payload)
}

// activateB2BSecondStep performs the first-half two-step transition. All
// writes remain in the materializer transaction: a crash can leave an orphan
// immutable R2 object, but can never leave a ready second step without its
// exact opening_frame payload and submission event.
func (repository *mongoGenerationCallbackRepository) activateB2BSecondStep(ctx context.Context, publication generation.ProviderResultPublication) error {
	target := publication.Target
	activation := publication.NextB2B
	if target.NextB2B == nil || activation == nil || activation.StepID != target.NextB2B.StepID || activation.Route != target.NextB2B.Route {
		return generation.ErrProviderResultPublicationConflict
	}
	payload, err := generation.MarshalProviderResultMaterializeEventPayload(target.Payload)
	if err != nil {
		return generation.ErrProviderResultPublicationConflict
	}
	deferredPayload, err := target.NextB2B.Deferred.Recipe.Marshal()
	if err != nil {
		return generation.ErrProviderResultPublicationConflict
	}
	secondPayload, err := activation.Recipe.Marshal()
	if err != nil {
		return generation.ErrProviderResultPublicationConflict
	}
	event, err := outbox.NewPending(outbox.SubmissionEventID(activation.StepID), target.Payload.CreationID, secondPayload, publication.PublishedAt)
	if err != nil {
		return generation.ErrProviderResultPublicationConflict
	}

	// The gate intentionally stays open: the charge becomes non-refundable
	// only when the final video is materialized, not when its first image is
	// copied to R2. Requiring it here prevents an activation after a refund.
	err = repository.reservations.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: target.Payload.CreationID}, {Key: "status", Value: "reserved"},
		{Key: "$or", Value: bson.A{bson.M{"publication_state": bson.M{"$exists": false}}, bson.M{"publication_state": "open"}}},
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return fmt.Errorf("read open B2B video publication gate: %w", err)
	}

	_, err = repository.assets.InsertOne(ctx, model.AssetDocument{
		ID: "asset:" + target.Payload.StepID + ":intermediate_result", OwnerType: callbackAssetOwnerType, OwnerID: target.Payload.StepID,
		AssetKind: callbackAssetKindIntermediate, StorageKey: publication.StorageURL, Status: callbackAssetStatusAvailable,
		ContentType: publication.ContentType, ByteSize: publication.ContentLength, ContentSHA256: publication.ContentSHA256,
		CreatedAt: publication.PublishedAt,
	})
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return fmt.Errorf("insert B2B opening-frame asset: %w", err)
	}

	result, err := repository.recipes.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: activation.StepID}, {Key: "step_id", Value: activation.StepID}, {Key: "creation_id", Value: target.Payload.CreationID},
		{Key: "atom", Value: string(creations.AtomImageToVideo)}, {Key: "protocol", Value: string(creations.DeferredRecipeProtocolB2B)},
		{Key: "status", Value: string(creations.DeferredRecipeStatusPending)}, {Key: "digest", Value: target.NextB2B.Deferred.Digest},
		{Key: "b2b_recipe", Value: deferredPayload},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: string(creations.DeferredRecipeStatusConsumed)}, {Key: "updated_at", Value: publication.PublishedAt}}}})
	if err != nil {
		return fmt.Errorf("consume B2B deferred second recipe: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}

	result, err = repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: target.Payload.StepID}, {Key: "creation_id", Value: target.Payload.CreationID},
		{Key: "provider", Value: target.Payload.Provider}, {Key: "account_ref", Value: target.Payload.AccountRef},
		{Key: "external_execution_id", Value: target.Payload.JobID}, {Key: "atom", Value: target.Payload.Capability},
		{Key: "terminal_version", Value: target.Payload.TerminalVersion}, {Key: "terminal_status", Value: string(generation.ProviderTerminalCompleted)},
		{Key: "terminal_digest", Value: target.Payload.TerminalDigest}, {Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)},
	}, bson.D{
		{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusSucceeded)}}},
		{Key: "$unset", Value: bson.D{
			{Key: "r2_upload_pending", Value: ""},
			{Key: "r2_upload_pending_at", Value: ""},
			{Key: "r2_upload_pending_reason", Value: ""},
		}},
	})
	if err != nil {
		return fmt.Errorf("mark B2B opening-frame step succeeded: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}

	result, err = repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: activation.StepID}, {Key: "creation_id", Value: target.Payload.CreationID}, {Key: "sequence", Value: int32(2)},
		{Key: "atom", Value: string(creations.AtomImageToVideo)}, {Key: "provider", Value: activation.Route.Provider},
		{Key: "account_ref", Value: activation.Route.AccountRef}, {Key: "contract_version", Value: activation.Route.ContractVersion},
		{Key: "mapping_version", Value: activation.Route.MappingVersion}, {Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusReady)}}}})
	if err != nil {
		return fmt.Errorf("activate B2B second generation step: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}

	_, err = repository.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: event.ID, AggregateID: event.AggregateID, EventType: string(event.EventType), Payload: event.Payload,
		DeliveryStatus: string(event.DeliveryStatus), AttemptCount: event.AttemptCount, NextAttemptAt: event.NextAttemptAt,
		CreatedAt: event.CreatedAt, UpdatedAt: event.UpdatedAt,
	})
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return fmt.Errorf("enqueue B2B second generation submission: %w", err)
	}
	if err := repository.reopenCreationAfterCompletedFirstStage(ctx, target.Payload.CreationID, publication.PublishedAt); err != nil {
		return err
	}
	return repository.markProviderResultMaterializationDelivered(ctx, publication, payload)
}

// reopenCreationAfterCompletedFirstStage resolves the only cancellation race
// that can otherwise strand a two-step video. A canonical completed first
// stage has already won the provider-terminal CAS and its frame is now safely
// owned in this transaction. If a user cancellation reached Mongo before that
// point, it must not leave the just-created second submission permanently
// blocked by CreationStatusCancelling. Pending creations are intentionally a
// no-op so the ordinary two-step path keeps its original timestamp semantics.
func (repository *mongoGenerationCallbackRepository) reopenCreationAfterCompletedFirstStage(ctx context.Context, creationID string, at time.Time) error {
	result, err := repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: creationID},
		{Key: "status", Value: string(creations.CreationStatusCancelling)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
		{Key: "updated_at", Value: at},
	}}})
	if err != nil {
		return fmt.Errorf("reopen completed B2B creation after cancellation race: %w", err)
	}
	if result.MatchedCount == 1 {
		return nil
	}
	// If no cancellation was pending, the creation must still be in its normal
	// pre-terminal state. Anything else is a terminal/publication race and must
	// abort the whole materialization transaction rather than resurrect it.
	err = repository.creations.FindOne(ctx, bson.D{
		{Key: "_id", Value: creationID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderResultPublicationConflict
	}
	if err != nil {
		return fmt.Errorf("verify reopened B2B creation: %w", err)
	}
	return nil
}

func (repository *mongoGenerationCallbackRepository) markProviderResultMaterializationDelivered(ctx context.Context, publication generation.ProviderResultPublication, payload []byte) error {
	result, err := repository.events.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: publication.EventID},
		{Key: "aggregate_id", Value: publication.Target.Payload.CreationID},
		{Key: "event_type", Value: generation.ProviderResultMaterializeEventType},
		{Key: "payload", Value: payload},
		{Key: "lease_token", Value: publication.LeaseToken},
		// Lease token alone is insufficient after a long R2 stream: no other
		// worker may have claimed it yet, while the lease has nevertheless
		// expired. Keep this fence as the final write in the transaction so all
		// preceding publication mutations roll back on loss of ownership.
		{Key: "lease_until", Value: bson.D{{Key: "$gt", Value: publication.PublishedAt}}},
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusDispatching), string(outbox.DeliveryStatusReconciling),
		}}}},
	}, bson.D{
		{Key: "$set", Value: bson.D{{Key: "delivery_status", Value: string(outbox.DeliveryStatusDelivered)}, {Key: "updated_at", Value: publication.PublishedAt}}},
		{Key: "$unset", Value: bson.D{{Key: "lease_token", Value: ""}, {Key: "lease_until", Value: ""}, {Key: "lease_owner", Value: ""}}},
	})
	if err != nil {
		return fmt.Errorf("mark provider result materialization delivered: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderResultPublicationConflict
	}
	return nil
}

// SettleProviderTerminal closes a failed/cancelled B2B terminal only after
// the surrounding transaction has reversed its reservation. It makes four
// facts atomic: the already-reversed reservation gate, the failed step, the
// failed creation and the durable settlement event. Any mismatch aborts the
// transaction; especially, a published reservation can never be refunded.
func (repository *mongoGenerationCallbackRepository) SettleProviderTerminal(ctx context.Context, raw generation.ProviderTerminalSettlement) error {
	settlement, err := raw.Normalize()
	if err != nil {
		return err
	}
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.reservations == nil || repository.events == nil {
		return errors.New("provider terminal settlement repository is not configured")
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() {
		return generation.ErrProviderTerminalSettlementConflict
	}

	// Re-read inside the write transaction. A target loaded before a callback
	// conflict, manual action or another worker settlement is not authority to
	// change a user-visible state.
	target, err := repository.LoadProviderTerminalSettlement(ctx, settlement.Target.Payload)
	if err != nil {
		return err
	}
	if target != settlement.Target {
		return generation.ErrProviderTerminalSettlementConflict
	}
	stepCount, err := repository.steps.CountDocuments(ctx, bson.D{{Key: "creation_id", Value: target.Payload.CreationID}})
	if err != nil {
		return fmt.Errorf("count provider terminal settlement steps: %w", err)
	}
	var terminalStep model.CreationStepDocument
	err = repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: target.Payload.StepID}, {Key: "creation_id", Value: target.Payload.CreationID},
		{Key: "provider", Value: target.Payload.Provider}, {Key: "account_ref", Value: target.Payload.AccountRef},
		{Key: "external_execution_id", Value: target.Payload.JobID}, {Key: "atom", Value: target.Payload.Capability},
		{Key: "terminal_version", Value: target.Payload.TerminalVersion}, {Key: "terminal_status", Value: string(target.Payload.Status)},
		{Key: "terminal_digest", Value: target.Payload.TerminalDigest}, {Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)},
	}).Decode(&terminalStep)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return fmt.Errorf("read provider terminal settlement topology: %w", err)
	}
	if stepCount != 1 && stepCount != 2 {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if stepCount == 1 && terminalStep.Sequence != 1 {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if stepCount == 2 {
		switch terminalStep.Sequence {
		case 1:
			if err := repository.verifyUnactivatedB2BSecondStep(ctx, target.Payload.CreationID, target.Payload.StepID); err != nil {
				return err
			}
		case 2:
			if err := repository.verifyActivatedB2BSecondStep(ctx, target.Payload.CreationID, target.Payload.StepID); err != nil {
				return err
			}
		default:
			return generation.ErrProviderTerminalSettlementConflict
		}
	}
	payload, err := generation.MarshalProviderTerminalSettlementEventPayload(target.Payload)
	if err != nil {
		return generation.ErrProviderTerminalSettlementConflict
	}

	// ReverseInTx has already run. Requiring its exact final gate prevents this
	// store from being used as an alternate path that marks work failed while a
	// charged reservation remains active.
	err = repository.reservations.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: target.Payload.CreationID},
		{Key: "status", Value: "reversed"},
		{Key: "publication_state", Value: "reversed"},
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return fmt.Errorf("read reversed provider terminal reservation: %w", err)
	}

	result, err := repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: target.Payload.StepID},
		{Key: "creation_id", Value: target.Payload.CreationID},
		{Key: "provider", Value: target.Payload.Provider},
		{Key: "account_ref", Value: target.Payload.AccountRef},
		{Key: "external_execution_id", Value: target.Payload.JobID},
		{Key: "atom", Value: target.Payload.Capability},
		{Key: "terminal_version", Value: target.Payload.TerminalVersion},
		{Key: "terminal_status", Value: string(target.Payload.Status)},
		{Key: "terminal_digest", Value: target.Payload.TerminalDigest},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusGenerationFailed)}}}})
	if err != nil {
		return fmt.Errorf("mark provider terminal step failed: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if stepCount == 2 && terminalStep.Sequence == 1 {
		if err := repository.terminateBlockedB2BSecondStep(ctx, target.Payload.CreationID, target.Payload.StepID, settlement.SettledAt); err != nil {
			return err
		}
	}
	result, err = repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: target.Payload.CreationID},
		{Key: "status", Value: preTerminalCreationStatusFilter()},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusGenerationFailed)},
		{Key: "updated_at", Value: settlement.SettledAt},
	}}})
	if err != nil {
		return fmt.Errorf("mark provider terminal creation failed: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderTerminalSettlementConflict
	}
	result, err = repository.events.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: settlement.EventID},
		{Key: "aggregate_id", Value: target.Payload.CreationID},
		{Key: "event_type", Value: generation.ProviderTerminalSettlementEventType},
		{Key: "payload", Value: payload},
		{Key: "lease_token", Value: settlement.LeaseToken},
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusDispatching), string(outbox.DeliveryStatusReconciling),
		}}}},
	}, bson.D{
		{Key: "$set", Value: bson.D{{Key: "delivery_status", Value: string(outbox.DeliveryStatusDelivered)}, {Key: "updated_at", Value: settlement.SettledAt}}},
		{Key: "$unset", Value: bson.D{{Key: "lease_token", Value: ""}, {Key: "lease_until", Value: ""}, {Key: "lease_owner", Value: ""}}},
	})
	if err != nil {
		return fmt.Errorf("mark provider terminal settlement delivered: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderTerminalSettlementConflict
	}
	return nil
}

// verifyUnactivatedB2BSecondStep proves that a failed first B2B image has no
// runnable descendant. A caller cannot use terminal settlement to reverse one
// charge while retaining a ready/queued second provider task.
func (repository *mongoGenerationCallbackRepository) verifyUnactivatedB2BSecondStep(ctx context.Context, creationID, firstStepID string) error {
	var second model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: creationID}, {Key: "sequence", Value: int32(2)}, {Key: "atom", Value: string(creations.AtomImageToVideo)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}).Decode(&second)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return fmt.Errorf("read blocked B2B second step for settlement: %w", err)
	}
	var recipe model.DeferredRecipeDocument
	err = repository.recipes.FindOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "step_id", Value: second.ID}, {Key: "creation_id", Value: creationID},
		{Key: "protocol", Value: string(creations.DeferredRecipeProtocolB2B)}, {Key: "status", Value: string(creations.DeferredRecipeStatusPending)},
	}).Decode(&recipe)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return fmt.Errorf("read pending B2B deferred recipe for settlement: %w", err)
	}
	if recipe.StepID != second.ID || recipe.ModelSKU != "" || len(recipe.InputTemplate) != 0 || len(recipe.B2BRecipe) == 0 || recipe.Digest == "" {
		return generation.ErrProviderTerminalSettlementConflict
	}
	count, err := repository.events.CountDocuments(ctx, bson.D{{Key: "_id", Value: outbox.SubmissionEventID(second.ID)}})
	if err != nil {
		return fmt.Errorf("count premature B2B second submission event: %w", err)
	}
	if count != 0 || second.ID == firstStepID {
		return generation.ErrProviderTerminalSettlementConflict
	}
	return nil
}

// verifyActivatedB2BSecondStep ensures a second-stage terminal can be settled
// only after the first stage succeeded and its submission event was durably
// bound/delivered. This protects the graph from a hand-crafted terminal that
// skips the R2 opening-frame activation transaction.
func (repository *mongoGenerationCallbackRepository) verifyActivatedB2BSecondStep(ctx context.Context, creationID, secondStepID string) error {
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: creationID}, {Key: "sequence", Value: int32(1)}, {Key: "atom", Value: string(creations.AtomTextToImage)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSucceeded)},
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return fmt.Errorf("read succeeded B2B first step for settlement: %w", err)
	}
	err = repository.events.FindOne(ctx, bson.D{
		{Key: "_id", Value: outbox.SubmissionEventID(secondStepID)}, {Key: "aggregate_id", Value: creationID},
		{Key: "event_type", Value: string(outbox.EventTypeGenerationSubmission)}, {Key: "delivery_status", Value: string(outbox.DeliveryStatusDelivered)},
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return fmt.Errorf("read delivered B2B second submission event for settlement: %w", err)
	}
	return nil
}

func (repository *mongoGenerationCallbackRepository) terminateBlockedB2BSecondStep(ctx context.Context, creationID, firstStepID string, at time.Time) error {
	var second model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: creationID}, {Key: "sequence", Value: int32(2)}, {Key: "atom", Value: string(creations.AtomImageToVideo)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}).Decode(&second)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrProviderTerminalSettlementConflict
	}
	if err != nil {
		return fmt.Errorf("re-read blocked B2B second step for termination: %w", err)
	}
	result, err := repository.recipes.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "step_id", Value: second.ID}, {Key: "creation_id", Value: creationID},
		{Key: "protocol", Value: string(creations.DeferredRecipeProtocolB2B)}, {Key: "status", Value: string(creations.DeferredRecipeStatusPending)},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: string(creations.DeferredRecipeStatusAbandoned)}, {Key: "updated_at", Value: at.UTC()}}}})
	if err != nil {
		return fmt.Errorf("abandon blocked B2B deferred recipe: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderTerminalSettlementConflict
	}
	result, err = repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "creation_id", Value: creationID},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusGenerationFailed)}}}})
	if err != nil {
		return fmt.Errorf("terminate blocked B2B second step: %w", err)
	}
	if result.MatchedCount != 1 || second.ID == firstStepID {
		return generation.ErrProviderTerminalSettlementConflict
	}
	return nil
}

// quarantineProviderTerminal 只写冲突证据，不改动已生效的终态字段。
func (repository *mongoGenerationCallbackRepository) quarantineProviderTerminal(ctx context.Context, stepID string, expectedVersion int64, reason string) error {
	result, err := repository.steps.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: stepID}, {Key: "terminal_version", Value: expectedVersion}},
		bson.D{{Key: "$set", Value: bson.M{"terminal_quarantined": true, "terminal_quarantine_reason": reason}}})
	if err != nil {
		return fmt.Errorf("quarantine provider terminal: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderTerminalVersion
	}
	return nil
}

// finalizeProviderReconciliation 在同一事务里终结该步骤的对账事件：标记已投递、
// 记录终结版本并清掉租约，使旧 lookup 租约无法再把事件改回可领取状态。
// 事件身份不符时返回错误让整个事务回滚，避免终态已写而对账仍活着。
func (repository *mongoGenerationCallbackRepository) finalizeProviderReconciliation(ctx context.Context, fact generation.ProviderTerminalFact, creationID string, terminalVersion int64) error {
	eventID := generation.ProviderInboxRecoveryEventID(fact.StepID)
	if eventID == "" {
		return generation.ErrInvalidProviderTerminal
	}
	payload, err := generation.MarshalProviderReconcileEventPayload(creationID, fact.StepID)
	if err != nil {
		return generation.ErrProviderTerminalConflict
	}
	result, err := repository.events.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: eventID}, {Key: "aggregate_id", Value: creationID}, {Key: "event_type", Value: generation.ProviderReconcileEventType}, {Key: "payload", Value: payload}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "delivery_status", Value: string(outbox.DeliveryStatusDelivered)},
				{Key: "terminal_version", Value: terminalVersion},
				{Key: "updated_at", Value: fact.ObservedAt.UTC()},
			}},
			{Key: "$unset", Value: bson.D{
				{Key: "lease_token", Value: ""},
				{Key: "lease_until", Value: ""},
				{Key: "lease_owner", Value: ""},
			}},
		})
	if err != nil {
		return fmt.Errorf("finalize provider reconciliation: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderTerminalConflict
	}
	return nil
}

type callbackReceipt struct {
	nonceHash       string
	payloadDigest   string
	creationID      string
	stepID          string
	externalRef     string
	jobID           string
	capability      executionv2.Capability
	terminal        string
	mediaType       string
	resultURL       string
	callbackVersion string
	at              time.Time
}

func completedReceipt(command generation.CompletedCallback) callbackReceipt {
	return callbackReceipt{nonceHash: command.NonceHash, payloadDigest: command.PayloadDigest, creationID: command.CreationID, stepID: command.StepID, externalRef: command.ExternalRef, jobID: command.JobID, capability: command.Capability, terminal: "completed", mediaType: command.MediaType, resultURL: command.ResultURL, callbackVersion: command.CallbackVersion, at: command.At}
}

func failedReceipt(command generation.FailedCallback) callbackReceipt {
	return callbackReceipt{nonceHash: command.NonceHash, payloadDigest: command.PayloadDigest, creationID: command.CreationID, stepID: command.StepID, externalRef: command.ExternalRef, jobID: command.JobID, capability: command.Capability, terminal: string(command.Terminal), callbackVersion: command.CallbackVersion, at: command.At}
}

func (repository *mongoGenerationCallbackRepository) insertReceipt(ctx context.Context, receipt callbackReceipt) error {
	result, err := repository.receipts.UpdateOne(ctx, bson.D{
		{Key: "source", Value: generationCallbackReceiptSource},
		{Key: "nonce_hash", Value: receipt.nonceHash},
	}, bson.D{{Key: "$setOnInsert", Value: model.CallbackReceiptDocument{
		ID:              "generation.callback:" + receipt.nonceHash,
		Source:          generationCallbackReceiptSource,
		NonceHash:       receipt.nonceHash,
		ReceivedAt:      receipt.at.UTC(),
		PayloadDigest:   receipt.payloadDigest,
		CreationID:      receipt.creationID,
		StepID:          receipt.stepID,
		ExternalRef:     receipt.externalRef,
		JobID:           receipt.jobID,
		Capability:      string(receipt.capability),
		Terminal:        receipt.terminal,
		MediaType:       receipt.mediaType,
		ResultURL:       receipt.resultURL,
		CallbackVersion: receipt.callbackVersion,
	}}}, options.UpdateOne().SetUpsert(true))
	if err == nil {
		if result.UpsertedCount == 1 {
			return nil
		}
	} else if !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("upsert generation callback receipt: %w", err)
	}
	var existing model.CallbackReceiptDocument
	err = repository.receipts.FindOne(ctx, bson.D{
		{Key: "source", Value: generationCallbackReceiptSource},
		{Key: "nonce_hash", Value: receipt.nonceHash},
	}).Decode(&existing)
	if err != nil {
		return fmt.Errorf("read existing generation callback receipt: %w", err)
	}
	if !sameCallbackReceipt(existing, receipt) {
		return generation.ErrInvalidCallbackEvent
	}
	return generation.ErrCallbackAlreadyConfirmed
}

func (repository *mongoGenerationCallbackRepository) updateTerminalStep(ctx context.Context, creationID, stepID, jobID string, capability executionv2.Capability, status creations.StepSubmitStatus, atTime time.Time) (bool, error) {
	result, err := repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: stepID},
		{Key: "creation_id", Value: creationID},
		{Key: "external_execution_id", Value: jobID},
		{Key: "atom", Value: callbackAtom(capability)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSubmitted)},
		{Key: "callback_version", Value: int64(0)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "submit_status", Value: string(status)},
		{Key: "callback_version", Value: callbackVersionV2},
	}}})
	if err != nil {
		return false, fmt.Errorf("compare-and-set callback step at %s: %w", atTime.UTC().Format(time.RFC3339), err)
	}
	return result.MatchedCount == 1, nil
}

func (repository *mongoGenerationCallbackRepository) insertResultAsset(ctx context.Context, stepID, resultURL string, atTime time.Time) error {
	_, err := repository.assets.InsertOne(ctx, model.AssetDocument{
		ID: "asset:" + stepID + ":result", OwnerType: callbackAssetOwnerType, OwnerID: stepID,
		AssetKind: callbackAssetKindResult, StorageKey: resultURL, Status: callbackAssetStatusAvailable, CreatedAt: atTime.UTC(),
	})
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("insert callback result asset: %w", err)
}

func (repository *mongoGenerationCallbackRepository) isFinalCallbackStep(ctx context.Context, creationID, stepID string) (bool, error) {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{{Key: "_id", Value: stepID}, {Key: "creation_id", Value: creationID}}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return false, fmt.Errorf("read callback step topology: %w", err)
	}
	count, err := repository.steps.CountDocuments(ctx, bson.D{{Key: "creation_id", Value: creationID}, {Key: "sequence", Value: bson.D{{Key: "$gt", Value: step.Sequence}}}})
	if err != nil {
		return false, fmt.Errorf("count callback creation steps: %w", err)
	}
	return count == 0, nil
}

func (repository *mongoGenerationCallbackRepository) markCreationSucceeded(ctx context.Context, creationID string, atTime time.Time) error {
	result, err := repository.creations.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: creationID},
		{Key: "status", Value: string(creations.CreationStatusPendingSubmission)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.CreationStatusSucceeded)},
		{Key: "updated_at", Value: atTime.UTC()},
	}}})
	if err != nil {
		return fmt.Errorf("mark generation succeeded creation: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	return nil
}

// preTerminalCreationStatusFilter is shared by B2B materialization and
// settlement paths.  Cancelling is an accepted request state, not a terminal:
// a callback/lookup terminal still has to be able to win the existing
// publication/ledger CAS.  Do not include any final state here.
func preTerminalCreationStatusFilter() bson.D {
	return bson.D{{Key: "$in", Value: bson.A{
		string(creations.CreationStatusPendingSubmission),
		string(creations.CreationStatusCancelling),
	}}}
}

func (repository *mongoGenerationCallbackRepository) activateSecondStep(ctx context.Context, command generation.CompletedCallback) error {
	var second model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "creation_id", Value: command.CreationID},
		{Key: "sequence", Value: int32(2)},
		{Key: "atom", Value: string(creations.AtomImageToVideo)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}).Decode(&second)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read blocked second step: %w", err)
	}
	var recipe model.DeferredRecipeDocument
	err = repository.recipes.FindOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "step_id", Value: second.ID},
		{Key: "creation_id", Value: command.CreationID}, {Key: "atom", Value: string(creations.AtomImageToVideo)},
		{Key: "status", Value: string(creations.DeferredRecipeStatusPending)},
		// Empty protocol is the explicit legacy-read compatibility window. A
		// b2b.job.v2 recipe is activated only by the R2 materializer: feeding
		// its public-product bytes into execution.v2 would otherwise both
		// corrupt the second request and bypass the owned-opening-frame gate.
		{Key: "$or", Value: bson.A{
			bson.M{"protocol": bson.M{"$exists": false}},
			bson.M{"protocol": string(creations.DeferredRecipeProtocolExecutionV2)},
		}},
	}).Decode(&recipe)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read deferred recipe: %w", err)
	}
	if recipe.Protocol != "" && recipe.Protocol != string(creations.DeferredRecipeProtocolExecutionV2) || len(recipe.B2BRecipe) != 0 {
		return generation.ErrInvalidCallbackEvent
	}
	deferred, err := executionv2.CompileDeferredImageToVideo(recipe.ModelSKU, recipe.InputTemplate)
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}
	if deferred.Digest != recipe.Digest {
		return generation.ErrInvalidCallbackEvent
	}
	snapshot, err := deferred.BindOpeningFrame(command.ResultURL)
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}
	payload, err := snapshot.MarshalSubmissionPayload()
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}
	event, err := outbox.NewPending(outbox.SubmissionEventID(second.ID), command.CreationID, payload, command.At.UTC())
	if err != nil {
		return generation.ErrInvalidCallbackEvent
	}

	result, err := repository.recipes.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "status", Value: string(creations.DeferredRecipeStatusPending)},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "status", Value: string(creations.DeferredRecipeStatusConsumed)}, {Key: "updated_at", Value: command.At.UTC()},
	}}})
	if err != nil {
		return fmt.Errorf("consume deferred recipe: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	result, err = repository.steps.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: second.ID}, {Key: "creation_id", Value: command.CreationID},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusBlocked)},
	}, bson.D{{Key: "$set", Value: bson.D{{Key: "submit_status", Value: string(creations.StepSubmitStatusReady)}}}})
	if err != nil {
		return fmt.Errorf("activate second generation step: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrInvalidCallbackEvent
	}
	_, err = repository.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: event.ID, AggregateID: event.AggregateID, EventType: string(event.EventType), Payload: event.Payload,
		DeliveryStatus: string(event.DeliveryStatus), AttemptCount: event.AttemptCount, NextAttemptAt: event.NextAttemptAt,
		CreatedAt: event.CreatedAt, UpdatedAt: event.UpdatedAt,
	})
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("enqueue second generation submission: %w", err)
}

func (repository *mongoGenerationCallbackRepository) existingCompleted(ctx context.Context, command generation.CompletedCallback) error {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: command.StepID}, {Key: "creation_id", Value: command.CreationID},
		{Key: "external_execution_id", Value: command.JobID}, {Key: "atom", Value: callbackAtom(command.Capability)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusSucceeded)}, {Key: "callback_version", Value: callbackVersionV2},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read existing completed callback step: %w", err)
	}
	var receipt model.CallbackReceiptDocument
	err = repository.receipts.FindOne(ctx, receiptFactFilter(completedReceipt(command), command.NonceHash)).Decode(&receipt)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read existing completed callback receipt: %w", err)
	}
	var asset model.AssetDocument
	err = repository.assets.FindOne(ctx, bson.D{
		{Key: "_id", Value: "asset:" + command.StepID + ":result"},
		{Key: "owner_type", Value: callbackAssetOwnerType}, {Key: "owner_id", Value: command.StepID},
		{Key: "asset_kind", Value: callbackAssetKindResult}, {Key: "storage_key", Value: command.ResultURL},
		{Key: "status", Value: callbackAssetStatusAvailable},
	}).Decode(&asset)
	if err == nil {
		return generation.ErrCallbackAlreadyConfirmed
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("read existing callback result asset: %w", err)
}

func (repository *mongoGenerationCallbackRepository) existingFailed(ctx context.Context, command generation.FailedCallback) error {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{
		{Key: "_id", Value: command.StepID}, {Key: "creation_id", Value: command.CreationID},
		{Key: "external_execution_id", Value: command.JobID}, {Key: "atom", Value: callbackAtom(command.Capability)},
		{Key: "submit_status", Value: string(creations.StepSubmitStatusGenerationFailed)}, {Key: "callback_version", Value: callbackVersionV2},
	}).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	if err != nil {
		return fmt.Errorf("read existing failed callback step: %w", err)
	}
	var receipt model.CallbackReceiptDocument
	err = repository.receipts.FindOne(ctx, receiptFactFilter(failedReceipt(command), command.NonceHash)).Decode(&receipt)
	if err == nil {
		return generation.ErrCallbackAlreadyConfirmed
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return generation.ErrInvalidCallbackEvent
	}
	return fmt.Errorf("read existing failed callback receipt: %w", err)
}

func (repository *mongoGenerationCallbackRepository) ready() error {
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.recipes == nil || repository.assets == nil || repository.receipts == nil || repository.events == nil {
		return errors.New("generation callback repository is not configured")
	}
	return nil
}

func callbackTransactionActive(ctx context.Context) bool {
	session := mongo.SessionFromContext(ctx)
	return session != nil && session.TransactionRunning()
}

func sameCallbackReceipt(existing model.CallbackReceiptDocument, expected callbackReceipt) bool {
	return existing.Source == generationCallbackReceiptSource && existing.NonceHash == expected.nonceHash &&
		existing.PayloadDigest == expected.payloadDigest && existing.CreationID == expected.creationID && existing.StepID == expected.stepID &&
		existing.ExternalRef == expected.externalRef && existing.JobID == expected.jobID && existing.Capability == string(expected.capability) &&
		existing.Terminal == expected.terminal && existing.MediaType == expected.mediaType && existing.ResultURL == expected.resultURL && existing.CallbackVersion == expected.callbackVersion
}

func receiptFactFilter(receipt callbackReceipt, excludeNonceHash string) bson.D {
	filter := bson.D{
		{Key: "source", Value: generationCallbackReceiptSource}, {Key: "payload_digest", Value: receipt.payloadDigest},
		{Key: "creation_id", Value: receipt.creationID}, {Key: "step_id", Value: receipt.stepID}, {Key: "external_ref", Value: receipt.externalRef},
		{Key: "job_id", Value: receipt.jobID}, {Key: "capability", Value: string(receipt.capability)}, {Key: "terminal", Value: receipt.terminal},
		{Key: "media_type", Value: receipt.mediaType}, {Key: "result_url", Value: receipt.resultURL}, {Key: "callback_version", Value: receipt.callbackVersion},
	}
	if excludeNonceHash != "" {
		filter = append(filter, bson.E{Key: "nonce_hash", Value: bson.D{{Key: "$ne", Value: excludeNonceHash}}})
	}
	return filter
}

func validCompletedCommand(command generation.CompletedCallback) bool {
	return validCallbackBase(command.CreationID, command.StepID, command.JobID, command.ExternalRef, command.Capability, command.NonceHash, command.PayloadDigest, command.CallbackVersion, command.At) &&
		callbackMediaMatches(command.Capability, command.MediaType) && callbackResultURL(command.ResultURL)
}

func validFailedCommand(command generation.FailedCallback) bool {
	return validCallbackBase(command.CreationID, command.StepID, command.JobID, command.ExternalRef, command.Capability, command.NonceHash, command.PayloadDigest, command.CallbackVersion, command.At) &&
		(command.Terminal == generation.CallbackTerminalFailed || command.Terminal == generation.CallbackTerminalCancelled)
}

func validCallbackBase(creationID, stepID, jobID, externalRef string, capability executionv2.Capability, nonceHash, payloadDigest, version string, atTime time.Time) bool {
	return callbackIdentifier(creationID) && callbackIdentifier(stepID) && callbackIdentifier(jobID) && externalRef == stepID &&
		callbackIdentifier(nonceHash) && callbackIdentifier(payloadDigest) && version == "2" && !atTime.IsZero() && callbackCapabilityMatchesAtom(capability, "")
}

func callbackIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= 512
}

func callbackCapabilityMatchesAtom(capability executionv2.Capability, atom string) bool {
	expected := callbackAtom(capability)
	return expected != "" && (atom == "" || atom == expected)
}

func callbackAtom(capability executionv2.Capability) string {
	switch capability {
	case executionv2.CapabilityTextToImage:
		return string(creations.AtomTextToImage)
	case executionv2.CapabilityImageEdit:
		return string(creations.AtomImageEdit)
	case executionv2.CapabilityImageToVideo:
		return string(creations.AtomImageToVideo)
	default:
		return ""
	}
}

func callbackMediaMatches(capability executionv2.Capability, mediaType string) bool {
	if capability == executionv2.CapabilityImageToVideo {
		return mediaType == "video"
	}
	return mediaType == "image"
}

func callbackResultURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && raw == strings.TrimSpace(raw) && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

var _ generation.CallbackStore = (*mongoGenerationCallbackRepository)(nil)
var _ generation.CallbackLinker = (*mongoGenerationCallbackRepository)(nil)
var _ generation.ProviderResultMaterializationStore = (*mongoGenerationCallbackRepository)(nil)
var _ generation.ProviderTerminalSettlementStore = (*mongoGenerationCallbackRepository)(nil)
