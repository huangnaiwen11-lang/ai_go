package data

import (
	"testing"
	"time"

	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoOutboxBacklogReaderReturnsAggregateCountsAndOldestFacts(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	prefix := "outbox-backlog-" + uuid.NewString() + "-"
	collection := database.Collection(schema.CollectionOutboxEvents)
	documents := []any{
		bson.D{{Key: "_id", Value: prefix + "pending-old"}, {Key: "delivery_status", Value: "pending"}, {Key: "created_at", Value: now.Add(-5 * time.Minute)}},
		bson.D{{Key: "_id", Value: prefix + "pending-new"}, {Key: "delivery_status", Value: "pending"}, {Key: "created_at", Value: now.Add(-time.Minute)}},
		bson.D{{Key: "_id", Value: prefix + "dispatching"}, {Key: "delivery_status", Value: "dispatching"}, {Key: "created_at", Value: now.Add(-3 * time.Minute)}},
		bson.D{{Key: "_id", Value: prefix + "reconciling"}, {Key: "delivery_status", Value: "reconciling"}, {Key: "created_at", Value: now.Add(-4 * time.Minute)}},
		bson.D{{Key: "_id", Value: prefix + "attention"}, {Key: "delivery_status", Value: "needs_attention"}, {Key: "created_at", Value: now.Add(-10 * time.Minute)}},
		bson.D{{Key: "_id", Value: prefix + "delivered"}, {Key: "delivery_status", Value: "delivered"}, {Key: "created_at", Value: now.Add(-20 * time.Minute)}},
	}
	if _, err := collection.InsertMany(ctx, documents); err != nil {
		t.Fatalf("insert outbox fixtures: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := collection.DeleteMany(cleanupCtx, bson.D{{Key: "_id", Value: bson.D{{Key: "$regex", Value: "^" + prefix}}}}); err != nil {
			t.Errorf("cleanup outbox fixtures: %v", err)
		}
	})

	snapshot, err := NewOutboxBacklogReader(&Data{client: client, database: database}).ReadOutboxBacklog(ctx)
	if err != nil {
		t.Fatalf("ReadOutboxBacklog() error = %v", err)
	}
	if snapshot.Pending != 2 || snapshot.Dispatching != 1 || snapshot.Reconciling != 1 || snapshot.NeedsAttention != 1 {
		t.Fatalf("ReadOutboxBacklog() counts = %#v", snapshot)
	}
	if !snapshot.OldestActiveCreatedAt.Equal(now.Add(-5 * time.Minute)) {
		t.Errorf("OldestActiveCreatedAt = %v, want %v", snapshot.OldestActiveCreatedAt, now.Add(-5*time.Minute))
	}
	if !snapshot.OldestAttentionCreatedAt.Equal(now.Add(-10 * time.Minute)) {
		t.Errorf("OldestAttentionCreatedAt = %v, want %v", snapshot.OldestAttentionCreatedAt, now.Add(-10*time.Minute))
	}
}
