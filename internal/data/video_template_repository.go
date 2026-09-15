package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/identity"
	bizvideo "ai-business-service/internal/biz/video"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoVideoTemplateRepository 只读取内容面允许且唯一启用的视频模板技术配方。
type mongoVideoTemplateRepository struct{ collection *mongo.Collection }

// NewVideoTemplateRepository 创建视频模板配方读取器。
func NewVideoTemplateRepository(data *Data) bizvideo.TemplateRecipeReader {
	if data == nil || data.database == nil {
		return &mongoVideoTemplateRepository{}
	}
	return &mongoVideoTemplateRepository{collection: data.database.Collection(schema.CollectionTemplates)}
}

// LoadTemplateVideo 在查询时同时限制模板模式、启用状态与内容面；多个启用版本一律失败关闭。
func (repository *mongoVideoTemplateRepository) LoadTemplateVideo(ctx context.Context, templateID, contentAccess string) (bizvideo.TemplateRecipe, error) {
	if repository == nil || repository.collection == nil || strings.TrimSpace(templateID) == "" {
		return bizvideo.TemplateRecipe{}, errors.New("video template repository is not configured")
	}
	surfaces, err := videoTemplateSurfaces(contentAccess)
	if err != nil {
		return bizvideo.TemplateRecipe{}, err
	}
	cursor, err := repository.collection.Find(ctx, bson.D{
		{Key: "template_id", Value: templateID},
		{Key: "enabled", Value: true},
		{Key: "mode", Value: string(catalog.ProductModeTemplateVideo)},
		{Key: "content_surface", Value: bson.D{{Key: "$in", Value: surfaces}}},
	}, options.Find().SetSort(bson.D{{Key: "version", Value: -1}}).SetLimit(2))
	if err != nil {
		return bizvideo.TemplateRecipe{}, fmt.Errorf("find video template recipe: %w", err)
	}
	defer cursor.Close(ctx)

	var documents []model.TemplateDocument
	for cursor.Next(ctx) {
		var document model.TemplateDocument
		if err := cursor.Decode(&document); err != nil {
			return bizvideo.TemplateRecipe{}, fmt.Errorf("decode video template recipe: %w", err)
		}
		documents = append(documents, document)
	}
	if err := cursor.Err(); err != nil {
		return bizvideo.TemplateRecipe{}, fmt.Errorf("iterate video template recipes: %w", err)
	}
	if len(documents) != 1 {
		return bizvideo.TemplateRecipe{}, bizvideo.ErrInvalidTemplateVideoRequest
	}
	return toVideoTemplateRecipe(documents[0])
}

func videoTemplateSurfaces(contentAccess string) (bson.A, error) {
	switch contentAccess {
	case identity.ContentAccessReviewRestricted:
		return bson.A{string(catalog.ContentSurfaceSFW)}, nil
	case identity.ContentAccessStandard:
		return bson.A{string(catalog.ContentSurfaceSFW), string(catalog.ContentSurfaceNSFW)}, nil
	default:
		return nil, bizvideo.ErrInvalidTemplateVideoRequest
	}
}

type templateVideoParametersDocument struct {
	Kind string   `bson:"kind"`
	I2V  bson.Raw `bson:"i2v"`
	T2I  bson.Raw `bson:"t2i"`
}

type technicalRecipeDocument struct {
	ModelSKU       string   `bson:"model_sku"`
	Prompt         string   `bson:"prompt"`
	NegativePrompt string   `bson:"negative_prompt"`
	Parameters     bson.Raw `bson:"parameters"`
}

func toVideoTemplateRecipe(document model.TemplateDocument) (bizvideo.TemplateRecipe, error) {
	var stored templateVideoParametersDocument
	if document.TemplateID == "" || document.Version <= 0 || len(document.Parameters) == 0 || !hasOnlyBSONFields(document.Parameters, "kind", "i2v", "t2i") || bson.Unmarshal(document.Parameters, &stored) != nil || stored.Kind != "template_video" {
		return bizvideo.TemplateRecipe{}, bizvideo.ErrInvalidTemplateVideoRequest
	}
	i2v, err := toVideoTechnicalRecipe(stored.I2V)
	if err != nil {
		return bizvideo.TemplateRecipe{}, bizvideo.ErrInvalidTemplateVideoRequest
	}
	recipe := bizvideo.TemplateRecipe{TemplateID: document.TemplateID, Version: document.Version, I2V: i2v}
	if len(stored.T2I) == 0 {
		return recipe, nil
	}
	t2i, err := toVideoTechnicalRecipe(stored.T2I)
	if err != nil {
		return bizvideo.TemplateRecipe{}, bizvideo.ErrInvalidTemplateVideoRequest
	}
	recipe.T2I = &t2i
	return recipe, nil
}

func toVideoTechnicalRecipe(raw bson.Raw) (bizvideo.TechnicalRecipe, error) {
	var stored technicalRecipeDocument
	if len(raw) == 0 || !hasOnlyBSONFields(raw, "model_sku", "prompt", "negative_prompt", "parameters") || bson.Unmarshal(raw, &stored) != nil || strings.TrimSpace(stored.ModelSKU) == "" || strings.TrimSpace(stored.Prompt) == "" || len(stored.Parameters) == 0 {
		return bizvideo.TechnicalRecipe{}, bizvideo.ErrInvalidTemplateVideoRequest
	}
	var parameters map[string]any
	if bson.Unmarshal(stored.Parameters, &parameters) != nil || len(parameters) == 0 {
		return bizvideo.TechnicalRecipe{}, bizvideo.ErrInvalidTemplateVideoRequest
	}
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return bizvideo.TechnicalRecipe{}, fmt.Errorf("encode video technical parameters: %w", err)
	}
	return bizvideo.TechnicalRecipe{ModelSKU: stored.ModelSKU, Prompt: stored.Prompt, NegativePrompt: stored.NegativePrompt, Parameters: encoded}, nil
}

// hasOnlyBSONFields 拒绝重复或未知 BSON 字段，避免 Go 静默忽略 Node 模板中的
// Provider、工作流、LoRA 等业务语义后仍继续接管生成请求。
func hasOnlyBSONFields(raw bson.Raw, allowed ...string) bool {
	allowedFields := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		allowedFields[field] = struct{}{}
	}
	elements, err := raw.Elements()
	if err != nil || len(elements) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(elements))
	for _, element := range elements {
		field, err := element.KeyErr()
		if err != nil {
			return false
		}
		if _, exists := allowedFields[field]; !exists {
			return false
		}
		if _, duplicate := seen[field]; duplicate {
			return false
		}
		seen[field] = struct{}{}
	}
	return true
}

var _ bizvideo.TemplateRecipeReader = (*mongoVideoTemplateRepository)(nil)
