package data

import (
	"context"
	"testing"
	"time"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoImageEditRecipe按内容访问级别读取模板(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	documents := []model.TemplateDocument{
		imageEditTemplateDocument(t, uuid.NewString(), "dress-up", catalog.ContentSurfaceSFW, now),
		imageEditTemplateDocument(t, uuid.NewString(), "undress", catalog.ContentSurfaceNSFW, now),
	}
	collection := database.Collection(schema.CollectionTemplates)
	for _, document := range documents {
		if _, err := collection.InsertOne(ctx, document); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		for _, document := range documents {
			if _, err := collection.DeleteOne(cleanupCtx, bson.D{{Key: "_id", Value: document.ID}}); err != nil {
				t.Errorf("精确删除 I2I 测试模板 %q：%v", document.ID, err)
			}
		}
	})
	repository := NewImageEditRecipeRepository(&Data{database: database})
	if recipe, err := repository.LoadImageEditRecipe(context.Background(), "dress-up", identity.ContentAccessReviewRestricted); err != nil || recipe.ModelSKU != "ps-edit-apparel-v1" || len(recipe.ReferenceAssets) != 1 {
		t.Fatalf("审核受限用户读取 SFW 模板 = %#v, %v", recipe, err)
	}
	if _, err := repository.LoadImageEditRecipe(context.Background(), "undress", identity.ContentAccessReviewRestricted); err == nil {
		t.Fatal("审核受限用户不应读取 NSFW 模板")
	}
	if recipe, err := repository.LoadImageEditRecipe(context.Background(), "undress", identity.ContentAccessStandard); err != nil || recipe.TemplateID != "undress" {
		t.Fatalf("普通用户读取 NSFW 模板 = %#v, %v", recipe, err)
	}
}

func imageEditTemplateDocument(t *testing.T, id, templateID string, surface catalog.ContentSurface, now time.Time) model.TemplateDocument {
	t.Helper()
	parameters, err := bson.Marshal(bson.M{
		"kind": "image_edit", "model_sku": "ps-edit-apparel-v1", "prompt": "模板提示词", "negative_prompt": "模糊",
		"parameters": bson.M{"steps": 28}, "input_rule": bson.M{"user_image_count": 1, "user_role": "source_image"},
		"reference_assets": bson.A{bson.M{"role": "guide_image", "url": "https://assets.example/guide.png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.TemplateDocument{ID: id, TemplateID: templateID, Version: 1, ContentSurface: string(surface), Mode: string(catalog.ProductModeTemplateImage), Enabled: true, Parameters: parameters, CreatedAt: now, UpdatedAt: now}
}
