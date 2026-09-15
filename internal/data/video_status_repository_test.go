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

// 视频状态仓储必须在 Mongo 查询中同时约束 user_id 与 product_output，最终地址只能来自最终步骤的可用视频资产。
func TestMongoVideoStatusRepository按归属和产品输出读取最终视频(t *testing.T) {
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
	imageID := uuid.NewString()
	outsiderID := uuid.NewString()
	finalStepID := uuid.NewString()
	collections := map[string][]string{
		schema.CollectionCreations:     {pendingID, succeededID, imageID, outsiderID},
		schema.CollectionCreationSteps: {finalStepID},
		schema.CollectionAssets:        {"asset:" + finalStepID + ":result"},
	}
	trackVideoStatusDocuments(t, database, collections)
	creationsCollection := database.Collection(schema.CollectionCreations)
	for _, document := range []model.CreationDocument{
		{ID: pendingID, IdempotencyKey: "test:" + pendingID, UserID: ownerID, ProductOutput: string(entitlement.ProductOutputVideo), VideoDurationSeconds: 5, Status: string(creations.CreationStatusPendingSubmission), CreatedAt: now, UpdatedAt: now},
		{ID: succeededID, IdempotencyKey: "test:" + succeededID, UserID: ownerID, ProductOutput: string(entitlement.ProductOutputVideo), VideoDurationSeconds: 10, Status: string(creations.CreationStatusSucceeded), CreatedAt: now, UpdatedAt: now},
		{ID: imageID, IdempotencyKey: "test:" + imageID, UserID: ownerID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusSucceeded), CreatedAt: now, UpdatedAt: now},
		{ID: outsiderID, IdempotencyKey: "test:" + outsiderID, UserID: uuid.NewString(), ProductOutput: string(entitlement.ProductOutputVideo), VideoDurationSeconds: 15, Status: string(creations.CreationStatusSucceeded), CreatedAt: now, UpdatedAt: now},
	} {
		if _, err := creationsCollection.InsertOne(ctx, document); err != nil {
			t.Fatalf("插入创作 %q: %v", document.ID, err)
		}
	}
	if _, err := database.Collection(schema.CollectionCreationSteps).InsertOne(ctx, model.CreationStepDocument{ID: finalStepID, CreationID: succeededID, Sequence: 1, Atom: string(creations.AtomImageToVideo), SubmitStatus: string(creations.StepSubmitStatusSucceeded), CreatedAt: now}); err != nil {
		t.Fatalf("插入最终步骤: %v", err)
	}
	if _, err := database.Collection(schema.CollectionAssets).InsertOne(ctx, model.AssetDocument{ID: "asset:" + finalStepID + ":result", OwnerType: "creation_step", OwnerID: finalStepID, AssetKind: "result", StorageKey: "https://assets.example.test/final.mp4", Status: "available", CreatedAt: now}); err != nil {
		t.Fatalf("插入最终视频资产: %v", err)
	}

	records, err := NewVideoStatusRepository(&Data{database: database}).FindOwnedVideos(context.Background(), ownerID, []string{pendingID, succeededID, imageID, outsiderID})
	if err != nil {
		t.Fatalf("FindOwnedVideos() error = %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("视频归属状态数 = %d, want 2", len(records))
	}
	if records[0].ID != pendingID || records[0].DurationSeconds != 5 || records[1].ID != succeededID || records[1].DurationSeconds != 10 || records[1].ResultURL != "https://assets.example.test/final.mp4" {
		t.Fatalf("视频归属状态 = %#v", records)
	}
}

// trackVideoStatusDocuments 只删除随机 fixture 的精确 _id，绝不按用户或集合进行宽泛删除。
func trackVideoStatusDocuments(t *testing.T, database *mongo.Database, collections map[string][]string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		for collectionName, ids := range collections {
			for _, id := range ids {
				if _, err := database.Collection(collectionName).DeleteOne(cleanupCtx, bson.D{{Key: "_id", Value: id}}); err != nil {
					t.Errorf("删除视频状态 fixture %s/%s: %v", collectionName, id, err)
				}
			}
		}
	})
}
