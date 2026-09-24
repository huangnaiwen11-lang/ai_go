package migrate

import (
	"context"
	"reflect"
	"testing"
	"time"

	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOutboxRetentionPolicyUsesApprovedDurations(t *testing.T) {
	got := defaultOutboxRetentionPolicy()
	want := []retentionRule{
		{status: "delivered", retention: 14 * 24 * time.Hour},
		{status: "failed", retention: 30 * 24 * time.Hour},
		{status: "needs_attention", retention: 90 * 24 * time.Hour},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaultOutboxRetentionPolicy() = %#v, want %#v", got, want)
	}
}

func TestOutboxRetentionCutoffNeverTargetsActiveStatuses(t *testing.T) {
	now := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	for _, rule := range defaultOutboxRetentionPolicy() {
		if rule.status != "delivered" && rule.status != "failed" && rule.status != "needs_attention" {
			t.Fatalf("unexpected retention status %q", rule.status)
		}
		if got := rule.cutoff(now); !got.Equal(now.Add(-rule.retention)) {
			t.Errorf("status %q cutoff = %v, want %v", rule.status, now.Add(-rule.retention), got)
		}
	}
}

func TestMongoOutboxRetentionPreflightPurgeAndCreateIndexes(t *testing.T) {
	// Preflight intentionally scans every terminal outbox event in its database.
	// It therefore cannot share the suite-wide cling_main test database: fixtures
	// from another package would make these exact retention counts nondeterministic.
	// Give this test a fresh database and drop only that generated database on exit.
	database := newLocalTestDatabase(t).Client().Database("cling_outbox_retention_" + uuid.NewString())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := database.Drop(cleanupCtx); err != nil {
			t.Errorf("drop isolated retention test database: %v", err)
		}
	})
	collection := database.Collection(schema.CollectionOutboxEvents)
	now := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefix := "retention-" + uuid.NewString() + "-"
	oldDelivered := now.Add(-15 * 24 * time.Hour)
	oldFailed := now.Add(-31 * 24 * time.Hour)
	oldAttention := now.Add(-91 * 24 * time.Hour)
	oldActive := now.Add(-365 * 24 * time.Hour)
	newDelivered := now.Add(-time.Hour)
	documents := []any{
		bson.D{{Key: "_id", Value: prefix + "delivered-old"}, {Key: "delivery_status", Value: "delivered"}, {Key: "updated_at", Value: oldDelivered}},
		bson.D{{Key: "_id", Value: prefix + "failed-old"}, {Key: "delivery_status", Value: "failed"}, {Key: "updated_at", Value: oldFailed}},
		bson.D{{Key: "_id", Value: prefix + "attention-old"}, {Key: "delivery_status", Value: "needs_attention"}, {Key: "updated_at", Value: oldAttention}},
		bson.D{{Key: "_id", Value: prefix + "pending-old"}, {Key: "delivery_status", Value: "pending"}, {Key: "updated_at", Value: oldActive}},
		bson.D{{Key: "_id", Value: prefix + "delivered-new"}, {Key: "delivery_status", Value: "delivered"}, {Key: "updated_at", Value: newDelivered}},
	}
	if _, err := collection.InsertMany(ctx, documents); err != nil {
		t.Fatalf("insert retention fixtures: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = collection.DeleteMany(cleanupCtx, bson.D{{Key: "_id", Value: bson.D{{Key: "$regex", Value: "^" + prefix}}}})
	})

	migrator := NewOutboxRetentionMigrator(database)
	report, err := migrator.Preflight(ctx, now)
	if err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	if !report.HasExpired() || len(report.Buckets) != 3 {
		t.Fatalf("Preflight() report = %#v, want three buckets with expired rows", report)
	}
	wantTotals := map[string]int64{"delivered": 2, "failed": 1, "needs_attention": 1}
	for _, bucket := range report.Buckets {
		if bucket.TotalCount != wantTotals[bucket.Status] || bucket.ExpiredCount != 1 || bucket.Oldest == nil {
			t.Errorf("bucket %q = %#v, want total=%d expired=1 oldest", bucket.Status, bucket, wantTotals[bucket.Status])
		}
	}

	deleted, err := migrator.PurgeExpired(ctx, now, 1)
	if err != nil {
		t.Fatalf("PurgeExpired() error = %v", err)
	}
	if deleted != 3 {
		t.Fatalf("PurgeExpired() deleted = %d, want 3", deleted)
	}
	for _, id := range []string{prefix + "delivered-old", prefix + "failed-old", prefix + "attention-old"} {
		if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Err(); err == nil {
			t.Errorf("expired document %q still exists", id)
		}
	}
	for _, id := range []string{prefix + "pending-old", prefix + "delivered-new"} {
		if err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Err(); err != nil {
			t.Errorf("active/non-expired document %q was removed: %v", id, err)
		}
	}

	if err := migrator.CreateIndexes(ctx); err != nil {
		t.Fatalf("CreateIndexes() error = %v", err)
	}
	indexes, err := collection.Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("list retention indexes: %v", err)
	}
	for _, name := range []string{
		"ix_outbox_events_delivered_updated_at_ttl",
		"ix_outbox_events_failed_updated_at_ttl",
		"ix_outbox_events_needs_attention_updated_at_ttl",
	} {
		found := false
		for i := range indexes {
			if indexes[i].Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("retention index %q was not created", name)
		}
	}
}
