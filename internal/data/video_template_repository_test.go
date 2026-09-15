package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/identity"
	bizvideo "ai-business-service/internal/biz/video"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 模板读取器必须把内容访问级别落实到 Mongo 查询中，审核受限用户不能获得 NSFW 配方。
func TestMongoVideoTemplateRepository按内容访问级别读取唯一模板(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}
	now := time.Now().UTC()
	documents := []model.TemplateDocument{
		videoTemplateDocument(t, uuid.NewString(), "video-sfw-"+uuid.NewString(), 1, catalog.ContentSurfaceSFW, now, true, true),
		videoTemplateDocument(t, uuid.NewString(), "video-nsfw-"+uuid.NewString(), 1, catalog.ContentSurfaceNSFW, now, true, true),
	}
	collection := database.Collection(schema.CollectionTemplates)
	trackVideoTemplateDocuments(t, collection, documents)
	for _, document := range documents {
		if _, err := collection.InsertOne(ctx, document); err != nil {
			t.Fatalf("插入视频模板 %q: %v", document.ID, err)
		}
	}

	repository := NewVideoTemplateRepository(&Data{database: database})
	if recipe, err := repository.LoadTemplateVideo(context.Background(), documents[0].TemplateID, identity.ContentAccessReviewRestricted); err != nil || recipe.TemplateID != documents[0].TemplateID || recipe.T2I == nil || recipe.I2V.ModelSKU != "ps-auto" {
		t.Fatalf("审核受限用户读取 SFW 模板 = %#v, %v", recipe, err)
	}
	if _, err := repository.LoadTemplateVideo(context.Background(), documents[1].TemplateID, identity.ContentAccessReviewRestricted); !errors.Is(err, bizvideo.ErrInvalidTemplateVideoRequest) {
		t.Fatalf("审核受限用户读取 NSFW 模板 error = %v，期望失败关闭", err)
	}
	if recipe, err := repository.LoadTemplateVideo(context.Background(), documents[1].TemplateID, identity.ContentAccessStandard); err != nil || recipe.TemplateID != documents[1].TemplateID {
		t.Fatalf("普通用户读取 NSFW 模板 = %#v, %v", recipe, err)
	}
}

// 多启用版本、非视频 kind 与不完整技术配方都必须失败关闭，不能按版本挑一个继续生成。
func TestMongoVideoTemplateRepository拒绝冲突与非法配方(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}
	now := time.Now().UTC()
	conflictID := "video-conflict-" + uuid.NewString()
	wrongKindID := "video-wrong-kind-" + uuid.NewString()
	invalidRecipeID := "video-invalid-recipe-" + uuid.NewString()
	unknownFieldID := "video-unknown-field-" + uuid.NewString()
	documents := []model.TemplateDocument{
		videoTemplateDocument(t, uuid.NewString(), conflictID, 1, catalog.ContentSurfaceSFW, now, true, true),
		videoTemplateDocument(t, uuid.NewString(), conflictID, 2, catalog.ContentSurfaceSFW, now, true, true),
		videoTemplateDocument(t, uuid.NewString(), wrongKindID, 1, catalog.ContentSurfaceSFW, now, true, true),
		videoTemplateDocument(t, uuid.NewString(), invalidRecipeID, 1, catalog.ContentSurfaceSFW, now, true, false),
		videoTemplateDocument(t, uuid.NewString(), unknownFieldID, 1, catalog.ContentSurfaceSFW, now, true, true),
	}
	wrongKind, err := bson.Marshal(bson.M{
		"kind": "image_edit", "i2v": bson.M{"model_sku": "ps-auto", "prompt": "动作", "negative_prompt": "模糊", "parameters": bson.M{"durationSeconds": 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	documents[2].Parameters = wrongKind
	unknownParameters, err := bson.Marshal(bson.M{
		"kind": "template_video", "provider": "legacy-provider",
		"i2v": bson.M{"model_sku": "ps-auto", "prompt": "动作", "negative_prompt": "模糊", "parameters": bson.M{"durationSeconds": 5}},
		"t2i": bson.M{"model_sku": "ps-image-v1", "prompt": "首帧", "negative_prompt": "模糊", "parameters": bson.M{"aspectRatio": "9:16"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	documents[4].Parameters = unknownParameters
	collection := database.Collection(schema.CollectionTemplates)
	trackVideoTemplateDocuments(t, collection, documents)
	for _, document := range documents {
		if _, err := collection.InsertOne(ctx, document); err != nil {
			t.Fatalf("插入视频模板 %q: %v", document.ID, err)
		}
	}

	repository := NewVideoTemplateRepository(&Data{database: database})
	for _, templateID := range []string{conflictID, wrongKindID, invalidRecipeID, unknownFieldID} {
		if _, err := repository.LoadTemplateVideo(context.Background(), templateID, identity.ContentAccessStandard); !errors.Is(err, bizvideo.ErrInvalidTemplateVideoRequest) {
			t.Fatalf("LoadTemplateVideo(%q) error = %v，期望失败关闭", templateID, err)
		}
	}
}

func videoTemplateDocument(t *testing.T, id, templateID string, version int64, surface catalog.ContentSurface, now time.Time, includeT2I, validI2V bool) model.TemplateDocument {
	t.Helper()
	i2v := bson.M{"model_sku": "ps-auto", "prompt": "服务端动作提示词", "negative_prompt": "模糊", "parameters": bson.M{"durationSeconds": 5}}
	if !validI2V {
		i2v = bson.M{"model_sku": "", "prompt": "", "parameters": bson.M{}}
	}
	parameters := bson.M{"kind": "template_video", "i2v": i2v}
	if includeT2I {
		parameters["t2i"] = bson.M{"model_sku": "ps-image-v1", "prompt": "服务端首帧提示词", "negative_prompt": "模糊", "parameters": bson.M{"aspectRatio": "9:16"}}
	}
	raw, err := bson.Marshal(parameters)
	if err != nil {
		t.Fatal(err)
	}
	return model.TemplateDocument{ID: id, TemplateID: templateID, Version: version, ContentSurface: string(surface), Mode: string(catalog.ProductModeTemplateVideo), Enabled: true, Parameters: raw, CreatedAt: now, UpdatedAt: now}
}

// trackVideoTemplateDocuments 仅按本测试生成的随机 _id 精确清理 fixture。
func trackVideoTemplateDocuments(t *testing.T, collection *mongo.Collection, documents []model.TemplateDocument) {
	t.Helper()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		for _, document := range documents {
			if _, err := collection.DeleteOne(cleanupCtx, bson.D{{Key: "_id", Value: document.ID}}); err != nil {
				t.Errorf("删除视频模板 fixture %q: %v", document.ID, err)
			}
		}
	})
}
