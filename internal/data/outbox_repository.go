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

func (repository *mongoOutboxRepository) claim(ctx context.Context, workerID, eventID string, eventType outbox.EventType, now, leaseUntil time.Time) (*outbox.Event, error) {
	if !isValidOutboxIdentifier(workerID) || now.IsZero() || !leaseUntil.After(now) {
		return nil, outbox.ErrInvalidEvent
	}

	leaseToken, err := newOutboxLeaseToken()
	if err != nil {
		return nil, fmt.Errorf("generate outbox lease token: %w", err)
	}
	filter := bson.D{
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
		ID:             event.ID,
		AggregateID:    event.AggregateID,
		EventType:      string(event.EventType),
		Payload:        append([]byte(nil), event.Payload...),
		DeliveryStatus: string(event.DeliveryStatus),
		AttemptCount:   event.AttemptCount,
		NextAttemptAt:  event.NextAttemptAt,
		LeaseToken:     event.LeaseToken,
		LeaseUntil:     event.LeaseUntil,
		LeaseOwner:     event.LeaseOwner,
		LastError:      event.LastError,
		CreatedAt:      event.CreatedAt,
		UpdatedAt:      event.UpdatedAt,
	}
}

func toBizOutboxEvent(document model.OutboxEventDocument) *outbox.Event {
	return &outbox.Event{
		ID:             document.ID,
		AggregateID:    document.AggregateID,
		EventType:      outbox.EventType(document.EventType),
		Payload:        append([]byte(nil), document.Payload...),
		DeliveryStatus: outbox.DeliveryStatus(document.DeliveryStatus),
		AttemptCount:   document.AttemptCount,
		NextAttemptAt:  document.NextAttemptAt,
		LeaseToken:     document.LeaseToken,
		LeaseUntil:     document.LeaseUntil,
		LeaseOwner:     document.LeaseOwner,
		LastError:      document.LastError,
		CreatedAt:      document.CreatedAt,
		UpdatedAt:      document.UpdatedAt,
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
