package data

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoOutboxRepository struct {
	collection *mongo.Collection
}

// NewOutboxRepository 返回发件箱事件仓储的领域接口实现。
func NewOutboxRepository(data *Data) outbox.Repository {
	return &mongoOutboxRepository{collection: data.database.Collection(schema.CollectionOutboxEvents)}
}

func (repository *mongoOutboxRepository) Enqueue(ctx context.Context, event *outbox.Event) error {
	if err := outbox.ValidatePending(event); err != nil {
		return err
	}

	_, err := repository.collection.InsertOne(ctx, newOutboxEventDocument(event))
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return outbox.ErrEventAlreadyExists
	}
	return fmt.Errorf("enqueue outbox event: %w", err)
}

func (repository *mongoOutboxRepository) Claim(ctx context.Context, workerID string, now, leaseUntil time.Time) (*outbox.Event, error) {
	return repository.claim(ctx, workerID, "", "", now, leaseUntil)
}

// ClaimByType 只领取指定事件类型，防止本工作者占用其他工作者的事件租约。
func (repository *mongoOutboxRepository) ClaimByType(ctx context.Context, workerID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	if eventType == "" {
		return nil, outbox.ErrInvalidEvent
	}
	return repository.claim(ctx, workerID, "", eventType, now, leaseUntil)
}

// ClaimByIDAndType 原子领取指定类型的指定事件，避免单次投递占用无关事件。
func (repository *mongoOutboxRepository) ClaimByIDAndType(ctx context.Context, workerID, eventID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	if !isValidOutboxIdentifier(eventID) || eventType == "" {
		return nil, outbox.ErrInvalidEvent
	}
	return repository.claim(ctx, workerID, eventID, eventType, now, leaseUntil)
}

// RenewLease only extends a lease that is still live and still held by the
// supplied token. It intentionally leaves payload, attempt_count and all
// frozen routing facts untouched.
func (repository *mongoOutboxRepository) RenewLease(ctx context.Context, eventID, leaseToken string, now, leaseUntil time.Time) error {
	if !isValidOutboxIdentifier(eventID) || !isValidOutboxIdentifier(leaseToken) || now.IsZero() || !leaseUntil.After(now) {
		return outbox.ErrInvalidEvent
	}
	result, err := repository.collection.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: eventID},
		{Key: "lease_token", Value: leaseToken},
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusDispatching), string(outbox.DeliveryStatusReconciling),
		}}}},
		{Key: "lease_until", Value: bson.D{{Key: "$gt", Value: now}}},
	}, bson.D{{Key: "$set", Value: bson.D{
		{Key: "lease_until", Value: leaseUntil},
		{Key: "updated_at", Value: now},
	}}})
	if err != nil {
		return fmt.Errorf("renew outbox event lease: %w", err)
	}
	if result.MatchedCount != 1 {
		return outbox.ErrLeaseConflict
	}
	return nil
}

func (repository *mongoOutboxRepository) claim(ctx context.Context, workerID, eventID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	if !isValidOutboxIdentifier(workerID) || now.IsZero() || !leaseUntil.After(now) {
		return nil, outbox.ErrInvalidEvent
	}

	leaseToken, err := newOutboxLeaseToken()
	if err != nil {
		return nil, fmt.Errorf("generate outbox lease token: %w", err)
	}
	filter := bson.D{
		// 可领取集合刻意只有这三个状态：needs_attention 与 failed、delivered 一样
		// 是终态。若把 needs_attention 放进来，预算耗尽的事件会被无限重新领取，
		// 等于重试预算从未生效。
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusPending),
			string(outbox.DeliveryStatusReconciling),
			string(outbox.DeliveryStatusDispatching),
		}}}},
		{Key: "next_attempt_at", Value: bson.D{{Key: "$lte", Value: now}}},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "lease_until", Value: bson.D{{Key: "$exists", Value: false}}}},
			bson.D{{Key: "lease_until", Value: nil}},
			bson.D{{Key: "lease_until", Value: bson.D{{Key: "$lte", Value: now}}}},
		}},
	}
	if eventType != "" {
		filter = append(filter, bson.E{Key: "event_type", Value: string(eventType)})
	}
	if eventID != "" {
		filter = append(filter, bson.E{Key: "_id", Value: eventID})
	}

	var document model.OutboxEventDocument
	// 接管已过期的 dispatching 事件时保留 reconciling，避免崩溃恢复后再次 POST。
	updatedStatus := bson.D{{Key: "$cond", Value: bson.A{
		bson.D{{Key: "$eq", Value: bson.A{"$delivery_status", string(outbox.DeliveryStatusDispatching)}}},
		string(outbox.DeliveryStatusReconciling),
		string(outbox.DeliveryStatusDispatching),
	}}}
	err = repository.collection.FindOneAndUpdate(
		ctx,
		filter,
		bson.A{
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "delivery_status", Value: updatedStatus},
				{Key: "lease_token", Value: leaseToken},
				{Key: "lease_until", Value: leaseUntil},
				{Key: "lease_owner", Value: workerID},
				{Key: "updated_at", Value: now},
				{Key: "attempt_count", Value: bson.D{{Key: "$add", Value: bson.A{"$attempt_count", int32(1)}}}},
			}}},
		},
		options.FindOneAndUpdate().SetSort(bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}).SetReturnDocument(options.After),
	).Decode(&document)
	if err == nil {
		return toBizOutboxEvent(document), nil
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return nil, fmt.Errorf("claim outbox event: %w", err)
}

func (repository *mongoOutboxRepository) Requeue(ctx context.Context, eventID, leaseToken string, nextAttemptAt time.Time) error {
	if !isValidOutboxIdentifier(eventID) || !isValidOutboxIdentifier(leaseToken) || nextAttemptAt.IsZero() {
		return outbox.ErrInvalidEvent
	}

	return repository.updateClaimed(ctx, eventID, leaseToken, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "delivery_status", Value: string(outbox.DeliveryStatusPending)},
			{Key: "next_attempt_at", Value: nextAttemptAt},
			{Key: "last_error", Value: outbox.RequeueLastError},
			{Key: "updated_at", Value: nextAttemptAt},
		}},
		{Key: "$unset", Value: bson.D{
			{Key: "lease_token", Value: ""},
			{Key: "lease_until", Value: ""},
			{Key: "lease_owner", Value: ""},
		}},
	})
}

func (repository *mongoOutboxRepository) MarkDelivered(ctx context.Context, eventID, leaseToken string, now time.Time) error {
	if !isValidOutboxIdentifier(eventID) || !isValidOutboxIdentifier(leaseToken) || now.IsZero() {
		return outbox.ErrInvalidEvent
	}

	return repository.updateClaimed(ctx, eventID, leaseToken, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "delivery_status", Value: string(outbox.DeliveryStatusDelivered)},
			{Key: "updated_at", Value: now},
		}},
		{Key: "$unset", Value: bson.D{
			{Key: "lease_token", Value: ""},
			{Key: "lease_until", Value: ""},
			{Key: "lease_owner", Value: ""},
		}},
	})
}

func (repository *mongoOutboxRepository) MarkFailed(ctx context.Context, eventID, leaseToken string, now time.Time) error {
	if !isValidOutboxIdentifier(eventID) || !isValidOutboxIdentifier(leaseToken) || now.IsZero() {
		return outbox.ErrInvalidEvent
	}

	return repository.updateClaimed(ctx, eventID, leaseToken, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "delivery_status", Value: string(outbox.DeliveryStatusFailed)},
			{Key: "updated_at", Value: now},
		}},
		{Key: "$unset", Value: bson.D{
			{Key: "lease_token", Value: ""},
			{Key: "lease_until", Value: ""},
			{Key: "lease_owner", Value: ""},
		}},
	})
}

// MarkNeedsAttention 把已领取事件转成人工关注终态。
//
// 它刻意不触碰 last_error：Requeue 每轮都会把 last_error 重写成固定摘要，
// 关注原因必须写进独立字段才能在重新入队的历史之后继续存活。
func (repository *mongoOutboxRepository) MarkNeedsAttention(ctx context.Context, eventID, leaseToken string, reason outbox.AttentionReason, now time.Time) error {
	if !isValidOutboxIdentifier(eventID) || !isValidOutboxIdentifier(leaseToken) || !outbox.ValidAttentionReason(reason) || now.IsZero() {
		return outbox.ErrInvalidEvent
	}

	return repository.updateClaimed(ctx, eventID, leaseToken, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "delivery_status", Value: string(outbox.DeliveryStatusNeedsAttention)},
			{Key: "attention_reason", Value: string(reason)},
			{Key: "updated_at", Value: now},
		}},
		{Key: "$unset", Value: bson.D{
			{Key: "lease_token", Value: ""},
			{Key: "lease_until", Value: ""},
			{Key: "lease_owner", Value: ""},
		}},
	})
}

// FindForRedrive 读取事件用于人工重驱的前置校验。
//
// 「不存在」按冲突处理：操作者看到的事件已经不在，重驱一个不存在的事实没有意义，
// 也不应该被当成「资格不足」而给出可重试的假象。
func (repository *mongoOutboxRepository) FindForRedrive(ctx context.Context, eventID string) (*outbox.Event, error) {
	if !isValidOutboxIdentifier(eventID) {
		return nil, outbox.ErrInvalidRedriveCommand
	}
	var document model.OutboxEventDocument
	err := repository.collection.FindOne(ctx, bson.D{{Key: "_id", Value: eventID}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, outbox.ErrRedriveConflict
	}
	if err != nil {
		return nil, fmt.Errorf("find outbox event for redrive: %w", err)
	}
	return toBizOutboxEvent(document), nil
}

// RedriveAttention 是唯一能让事件离开 needs_attention 的操作。
//
// 三步都在调用方传入的事务里完成：读当前文档做资格判断 → 以
// {_id, delivery_status, redrive_count} 为条件做 CAS → 返回更新后的文档。
//
// 更新语句里没有 payload、attempt_count、created_at：重驱只重新打开事件，
// 不改写任何冻结事实。redrive_attempt_base 取自在 attention 状态下不会被改动的
// attempt_count（该状态不在任何可领取集合里，因此先读再写是安全的）。
func (repository *mongoOutboxRepository) RedriveAttention(ctx context.Context, command outbox.RedriveCommand) (*outbox.Event, error) {
	if err := command.Validate(); err != nil {
		return nil, err
	}
	var current model.OutboxEventDocument
	err := repository.collection.FindOne(ctx, bson.D{{Key: "_id", Value: command.EventID}}).Decode(&current)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, outbox.ErrRedriveConflict
	}
	if err != nil {
		return nil, fmt.Errorf("read outbox event for redrive: %w", err)
	}
	event := toBizOutboxEvent(current)
	if event.DeliveryStatus != outbox.DeliveryStatusNeedsAttention {
		return nil, outbox.ErrRedriveNotEligible
	}
	if event.RedriveCount != command.ExpectedRedriveCount {
		// 操作者看到的次数已过期（别人刚重驱过）。这是冲突而不是资格问题：
		// 请求本身没问题，只是前提已经不成立。
		return nil, outbox.ErrRedriveConflict
	}
	if !event.RedriveEligible() {
		return nil, outbox.ErrRedriveNotEligible
	}

	at := command.At.UTC()
	// redrive_count 带 omitempty：从未重驱过的事件根本没有这个字段，而 MongoDB 的
	// {redrive_count: 0} 不匹配「字段缺失」。因此期望次数为 0 时必须同时接受缺失。
	countMatch := bson.D{{Key: "redrive_count", Value: command.ExpectedRedriveCount}}
	if command.ExpectedRedriveCount == 0 {
		countMatch = bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "redrive_count", Value: int32(0)}},
			bson.D{{Key: "redrive_count", Value: bson.D{{Key: "$exists", Value: false}}}},
		}}}
	}

	var updated model.OutboxEventDocument
	err = repository.collection.FindOneAndUpdate(ctx, bson.D{
		{Key: "_id", Value: command.EventID},
		{Key: "delivery_status", Value: string(outbox.DeliveryStatusNeedsAttention)},
		countMatch[0],
	}, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "delivery_status", Value: string(outbox.DeliveryStatusPending)},
			{Key: "next_attempt_at", Value: at},
			{Key: "redrive_started_at", Value: at},
			{Key: "redrive_attempt_base", Value: event.AttemptCount},
			{Key: "updated_at", Value: at},
		}},
		{Key: "$inc", Value: bson.D{{Key: "redrive_count", Value: int32(1)}}},
		{Key: "$unset", Value: bson.D{{Key: "attention_reason", Value: ""}}},
	}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&updated)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, outbox.ErrRedriveConflict
	}
	if err != nil {
		return nil, fmt.Errorf("redrive outbox event: %w", err)
	}
	return toBizOutboxEvent(updated), nil
}

func (repository *mongoOutboxRepository) updateClaimed(ctx context.Context, eventID, leaseToken string, update bson.D) error {
	result, err := repository.collection.UpdateOne(ctx, bson.D{
		{Key: "_id", Value: eventID},
		{Key: "lease_token", Value: leaseToken},
		{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: bson.A{
			string(outbox.DeliveryStatusDispatching),
			string(outbox.DeliveryStatusReconciling),
		}}}},
	}, update)
	if err != nil {
		return fmt.Errorf("update outbox event: %w", err)
	}
	if result.MatchedCount != 1 {
		return outbox.ErrLeaseConflict
	}
	return nil
}

func newOutboxEventDocument(event *outbox.Event) model.OutboxEventDocument {
	return model.OutboxEventDocument{
		ID:              event.ID,
		AggregateID:     event.AggregateID,
		EventType:       string(event.EventType),
		Payload:         append([]byte(nil), event.Payload...),
		DeliveryStatus:  string(event.DeliveryStatus),
		AttemptCount:    event.AttemptCount,
		NextAttemptAt:   event.NextAttemptAt,
		LeaseToken:      event.LeaseToken,
		LeaseUntil:      event.LeaseUntil,
		LeaseOwner:      event.LeaseOwner,
		LastError:       event.LastError,
		AttentionReason: string(event.AttentionReason),
		// 窗口与次数必须往返映射：漏掉任一字段都会让重驱后的事件在持久化时
		// 丢掉新窗口，从而在下一次失败时立刻回到 needs_attention。
		RedriveStartedAt:   event.RedriveStartedAt,
		RedriveAttemptBase: event.RedriveAttemptBase,
		RedriveCount:       event.RedriveCount,
		CreatedAt:          event.CreatedAt,
		UpdatedAt:          event.UpdatedAt,
	}
}

func toBizOutboxEvent(document model.OutboxEventDocument) *outbox.Event {
	return &outbox.Event{
		ID:                 document.ID,
		AggregateID:        document.AggregateID,
		EventType:          outbox.EventType(document.EventType),
		Payload:            append([]byte(nil), document.Payload...),
		DeliveryStatus:     outbox.DeliveryStatus(document.DeliveryStatus),
		AttemptCount:       document.AttemptCount,
		NextAttemptAt:      document.NextAttemptAt,
		LeaseToken:         document.LeaseToken,
		LeaseUntil:         document.LeaseUntil,
		LeaseOwner:         document.LeaseOwner,
		LastError:          document.LastError,
		AttentionReason:    outbox.AttentionReason(document.AttentionReason),
		RedriveStartedAt:   document.RedriveStartedAt,
		RedriveAttemptBase: document.RedriveAttemptBase,
		RedriveCount:       document.RedriveCount,
		CreatedAt:          document.CreatedAt,
		UpdatedAt:          document.UpdatedAt,
	}
}

func newOutboxLeaseToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func isValidOutboxIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 512
}

var _ outbox.Repository = (*mongoOutboxRepository)(nil)
