package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/outbox"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoOutboxBacklogReader struct {
	collection *mongo.Collection
}

// NewOutboxBacklogReader returns the read-only aggregate needed by the worker
// metrics endpoint. It does not expose documents or payloads and is not part
// of the delivery Repository contract.
func NewOutboxBacklogReader(data *Data) outbox.BacklogReader {
	if data == nil || data.database == nil {
		return &mongoOutboxBacklogReader{}
	}
	return &mongoOutboxBacklogReader{collection: data.database.Collection(schema.CollectionOutboxEvents)}
}

func (reader *mongoOutboxBacklogReader) ReadOutboxBacklog(ctx context.Context) (outbox.BacklogSnapshot, error) {
	if reader == nil || reader.collection == nil {
		return outbox.BacklogSnapshot{}, errors.New("outbox backlog reader is not configured")
	}
	if ctx == nil {
		return outbox.BacklogSnapshot{}, errors.New("outbox backlog context is required")
	}

	statuses := bson.A{
		string(outbox.DeliveryStatusPending),
		string(outbox.DeliveryStatusDispatching),
		string(outbox.DeliveryStatusReconciling),
		string(outbox.DeliveryStatusNeedsAttention),
	}
	cursor, err := reader.collection.Aggregate(ctx, bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: statuses}}}}}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$delivery_status"}, {Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
	})
	if err != nil {
		return outbox.BacklogSnapshot{}, fmt.Errorf("aggregate outbox backlog: %w", err)
	}
	defer func() { _ = cursor.Close(ctx) }()

	var snapshot outbox.BacklogSnapshot
	for cursor.Next(ctx) {
		var bucket struct {
			Status string `bson:"_id"`
			Count  int64  `bson:"count"`
		}
		if err := cursor.Decode(&bucket); err != nil {
			return outbox.BacklogSnapshot{}, fmt.Errorf("decode outbox backlog bucket: %w", err)
		}
		switch outbox.DeliveryStatus(bucket.Status) {
		case outbox.DeliveryStatusPending:
			snapshot.Pending = bucket.Count
		case outbox.DeliveryStatusDispatching:
			snapshot.Dispatching = bucket.Count
		case outbox.DeliveryStatusReconciling:
			snapshot.Reconciling = bucket.Count
		case outbox.DeliveryStatusNeedsAttention:
			snapshot.NeedsAttention = bucket.Count
		}
	}
	if err := cursor.Err(); err != nil {
		return outbox.BacklogSnapshot{}, fmt.Errorf("iterate outbox backlog buckets: %w", err)
	}

	activeStatuses := bson.A{
		string(outbox.DeliveryStatusPending),
		string(outbox.DeliveryStatusDispatching),
		string(outbox.DeliveryStatusReconciling),
	}
	activeFilter := bson.D{{Key: "delivery_status", Value: bson.D{{Key: "$in", Value: activeStatuses}}}}
	activeCreatedAt, err := reader.oldestCreatedAt(ctx, activeFilter)
	if err != nil {
		return outbox.BacklogSnapshot{}, err
	}
	snapshot.OldestActiveCreatedAt = activeCreatedAt
	attentionCreatedAt, err := reader.oldestCreatedAt(ctx, bson.D{{Key: "delivery_status", Value: string(outbox.DeliveryStatusNeedsAttention)}})
	if err != nil {
		return outbox.BacklogSnapshot{}, err
	}
	snapshot.OldestAttentionCreatedAt = attentionCreatedAt
	return snapshot, nil
}

func (reader *mongoOutboxBacklogReader) oldestCreatedAt(ctx context.Context, filter bson.D) (time.Time, error) {
	var oldest struct {
		CreatedAt time.Time `bson:"created_at"`
	}
	err := reader.collection.FindOne(
		ctx,
		filter,
		options.FindOne().SetProjection(bson.D{{Key: "created_at", Value: 1}}).SetSort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}}),
	).Decode(&oldest)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("find oldest outbox event: %w", err)
	}
	return oldest.CreatedAt, nil
}

var _ outbox.BacklogReader = (*mongoOutboxBacklogReader)(nil)
