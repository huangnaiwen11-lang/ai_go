package data

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// mongoGenerationProviderInboxRepository 持久化 B2B delivery 去重事实，
// 并在同一事务中确保两条恢复工作项存在：
//   - 按 receipt 去重的 generation.inbox.consume（每条 delivery 一份）；
//   - 按步骤去重的 generation.reconcile（唤醒 lookup 对账，每个步骤一份）。
//
// 两条工作项粒度不同，不能共用一个 ID：否则同一步骤的第二条 delivery 会
// 因为第一条已经 ACK 而拿不到可消费的事件。
type mongoGenerationProviderInboxRepository struct {
	inbox  *mongo.Collection
	events *mongo.Collection
	steps  *mongo.Collection
}

// NewGenerationProviderInboxRepository 返回独立 inbox 仓储。
func NewGenerationProviderInboxRepository(data *Data) generation.ProviderInboxStore {
	if data == nil || data.database == nil {
		return &mongoGenerationProviderInboxRepository{}
	}
	return &mongoGenerationProviderInboxRepository{
		inbox:  data.database.Collection(schema.CollectionGenerationProviderInbox),
		events: data.database.Collection(schema.CollectionOutboxEvents),
		steps:  data.database.Collection(schema.CollectionCreationSteps),
	}
}

func (r *mongoGenerationProviderInboxRepository) ApplyAndSchedule(ctx context.Context, incoming generation.ProviderInboxRecord) (generation.ProviderInboxApplyResult, error) {
	// 入站只允许 pending；这里再挡一次，避免调用方绕过领域入口直接落库。
	if err := incoming.ValidateIncomingDelivery(); err != nil {
		return "", err
	}
	if r == nil || r.inbox == nil || r.events == nil || r.steps == nil {
		return "", errors.New("generation provider inbox repository is not configured")
	}
	session := mongo.SessionFromContext(ctx)
	if session == nil || !session.TransactionRunning() {
		return "", errors.New("provider inbox requires an active transaction")
	}
	creationID, err := r.validateIncomingProviderDelivery(ctx, incoming)
	if err != nil {
		return "", err
	}

	key := providerInboxKeyFilter(generation.ProviderInboxKey{Source: incoming.Source, AccountRef: incoming.AccountRef, DeliveryID: incoming.DeliveryID})
	result := generation.InboxApplyInserted
	// stored 是本次 ACK 之后 inbox 里真实保存的 delivery 事实。恢复事件的身份
	// 必须按它校验，而不是按入站原文：入站可能与已保存事实冲突，此时事件属于
	// 已保存的那条 delivery，不能被入站内容改写。
	var stored model.ProviderInboxDocument
	readErr := r.inbox.FindOne(ctx, key).Decode(&stored)
	switch {
	case readErr == nil:
		merged, mergeErr := r.mergeInbox(ctx, stored, incoming)
		if mergeErr != nil {
			return "", mergeErr
		}
		result = merged
	case errors.Is(readErr, mongo.ErrNoDocuments):
		document := providerInboxDomainToDocument(incoming)
		if _, insertErr := r.inbox.InsertOne(ctx, document); insertErr != nil {
			// E11000 同样会中止当前事务，不能在这个 session 里重读后 ACK。
			// 保留原错误供整笔事务重试或供应商后续重投恢复。
			return "", fmt.Errorf("insert provider inbox: %w", insertErr)
		} else {
			stored = document
		}
	default:
		return "", fmt.Errorf("read provider inbox: %w", readErr)
	}

	// 先写本条 delivery 的消费事件，再唤醒步骤级对账。任一步身份冲突都会
	// 返回错误，使整笔事务（含上面的 inbox 写入）一起回滚，ACK 不发出。
	if err := r.ensureInboxEvent(ctx, stored); err != nil {
		return "", err
	}
	if err := r.ensureReconcileEvent(ctx, stored, creationID); err != nil {
		return "", err
	}
	return result, nil
}

// mergeInbox 用纯领域规则合并已有 delivery 事实。冲突只落 quarantine 证据：
// 已保存的 payload 与时间戳保持不变，不可信的新 payload 不写进原记录。
func (r *mongoGenerationProviderInboxRepository) mergeInbox(ctx context.Context, existing model.ProviderInboxDocument, incoming generation.ProviderInboxRecord) (generation.ProviderInboxApplyResult, error) {
	current, err := providerInboxDocumentToDomain(existing)
	if err != nil {
		return "", err
	}
	merged, mergeResult, mergeErr := generation.ApplyProviderInbox(&current, incoming)
	if mergeResult == generation.InboxApplyQuarantined {
		if _, updateErr := r.inbox.UpdateOne(ctx, bson.D{{Key: "_id", Value: existing.ID}}, bson.D{{Key: "$set", Value: bson.M{
			"status":     string(merged.Status),
			"updated_at": merged.UpdatedAt.UTC(),
		}}}); updateErr != nil {
			return "", fmt.Errorf("quarantine provider inbox: %w", updateErr)
		}
		return mergeResult, nil
	}
	if mergeErr != nil {
		return "", mergeErr
	}
	return mergeResult, nil
}

// ensureInboxEvent 为一条已验签 delivery 建立消费工作项。ID 由 receipt 身份
// 派生，因此重复 delivery 命中同一条；已有事件必须身份完全一致，否则拒绝
// ACK，而不是静默复用别的 delivery 的事件。
func (r *mongoGenerationProviderInboxRepository) ensureInboxEvent(ctx context.Context, stored model.ProviderInboxDocument) error {
	eventID := generation.ProviderInboxEventID(stored.Source, stored.AccountRef, stored.DeliveryID)
	if eventID == "" {
		return generation.ErrInvalidProviderInboxRecord
	}
	payload := marshalProviderInboxRecovery(stored)
	var existing model.OutboxEventDocument
	readErr := r.events.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&existing)
	if readErr == nil {
		if existing.EventType != generation.ProviderInboxConsumeEventType || existing.AggregateID != stored.StepID || !bytes.Equal(existing.Payload, payload) {
			return generation.ErrProviderInboxConflict
		}
		// 已投递的事件不重新打开：ACK 只负责让事实可被消费一次。
		return nil
	}
	if !errors.Is(readErr, mongo.ErrNoDocuments) {
		return fmt.Errorf("read provider inbox event: %w", readErr)
	}
	now := stored.UpdatedAt.UTC()
	_, err := r.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: eventID, AggregateID: stored.StepID, EventType: generation.ProviderInboxConsumeEventType, Payload: payload,
		DeliveryStatus: string(outbox.DeliveryStatusPending), AttemptCount: 0, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return generation.ErrProviderInboxConflict
		}
		return fmt.Errorf("insert provider inbox event: %w", err)
	}
	return nil
}

// ensureReconcileEvent 保证该步骤存在一条按步骤去重的对账唤醒事件。
// 只核对事件类型：该事件的 aggregate_id 在提交意图与 inbox 两条生产者之间
// 语义不同（创作 ID / 步骤 ID），这里不替它们做统一。
func (r *mongoGenerationProviderInboxRepository) ensureReconcileEvent(ctx context.Context, stored model.ProviderInboxDocument, creationID string) error {
	eventID := generation.ProviderInboxRecoveryEventID(stored.StepID)
	if eventID == "" {
		return generation.ErrInvalidProviderInboxRecord
	}
	payload, err := generation.MarshalProviderReconcileEventPayload(creationID, stored.StepID)
	if err != nil {
		return err
	}
	var existing model.OutboxEventDocument
	readErr := r.events.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&existing)
	if readErr == nil {
		if existing.EventType != generation.ProviderReconcileEventType || existing.AggregateID != creationID || !bytes.Equal(existing.Payload, payload) {
			return generation.ErrProviderInboxConflict
		}
		return nil
	}
	if !errors.Is(readErr, mongo.ErrNoDocuments) {
		return fmt.Errorf("read provider reconciliation event: %w", readErr)
	}
	now := stored.UpdatedAt.UTC()
	_, err = r.events.InsertOne(ctx, model.OutboxEventDocument{
		ID: eventID, AggregateID: creationID, EventType: generation.ProviderReconcileEventType, Payload: payload,
		DeliveryStatus: string(outbox.DeliveryStatusPending), AttemptCount: 0, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return generation.ErrProviderInboxConflict
		}
		return fmt.Errorf("insert provider reconciliation event: %w", err)
	}
	return nil
}

// validateIncomingProviderDelivery 检查一条已验签的报文能否归属到本地的冻结 B2B
// 步骤。允许 external_execution_id 尚未绑定：供应商的 callback 可以比 Submit
// 响应更早到达；但一旦已绑定，job ID 必须严格相同。
func (r *mongoGenerationProviderInboxRepository) validateIncomingProviderDelivery(ctx context.Context, incoming generation.ProviderInboxRecord) (string, error) {
	var document struct {
		model.CreationStepDocument `bson:",inline"`
		Intent                     *model.ProviderSubmissionIntentDocument `bson:"submission_intent"`
	}
	if err := r.steps.FindOne(ctx, bson.D{{Key: "_id", Value: incoming.StepID}}).Decode(&document); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return "", generation.ErrProviderInboxStepMismatch
		}
		return "", fmt.Errorf("read provider inbox step: %w", err)
	}
	route, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{
		Provider: document.Provider, AccountRef: document.AccountRef,
		ContractVersion: document.ContractVersion, MappingVersion: document.MappingVersion,
	})
	if err != nil || route.Provider != creations.PolarStarB2BProvider || route.AccountRef != incoming.AccountRef ||
		document.CreationID == "" || document.Intent == nil ||
		(document.ExternalExecutionID != "" && document.ExternalExecutionID != incoming.JobID) {
		return "", generation.ErrProviderInboxStepMismatch
	}
	request := generation.FrozenProviderRequest{
		Route: route, StepID: document.ID, Capability: document.Atom,
		IdempotencyKey: document.Intent.IdempotencyKey, Digest: document.Intent.Digest,
		Payload: append([]byte(nil), document.Intent.Payload...),
	}
	if request.Validate() != nil || document.Intent.PreparedAt.IsZero() || document.Intent.Fence <= 0 {
		return "", generation.ErrProviderInboxStepMismatch
	}
	return document.CreationID, nil
}

func marshalProviderInboxRecovery(stored model.ProviderInboxDocument) []byte {
	// 消费摘要是可变的本地结果，不属于已持久化 delivery 的事件身份。
	// 结构体只含字符串字段，json.Marshal 不可能失败；忽略错误好过用一个
	// 永不触发的错误分支掩盖字段改动。
	payload, _ := json.Marshal(struct {
		Source        string `json:"source"`
		AccountRef    string `json:"accountRef"`
		DeliveryID    string `json:"deliveryId"`
		StepID        string `json:"stepId"`
		JobID         string `json:"jobId"`
		PayloadDigest string `json:"payloadDigest"`
	}{stored.Source, stored.AccountRef, stored.DeliveryID, stored.StepID, stored.JobID, stored.PayloadDigest})
	return payload
}

func providerInboxDomainToDocument(record generation.ProviderInboxRecord) model.ProviderInboxDocument {
	digest := generation.ProviderInboxDeliveryDigest(record.Source, record.AccountRef, record.DeliveryID)
	return model.ProviderInboxDocument{ID: "generation.inbox:" + digest, Source: record.Source, AccountRef: record.AccountRef, DeliveryID: record.DeliveryID, StepID: record.StepID, JobID: record.JobID, TerminalDigest: record.TerminalDigest, PayloadDigest: record.PayloadDigest, Payload: append([]byte(nil), record.Payload...), Status: string(record.Status), Attempts: record.Attempts, CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC()}
}

func providerInboxDocumentToDomain(document model.ProviderInboxDocument) (generation.ProviderInboxRecord, error) {
	record := generation.ProviderInboxRecord{Source: document.Source, AccountRef: document.AccountRef, DeliveryID: document.DeliveryID, StepID: document.StepID, JobID: document.JobID, TerminalDigest: document.TerminalDigest, PayloadDigest: document.PayloadDigest, Payload: append([]byte(nil), document.Payload...), Status: generation.ProviderInboxStatus(document.Status), Attempts: document.Attempts, CreatedAt: document.CreatedAt, UpdatedAt: document.UpdatedAt}
	if err := record.Validate(); err != nil {
		return generation.ProviderInboxRecord{}, err
	}
	return record, nil
}

var _ generation.ProviderInboxStore = (*mongoGenerationProviderInboxRepository)(nil)
