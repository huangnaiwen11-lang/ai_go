package data

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/ledger"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// BindProviderJob 在同一个 Mongo transaction 中把 PolarStar 返回的稳定任务
// 标识绑定到步骤和提交事件。所有身份字段均参与条件更新；因此旧租约即便在
// 网络调用完成后才返回，也不能覆盖新租约已经绑定的任务。
func (r *mongoGenerationSubmissionRepository) BindProviderJob(ctx context.Context, raw generation.ProviderJobBinding) error {
	binding, err := raw.Normalize()
	if err != nil {
		return err
	}
	if err := r.ready(); err != nil {
		return err
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() {
		return errors.New("provider job binding requires an active transaction")
	}

	eventFilter := bson.M{
		"_id": binding.EventID, "aggregate_id": binding.CreationID,
		"event_type":      string(outbox.EventTypeGenerationSubmission),
		"delivery_status": bson.M{"$in": bson.A{string(outbox.DeliveryStatusDispatching), string(outbox.DeliveryStatusReconciling)}},
		"lease_token":     binding.LeaseToken, "lease_owner": binding.LeaseOwner,
		"attempt_count": binding.Fence, "lease_until": bson.M{"$gt": binding.At},
		"job_id": bson.M{"$exists": false},
	}
	var event model.OutboxEventDocument
	if err := r.events.FindOne(ctx, eventFilter).Decode(&event); err != nil {
		return providerStorageError(err)
	}

	stepFilter := bson.M{
		"_id": binding.StepID, "creation_id": binding.CreationID,
		"provider": binding.Route.Provider, "account_ref": binding.Route.AccountRef,
		"contract_version": binding.Route.ContractVersion, "mapping_version": binding.Route.MappingVersion,
		"atom":                  binding.Capability,
		"submit_status":         bson.M{"$in": bson.A{"submitting", string(creations.StepSubmitStatusReconciling)}},
		"external_execution_id": bson.M{"$exists": false},
	}
	result, err := r.steps.UpdateOne(ctx, stepFilter, bson.M{"$set": bson.M{
		"submit_status":         string(creations.StepSubmitStatusSubmitted),
		"external_execution_id": binding.ExternalID,
		"updated_at":            binding.At,
	}})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return generation.ErrProviderJobBindingConflict
		}
		return fmt.Errorf("bind provider job to creation step: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderJobBindingConflict
	}

	result, err = r.events.UpdateOne(ctx, eventFilter, bson.M{
		"$set":   bson.M{"delivery_status": string(outbox.DeliveryStatusDelivered), "job_id": binding.ExternalID, "updated_at": binding.At},
		"$unset": bson.M{"lease_token": "", "lease_until": "", "lease_owner": ""},
	})
	if err != nil {
		return fmt.Errorf("mark provider job event delivered: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderJobBindingConflict
	}

	// PrepareProviderSubmission 已写入稳定 recovery 事件；成功绑定后立即
	// 唤醒它，使故障恢复 worker 能尽快核对回调/状态，而不会等待原租约超时。
	// 这不是“按 ID 盲写”：同一事件由 ACK 与终态路径共用，aggregate/payload
	// 任一不符都意味着已有事实损坏，必须回滚本次绑定而不是继续运行。
	recoveryPayload, err := generation.MarshalProviderReconcileEventPayload(binding.CreationID, binding.StepID)
	if err != nil {
		return generation.ErrProviderJobBindingConflict
	}
	result, err = r.events.UpdateOne(ctx, bson.M{
		"_id":             generation.ProviderInboxRecoveryEventID(binding.StepID),
		"aggregate_id":    binding.CreationID,
		"event_type":      generation.ProviderReconcileEventType,
		"payload":         recoveryPayload,
		"delivery_status": bson.M{"$in": bson.A{string(outbox.DeliveryStatusPending), string(outbox.DeliveryStatusReconciling)}},
	}, bson.M{"$set": bson.M{"next_attempt_at": binding.At, "updated_at": binding.At}})
	if err != nil {
		return fmt.Errorf("wake provider job reconciliation: %w", err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrProviderJobBindingConflict
	}
	// A cancellation may have been accepted while Submit's HTTP result was
	// unknown and the external job ID was not yet available.  Binding is the
	// first durable moment at which a provider Cancel request can be addressed,
	// so schedule it in this same transaction.  If the creation is not
	// cancelling this is deliberately a no-op.
	if err := r.scheduleBoundProviderCancellation(ctx, binding); err != nil {
		return err
	}
	return nil
}

// scheduleBoundProviderCancellation creates the unique cancellation delivery
// only after the job binding and creation status agree.  It never changes a
// terminal, reservation or submission intent; the provider callback/lookup
// remains the sole authority for those facts.
func (r *mongoGenerationSubmissionRepository) scheduleBoundProviderCancellation(ctx context.Context, binding generation.ProviderJobBinding) error {
	err := r.creations.FindOne(ctx, bson.M{
		"_id": binding.CreationID, "status": string(creations.CreationStatusCancelling),
	}).Err()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read cancelling creation after provider binding: %w", err)
	}
	payload, err := generation.MarshalProviderCancelEventPayload(generation.ProviderCancelEventPayload{
		CreationID: binding.CreationID, StepID: binding.StepID, Provider: binding.Route.Provider,
		AccountRef: binding.Route.AccountRef, JobID: binding.ExternalID, Capability: binding.Capability,
	})
	if err != nil {
		return generation.ErrProviderJobBindingConflict
	}
	eventID := generation.ProviderCancelEventID(binding.StepID)
	if eventID == "" {
		return generation.ErrProviderJobBindingConflict
	}
	_, err = r.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: eventID, AggregateID: binding.CreationID, EventType: generation.ProviderCancelEventType, Payload: payload,
		DeliveryStatus: string(outbox.DeliveryStatusPending), AttemptCount: 0, NextAttemptAt: binding.At,
		CreatedAt: binding.At, UpdatedAt: binding.At,
	})
	if err == nil {
		return nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("insert provider cancel after binding: %w", err)
	}
	var existing model.OutboxEventDocument
	if readErr := r.events.FindOne(ctx, bson.M{"_id": eventID}).Decode(&existing); readErr != nil {
		return fmt.Errorf("read provider cancel after duplicate binding: %w", readErr)
	}
	if existing.AggregateID != binding.CreationID || existing.EventType != generation.ProviderCancelEventType || !bytes.Equal(existing.Payload, payload) {
		return generation.ErrProviderJobBindingConflict
	}
	return nil
}

// PrepareProviderSubmission atomically grants the first submission intent and
// persists its recovery event. It performs no HTTP calls. Even the same command
// cannot obtain a second grant after commit; ambiguous commit means lookup only.
func (r *mongoGenerationSubmissionRepository) PrepareProviderSubmission(ctx context.Context, c generation.PrepareProviderSubmissionCommand) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := r.ready(); err != nil {
		return err
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() || r.reservations == nil {
		return errors.New("provider submission requires an active transaction")
	}
	var event model.OutboxEventDocument
	leaseFilter := bson.M{"_id": c.EventID, "aggregate_id": c.CreationID, "event_type": string(outbox.EventTypeGenerationSubmission),
		"delivery_status": string(outbox.DeliveryStatusDispatching), "lease_token": c.LeaseToken, "lease_owner": c.LeaseOwner, "attempt_count": c.Fence, "lease_until": bson.M{"$gt": c.At}, "job_id": bson.M{"$exists": false}}
	if err := r.events.FindOne(ctx, leaseFilter).Decode(&event); err != nil {
		return providerStorageError(err)
	}
	// Write the lease record, not just read it: a concurrent reclaim must conflict.
	result, err := r.events.UpdateOne(ctx, leaseFilter, bson.M{"$set": bson.M{"request_digest": c.Request.Digest, "updated_at": c.At.UTC()}})
	if err != nil {
		return providerStorageError(err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	var creation model.CreationDocument
	if err := r.creations.FindOne(ctx, bson.M{"_id": c.CreationID, "status": string(creations.CreationStatusPendingSubmission)}).Decode(&creation); err != nil {
		return providerStorageError(err)
	}
	// Submission permission participates in the same reservation write boundary as
	// refund. R8 final publication/refund gate transitions are a later stage.
	result, err = r.reservations.UpdateOne(ctx, bson.M{"creation_id": c.CreationID, "status": string(ledger.ReservationStatusReserved),
		"$or": bson.A{bson.M{"publication_state": bson.M{"$exists": false}}, bson.M{"publication_state": "open"}}},
		bson.M{"$inc": bson.M{"publication_version": int64(1)}, "$set": bson.M{"publication_state": "open"}})
	if err != nil {
		return providerStorageError(err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	req := c.Request
	intent := model.ProviderSubmissionIntentDocument{Payload: append([]byte(nil), req.Payload...), Digest: req.Digest, IdempotencyKey: req.IdempotencyKey, PreparedAt: c.At.UTC(), Fence: c.Fence}
	result, err = r.steps.UpdateOne(ctx, bson.M{"_id": req.StepID, "creation_id": c.CreationID, "atom": req.Capability,
		"provider": req.Route.Provider, "account_ref": req.Route.AccountRef, "contract_version": req.Route.ContractVersion, "mapping_version": req.Route.MappingVersion,
		"submit_status": string(creations.StepSubmitStatusReady), "external_execution_id": bson.M{"$exists": false}, "submission_intent": bson.M{"$exists": false},
		"$or": bson.A{bson.M{"terminal_version": bson.M{"$exists": false}}, bson.M{"terminal_version": int64(0)}}},
		bson.M{"$set": bson.M{"submission_intent": intent, "submit_status": "submitting", "terminal_version": int64(0)}})
	if err != nil {
		return providerStorageError(err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	// Recovery has no job ID yet. Its immutable identity is the frozen request;
	// a duplicate ID is a conflict and rolls back all writes, never swallowed.
	payload, err := generation.MarshalProviderReconcileEventPayload(c.CreationID, req.StepID)
	if err != nil {
		return generation.ErrInvalidSubmissionCommand
	}
	_, err = r.events.InsertOne(ctx, model.OutboxEventDocument{ID: generation.ProviderInboxRecoveryEventID(req.StepID), AggregateID: c.CreationID, EventType: generation.ProviderReconcileEventType, Payload: payload,
		DeliveryStatus: string(outbox.DeliveryStatusPending), NextAttemptAt: event.LeaseUntil, CreatedAt: c.At.UTC(), UpdatedAt: c.At.UTC()})
	return providerStorageError(err)
}

// ReauthorizeProviderSubmission 原子地消费唯一一次「重新授权提交」并写下审计事实。
//
// 三重条件缺一不可：
//   - 冻结意图存在：没有它就没有可重发的字节；
//   - 从未绑定外部任务号：绑过说明任务已被受理，不需要也不允许重发；
//   - 从未重新授权过：额度只有一次。
//
// 事件租约与围栏同样参与匹配：过期租约即便在查询返回之后才回来，也不能消费额度，
// 否则「唯一一次」会被并行重试悄悄花掉。
func (r *mongoGenerationSubmissionRepository) ReauthorizeProviderSubmission(ctx context.Context, c generation.ReauthorizeProviderSubmissionCommand) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := r.ready(); err != nil {
		return err
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() || r.reservations == nil {
		return errors.New("provider submission reauthorization requires an active transaction")
	}
	// 必须写租约记录：仅快照读取不能与并发回收租约形成写冲突。
	leaseFilter := bson.M{
		"_id": c.EventID, "aggregate_id": c.CreationID,
		"event_type":      string(outbox.EventTypeGenerationSubmission),
		"delivery_status": bson.M{"$in": bson.A{string(outbox.DeliveryStatusDispatching), string(outbox.DeliveryStatusReconciling)}},
		"lease_token":     c.LeaseToken, "lease_owner": c.LeaseOwner,
		"attempt_count": c.Fence, "lease_until": bson.M{"$gt": c.At},
		"job_id": bson.M{"$exists": false},
	}
	result, err := r.events.UpdateOne(ctx, leaseFilter, bson.M{"$inc": bson.M{"reauthorization_version": int64(1)}})
	if err != nil {
		return providerStorageError(err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrSubmissionConflict
	}
	intent, err := r.ReadProviderSubmission(ctx, c.StepID)
	if err != nil {
		return err
	}
	if intent == nil || intent.Request.Route.Provider != creations.PolarStarB2BProvider {
		return generation.ErrSubmissionConflict
	}
	// 与首次提交和退款写同一预留文档，保证提交权限与退款不能同时获准。
	result, err = r.reservations.UpdateOne(ctx, bson.M{"creation_id": c.CreationID, "status": string(ledger.ReservationStatusReserved),
		"$or": bson.A{bson.M{"publication_state": bson.M{"$exists": false}}, bson.M{"publication_state": "open"}}},
		bson.M{"$inc": bson.M{"publication_version": int64(1)}, "$set": bson.M{"publication_state": "open"}})
	if err != nil {
		return providerStorageError(err)
	}
	if result.MatchedCount != 1 {
		return generation.ErrReauthorizationExhausted
	}
	// 审计写入用 $set 而非 $push：字段存在本身就是「额度已用完」的判据，
	// 数组会把「至多一次」退化成「只受并发窗口限制」。
	result, err = r.steps.UpdateOne(ctx, bson.M{
		"_id": c.StepID, "creation_id": c.CreationID,
		"provider": intent.Request.Route.Provider, "account_ref": intent.Request.Route.AccountRef,
		"contract_version": intent.Request.Route.ContractVersion, "mapping_version": intent.Request.Route.MappingVersion,
		"submit_status":        bson.M{"$in": bson.A{"submitting", string(creations.StepSubmitStatusReconciling)}},
		"terminal_quarantined": bson.M{"$ne": true},
		"$and": bson.A{
			bson.M{"$or": bson.A{bson.M{"terminal_version": bson.M{"$exists": false}}, bson.M{"terminal_version": int64(0)}}},
			bson.M{"$or": bson.A{bson.M{"terminal_status": bson.M{"$exists": false}}, bson.M{"terminal_status": ""}}},
		},
		"submission_intent":                 bson.M{"$exists": true},
		"submission_intent.reauthorization": bson.M{"$exists": false},
		"external_execution_id":             bson.M{"$exists": false},
	}, bson.M{"$set": bson.M{
		"submission_intent.reauthorization": model.ProviderReauthorizationDocument{At: c.At.UTC(), Reason: c.Reason, Fence: c.Fence},
		"updated_at":                        c.At.UTC(),
	}})
	if err != nil {
		return providerStorageError(err)
	}
	if result.MatchedCount != 1 {
		// 条件不成立只有两种可能：额度已用完，或冻结事实已变（已绑定/已被改写）。
		// 两者都必须回到对账、绝不重发，因此归一到同一个哨兵。
		return generation.ErrReauthorizationExhausted
	}
	return nil
}

// ReadProviderSubmission returns persisted facts only, never permission to POST.
func (r *mongoGenerationSubmissionRepository) ReadProviderSubmission(ctx context.Context, stepID string) (*generation.ProviderSubmissionIntent, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if outbox.SubmissionEventID(stepID) == "" {
		return nil, generation.ErrInvalidSubmissionCommand
	}
	var doc struct {
		model.CreationStepDocument `bson:",inline"`
		Intent                     *model.ProviderSubmissionIntentDocument `bson:"submission_intent"`
		TerminalVersion            int64                                   `bson:"terminal_version"`
		TerminalStatus             string                                  `bson:"terminal_status"`
		TerminalQuarantined        bool                                    `bson:"terminal_quarantined"`
	}
	if err := r.steps.FindOne(ctx, bson.M{"_id": stepID}).Decode(&doc); err != nil {
		return nil, providerStorageError(err)
	}
	if doc.Intent == nil {
		route, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{Provider: doc.Provider, AccountRef: doc.AccountRef, ContractVersion: doc.ContractVersion, MappingVersion: doc.MappingVersion})
		if err == nil && route.Provider == creations.PolarStarB2BProvider && doc.CreationID != "" &&
			doc.SubmitStatus == string(creations.StepSubmitStatusReady) && doc.ExternalExecutionID == "" &&
			doc.TerminalVersion == 0 && doc.TerminalStatus == "" && !doc.TerminalQuarantined {
			// 尚未领取提交授权；不能把领取次数当成已发出 HTTP 的证据。
			// 真正的授权仍由 PrepareProviderSubmission 的事务 CAS 决定。
			return nil, nil
		}
		return nil, generation.ErrSubmissionConflict
	}
	i := doc.Intent
	req := generation.FrozenProviderRequest{Route: creations.ExecutionRoute{Provider: doc.Provider, AccountRef: doc.AccountRef, ContractVersion: doc.ContractVersion, MappingVersion: doc.MappingVersion},
		StepID: stepID, Capability: doc.Atom, IdempotencyKey: i.IdempotencyKey, Digest: i.Digest, Payload: append([]byte(nil), i.Payload...)}
	if req.Validate() != nil || doc.CreationID == "" || i.PreparedAt.IsZero() || i.Fence <= 0 {
		return nil, generation.ErrSubmissionConflict
	}
	// 外部任务号可以为空：授权已发出但结果未知是合法状态，对账路径据此决定
	// 是「按外部任务号核对」还是「等提交路径先绑定」。它的形状已由 BindProviderJob
	// 在写入时校验过，这里不再重复判一遍。
	return &generation.ProviderSubmissionIntent{Request: req, PreparedAt: i.PreparedAt, Fence: i.Fence, ExternalExecutionID: doc.ExternalExecutionID}, nil
}

// NewGenerationProviderSubmissionStore 把 Mongo 提交仓储收窄为「读回冻结提交意图」
// 这一条边界。
//
// 它与 NewGenerationSubmissionRepository 返回同一个实现，但暴露的方法集不同：
// B2B 消费路径只需要读回已冻结的意图，不该顺带拿到提交状态机的写能力。
func NewGenerationProviderSubmissionStore(data *Data) generation.ProviderSubmissionStore {
	repository, ok := NewGenerationSubmissionRepository(data).(*mongoGenerationSubmissionRepository)
	if !ok {
		return nil
	}
	return repository
}

func providerStorageError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, mongo.ErrNoDocuments) || mongo.IsDuplicateKeyError(err) {
		return generation.ErrSubmissionConflict
	}
	return fmt.Errorf("provider submission storage: %w", err)
}

var _ generation.ProviderSubmissionStore = (*mongoGenerationSubmissionRepository)(nil)
var _ generation.ProviderJobBinder = (*mongoGenerationSubmissionRepository)(nil)

// 重新授权是可选能力：它不并入 ProviderSubmissionStore，因为只读回冻结意图的
// 装配不应该顺带拿到重发能力。断言在这里保证生产实现确实提供它。
var _ generation.ProviderReauthorizationStore = (*mongoGenerationSubmissionRepository)(nil)
