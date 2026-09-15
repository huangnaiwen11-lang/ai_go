package data

import (
	"context"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 状态仓储必须在 MongoDB 查询中限制 user_id，并且成功图片只能来自最终步骤的可用结果资产。
func TestMongoImageStatusRepository按归属读取最终结果资产(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	now := time.Now().UTC()
	ownerID := uuid.NewString()
	pendingID := uuid.NewString()
	succeededID := uuid.NewString()
	outsiderID := uuid.NewString()
	videoID := uuid.NewString()
	finalStepID := uuid.NewString()
	collections := map[string][]string{
		schema.CollectionCreations:     {pendingID, succeededID, outsiderID, videoID},
		schema.CollectionCreationSteps: {finalStepID},
		schema.CollectionAssets:        {"asset:" + finalStepID + ":result"},
	}
	for collectionName, ids := range collections {
		for _, id := range ids {
			cleanupImageStatusTestDocument(t, database, collectionName, id)
		}
	}

	creationsCollection := database.Collection(schema.CollectionCreations)
	for _, document := range []model.CreationDocument{
		{ID: pendingID, IdempotencyKey: "test:" + pendingID, UserID: ownerID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusPendingSubmission), CreatedAt: now, UpdatedAt: now},
		{ID: succeededID, IdempotencyKey: "test:" + succeededID, UserID: ownerID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusSucceeded), CreatedAt: now, UpdatedAt: now},
		{ID: videoID, IdempotencyKey: "test:" + videoID, UserID: ownerID, ProductOutput: string(entitlement.ProductOutputVideo), Status: string(creations.CreationStatusPendingSubmission), CreatedAt: now, UpdatedAt: now},
		{ID: outsiderID, IdempotencyKey: "test:" + outsiderID, UserID: uuid.NewString(), ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusSucceeded), CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := creationsCollection.InsertOne(ctx, document); err != nil {
			t.Fatalf("插入创作 %q: %v", document.ID, err)
		}
	}
	if _, err := database.Collection(schema.CollectionCreationSteps).InsertOne(ctx, model.CreationStepDocument{
		ID: finalStepID, CreationID: succeededID, Sequence: 1, Atom: string(creations.AtomTextToImage), SubmitStatus: string(creations.StepSubmitStatusSucceeded), CreatedAt: now,
	}); err != nil {
		t.Fatalf("插入最终步骤: %v", err)
	}
	if _, err := database.Collection(schema.CollectionAssets).InsertOne(ctx, model.AssetDocument{
		ID: "asset:" + finalStepID + ":result", OwnerType: "creation_step", OwnerID: finalStepID, AssetKind: "result", StorageKey: "https://assets.example.test/final.png", Status: "available", CreatedAt: now,
	}); err != nil {
		t.Fatalf("插入最终结果资产: %v", err)
	}

	records, err := NewImageStatusRepository(&Data{database: database}).FindOwnedImages(context.Background(), ownerID, []string{pendingID, succeededID, videoID, outsiderID})
	if err != nil {
		t.Fatalf("FindOwnedImages() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("归属状态数 = %d, want 2", len(records))
	}
	if records[0].ID != pendingID || records[0].ResultURL != "" || records[1].ID != succeededID || records[1].ResultURL != "https://assets.example.test/final.png" {
		t.Fatalf("归属状态 = %#v", records)
	}
}

// cleanupImageStatusTestDocument 只删除本测试随机生成的精确 _id，禁止按宽泛条件清理集合。
func cleanupImageStatusTestDocument(t *testing.T, database *mongo.Database, collectionName, id string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := database.Collection(collectionName).DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: id}}); err != nil {
			t.Errorf("删除测试文档 %s/%s: %v", collectionName, id, err)
		}
	})
}
