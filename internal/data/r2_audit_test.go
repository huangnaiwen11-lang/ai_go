package data

import (
	"context"
	"testing"
	"time"

	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoR2AuditAssetIndexOnlyCountsAvailableCreationStepAssets(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("initialize schema: %v", err)
	}
	storage := &Data{client: client, database: database}
	now := time.Date(2026, time.September, 20, 20, 0, 0, 0, time.UTC)

	const referenced = "https://media.example.test/gen/text_to_image/tenant/step-1.png"
	const unavailable = "https://media.example.test/gen/text_to_image/tenant/step-2.png"
	const otherOwner = "https://media.example.test/gen/text_to_image/tenant/step-3.png"
	if _, err := database.Collection(schema.CollectionAssets).InsertMany(ctx, []any{
		model.AssetDocument{ID: "r2-audit-result", OwnerType: "creation_step", OwnerID: "step-1", AssetKind: "result", StorageKey: referenced, Status: "available", CreatedAt: now},
		model.AssetDocument{ID: "r2-audit-unavailable", OwnerType: "creation_step", OwnerID: "step-2", AssetKind: "result", StorageKey: unavailable, Status: "deleted", CreatedAt: now},
		model.AssetDocument{ID: "r2-audit-other-owner", OwnerType: "upload", OwnerID: "upload-3", AssetKind: "source", StorageKey: otherOwner, Status: "available", CreatedAt: now},
	}); err != nil {
		t.Fatalf("insert assets: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := database.Collection(schema.CollectionAssets).DeleteMany(cleanupCtx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: bson.A{"r2-audit-result", "r2-audit-unavailable", "r2-audit-other-owner"}}}}}); err != nil {
			t.Errorf("cleanup assets: %v", err)
		}
	})

	referencedKeys, err := NewR2AuditAssetIndex(storage).ReferencedStorageKeys(context.Background(), []string{referenced, unavailable, otherOwner})
	if err != nil {
		t.Fatalf("ReferencedStorageKeys() error = %v", err)
	}
	if len(referencedKeys) != 1 {
		t.Fatalf("ReferencedStorageKeys() = %#v, want only available creation-step asset", referencedKeys)
	}
	if _, exists := referencedKeys[referenced]; !exists {
		t.Fatalf("referenced key missing: %#v", referencedKeys)
	}
}
