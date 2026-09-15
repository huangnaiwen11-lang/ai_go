package data

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/works"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestMongo作品历史仅返回本人指定类型并投影安全结果(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	now := time.Date(2026, time.September, 11, 9, 0, 0, 0, time.UTC)
	ownerID, outsiderID := uuid.NewString(), uuid.NewString()
	succeededID, pendingID, failedID, videoID, outsiderWorkID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	stepID := uuid.NewString()
	trackWorksDocuments(t, database, map[string][]string{
		schema.CollectionCreations:     {succeededID, pendingID, failedID, videoID, outsiderWorkID},
		schema.CollectionCreationSteps: {stepID},
		schema.CollectionAssets:        {"works-result:" + stepID},
	})
	creationsCollection := database.Collection(schema.CollectionCreations)
	for _, document := range []model.CreationDocument{
		{ID: succeededID, IdempotencyKey: "works:" + succeededID, UserID: ownerID, TemplateID: "image-template", TemplateVersion: 7, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusSucceeded), CreatedAt: now, UpdatedAt: now},
		{ID: pendingID, IdempotencyKey: "works:" + pendingID, UserID: ownerID, TemplateID: "image-template", TemplateVersion: 7, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusPendingSubmission), CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
		{ID: failedID, IdempotencyKey: "works:" + failedID, UserID: ownerID, TemplateID: "image-template", TemplateVersion: 7, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusGenerationFailed), CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-time.Minute)},
		{ID: videoID, IdempotencyKey: "works:" + videoID, UserID: ownerID, TemplateID: "video-template", TemplateVersion: 3, ProductOutput: string(entitlement.ProductOutputVideo), VideoDurationSeconds: 10, Status: string(creations.CreationStatusPendingSubmission), CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute)},
		{ID: outsiderWorkID, IdempotencyKey: "works:" + outsiderWorkID, UserID: outsiderID, TemplateID: "image-template", TemplateVersion: 7, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusSucceeded), CreatedAt: now.Add(2 * time.Minute), UpdatedAt: now.Add(2 * time.Minute)},
	} {
		if _, err := creationsCollection.InsertOne(ctx, document); err != nil {
			t.Fatalf("插入作品 %q: %v", document.ID, err)
		}
	}
	if _, err := database.Collection(schema.CollectionCreationSteps).InsertOne(ctx, model.CreationStepDocument{ID: stepID, CreationID: succeededID, Sequence: 1, Atom: string(creations.AtomTextToImage), SubmitStatus: string(creations.StepSubmitStatusSucceeded), CreatedAt: now}); err != nil {
		t.Fatalf("插入最终步骤: %v", err)
	}
	if _, err := database.Collection(schema.CollectionAssets).InsertOne(ctx, model.AssetDocument{ID: "works-result:" + stepID, OwnerType: creationStepAssetOwnerType, OwnerID: stepID, AssetKind: creationStepAssetKind, StorageKey: "https://assets.example.test/final.png", Status: creationStepAssetAvailable, CreatedAt: now}); err != nil {
		t.Fatalf("插入最终结果资产: %v", err)
	}

	repository := NewWorksRepository(&Data{database: database})
	page, err := repository.List(ctx, works.ListQuery{UserID: ownerID, Kind: works.KindImage, Limit: 10})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got, want := workIDs(page.Items), []string{succeededID, pendingID, failedID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("image works = %v, want %v", got, want)
	}
	if page.Items[0].ResultURL != "https://assets.example.test/final.png" || page.Items[0].Kind != works.KindImage || page.Items[0].DurationSeconds != 0 {
		t.Fatalf("成功图片投影 = %#v", page.Items[0])
	}
	if page.Items[1].ResultURL != "" || page.Items[1].Error != "" || page.Items[2].ResultURL != "" || page.Items[2].Error == "" {
		t.Fatalf("待生成或失败作品投影错误: %#v", page.Items)
	}

	videoPage, err := repository.List(ctx, works.ListQuery{UserID: ownerID, Kind: works.KindVideo, Limit: 10})
	if err != nil || len(videoPage.Items) != 1 || videoPage.Items[0].ID != videoID || videoPage.Items[0].DurationSeconds != 10 {
		t.Fatalf("video works=%#v err=%v", videoPage, err)
	}
	allPage, err := repository.List(ctx, works.ListQuery{UserID: ownerID, Limit: 10})
	if err != nil {
		t.Fatalf("全部作品 List() error = %v", err)
	}
	if got, want := workIDs(allPage.Items), []string{videoID, succeededID, pendingID, failedID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("全部作品 = %v, want %v", got, want)
	}
	if _, err := repository.FindByID(ctx, ownerID, outsiderWorkID); !errors.Is(err, works.ErrWorkNotFound) {
		t.Fatalf("跨用户详情 error=%v，want ErrWorkNotFound", err)
	}
}

func TestMongo作品历史复合游标不遗漏同一时间戳(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	prefix := "works-same-time-" + uuid.NewString() + "-"
	sameTime := time.Date(2026, time.September, 11, 8, 0, 0, 0, time.UTC)
	documents := []model.CreationDocument{
		{ID: prefix + "c", IdempotencyKey: prefix + "c", UserID: userID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusPendingSubmission), CreatedAt: sameTime, UpdatedAt: sameTime},
		{ID: prefix + "b", IdempotencyKey: prefix + "b", UserID: userID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusPendingSubmission), CreatedAt: sameTime, UpdatedAt: sameTime},
		{ID: prefix + "a", IdempotencyKey: prefix + "a", UserID: userID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusPendingSubmission), CreatedAt: sameTime, UpdatedAt: sameTime},
		{ID: prefix + "older", IdempotencyKey: prefix + "older", UserID: userID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusPendingSubmission), CreatedAt: sameTime.Add(-time.Minute), UpdatedAt: sameTime.Add(-time.Minute)},
	}
	trackWorksDocuments(t, database, map[string][]string{schema.CollectionCreations: {documents[0].ID, documents[1].ID, documents[2].ID, documents[3].ID}})
	for _, document := range documents {
		if _, err := database.Collection(schema.CollectionCreations).InsertOne(ctx, document); err != nil {
			t.Fatalf("插入作品 %q: %v", document.ID, err)
		}
	}

	repository := NewWorksRepository(&Data{database: database})
	got := collectWorkIDs(t, ctx, repository, userID, 1)
	want := []string{documents[0].ID, documents[1].ID, documents[2].ID, documents[3].ID}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("分页作品 = %v, want %v", got, want)
	}
}

func TestMongo作品历史成功但缺失最终资产拒绝伪造成功(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}
	userID, workID := uuid.NewString(), uuid.NewString()
	trackWorksDocuments(t, database, map[string][]string{schema.CollectionCreations: {workID}})
	if _, err := database.Collection(schema.CollectionCreations).InsertOne(ctx, model.CreationDocument{ID: workID, IdempotencyKey: "works:" + workID, UserID: userID, ProductOutput: string(entitlement.ProductOutputImage), Status: string(creations.CreationStatusSucceeded), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, err := NewWorksRepository(&Data{database: database}).FindByID(ctx, userID, workID)
	if !errors.Is(err, works.ErrResultUnavailable) {
		t.Fatalf("FindByID() error=%v，成功作品缺失结果不得伪造成功", err)
	}
}

func collectWorkIDs(t *testing.T, ctx context.Context, repository works.Repository, userID string, limit int) []string {
	t.Helper()
	var all []string
	query := works.ListQuery{UserID: userID, Kind: works.KindImage, Limit: limit}
	for {
		page, err := repository.List(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, workIDs(page.Items)...)
		if page.NextCursor == "" {
			return all
		}
		query.Cursor, query.Skip = page.NextCursor, 999
	}
}

func workIDs(items []works.Work) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func trackWorksDocuments(t *testing.T, database *mongo.Database, collections map[string][]string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupCtx, cancel := newMongoTestContext()
		defer cancel()
		for collectionName, ids := range collections {
			for _, id := range ids {
				if _, err := database.Collection(collectionName).DeleteOne(cleanupCtx, bson.D{{Key: "_id", Value: id}}); err != nil {
					t.Errorf("删除作品 fixture %s/%s: %v", collectionName, id, err)
				}
			}
		}
	})
}
