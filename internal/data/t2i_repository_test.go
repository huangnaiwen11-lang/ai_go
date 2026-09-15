package data

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 自由文生图只能读取唯一、启用且属于安全图片模板的服务端配方。
func TestMongoFreeformRecipeRepository读取唯一启用配方(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	now := time.Now().UTC()
	// 每个集成测试使用独立模板标识，避免与正在运行的本地 fixture 共用配方记录。
	templateID := "test-t2i-freeform-" + uuid.NewString()
	valid := model.TemplateDocument{
		ID:             uuid.NewString(),
		TemplateID:     templateID,
		Version:        7,
		ContentSurface: string(catalog.ContentSurfaceSFW),
		Mode:           string(catalog.ProductModeTemplateImage),
		Enabled:        true,
		Parameters: newFreeformRecipeParameters(t, "ps-image-v1", bson.M{
			"steps":   28,
			"quality": "high",
		}),
		CreatedAt: now,
		UpdatedAt: now,
	}
	wrongMode := valid
	wrongMode.ID = uuid.NewString()
	wrongMode.Version = 8
	wrongMode.Mode = string(catalog.ProductModeTemplateVideo)

	collection := database.Collection(schema.CollectionTemplates)
	for _, document := range []model.TemplateDocument{valid, wrongMode} {
		if _, err := collection.InsertOne(ctx, document); err != nil {
			t.Fatalf("插入模板 %q: %v", document.ID, err)
		}
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		for _, document := range []model.TemplateDocument{valid, wrongMode} {
			if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: document.ID}}); err != nil {
				t.Errorf("删除测试模板 %q: %v", document.ID, err)
			}
		}
	})

	repository := &mongoFreeformRecipeRepository{collection: collection, templateID: templateID}
	recipe, err := repository.LoadFreeformRecipe(context.Background())
	if err != nil {
		t.Fatalf("LoadFreeformRecipe() error = %v", err)
	}
	if recipe.TemplateID != valid.TemplateID || recipe.Version != valid.Version || recipe.ModelSKU != "ps-image-v1" {
		t.Fatalf("读取配方 = %#v", recipe)
	}
	var parameters map[string]any
	if err := json.Unmarshal(recipe.Parameters, &parameters); err != nil {
		t.Fatalf("技术参数未转换为 JSON: %v", err)
	}
	if parameters["quality"] != "high" || parameters["steps"] != float64(28) {
		t.Fatalf("技术参数 = %#v", parameters)
	}
}

// 多个同时启用的自由模板会使同一次创建的配方不确定，读取必须失败关闭。
func TestMongoFreeformRecipeRepository拒绝多个启用版本(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	now := time.Now().UTC()
	collection := database.Collection(schema.CollectionTemplates)
	templateID := "test-t2i-freeform-" + uuid.NewString()
	documents := []model.TemplateDocument{
		freeformTemplateDocument(t, uuid.NewString(), templateID, 1, now),
		freeformTemplateDocument(t, uuid.NewString(), templateID, 2, now),
	}
	for _, document := range documents {
		if _, err := collection.InsertOne(ctx, document); err != nil {
			t.Fatalf("插入模板 %q: %v", document.ID, err)
		}
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		for _, document := range documents {
			if _, err := collection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: document.ID}}); err != nil {
				t.Errorf("删除测试模板 %q: %v", document.ID, err)
			}
		}
	})

	repository := &mongoFreeformRecipeRepository{collection: collection, templateID: templateID}
	if _, err := repository.LoadFreeformRecipe(ctx); err == nil {
		t.Fatal("多个启用版本仍返回了配方")
	}
}

func freeformTemplateDocument(t *testing.T, id, templateID string, version int64, now time.Time) model.TemplateDocument {
	t.Helper()
	return model.TemplateDocument{
		ID:             id,
		TemplateID:     templateID,
		Version:        version,
		ContentSurface: string(catalog.ContentSurfaceSFW),
		Mode:           string(catalog.ProductModeTemplateImage),
		Enabled:        true,
		Parameters:     newFreeformRecipeParameters(t, "ps-image-v1", bson.M{"steps": 28}),
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func newFreeformRecipeParameters(t *testing.T, modelSKU string, parameters bson.M) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(bson.M{"model_sku": modelSKU, "parameters": parameters})
	if err != nil {
		t.Fatalf("编码自由文生图配方: %v", err)
	}
	return raw
}
