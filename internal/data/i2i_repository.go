package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/t2i"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoImageEditRecipeRepository 只读取用户可见的唯一启用图片编辑模板。
type mongoImageEditRecipeRepository struct{ collection *mongo.Collection }

func NewImageEditRecipeRepository(data *Data) t2i.ImageEditRecipeReader {
	if data == nil || data.database == nil {
		return &mongoImageEditRecipeRepository{}
	}
	return &mongoImageEditRecipeRepository{collection: data.database.Collection(schema.CollectionTemplates)}
}

func (repository *mongoImageEditRecipeRepository) LoadImageEditRecipe(ctx context.Context, templateID, contentAccess string) (t2i.ImageEditRecipe, error) {
	if repository == nil || repository.collection == nil || templateID == "" {
		return t2i.ImageEditRecipe{}, errors.New("image edit recipe repository is not configured")
	}
	surfaces := bson.A{string(catalog.ContentSurfaceSFW), string(catalog.ContentSurfaceNSFW)}
	if contentAccess == identity.ContentAccessReviewRestricted {
		surfaces = bson.A{string(catalog.ContentSurfaceSFW)}
	} else if contentAccess != identity.ContentAccessStandard {
		return t2i.ImageEditRecipe{}, t2i.ErrInvalidImageEditRequest
	}
	cursor, err := repository.collection.Find(ctx, bson.D{{Key: "template_id", Value: templateID}, {Key: "enabled", Value: true}, {Key: "mode", Value: string(catalog.ProductModeTemplateImage)}, {Key: "content_surface", Value: bson.D{{Key: "$in", Value: surfaces}}}}, options.Find().SetSort(bson.D{{Key: "version", Value: -1}}).SetLimit(2))
	if err != nil {
		return t2i.ImageEditRecipe{}, fmt.Errorf("find image edit recipe: %w", err)
	}
	defer cursor.Close(ctx)
	var documents []model.TemplateDocument
	for cursor.Next(ctx) {
		var document model.TemplateDocument
		if err := cursor.Decode(&document); err != nil {
			return t2i.ImageEditRecipe{}, fmt.Errorf("decode image edit recipe: %w", err)
		}
		documents = append(documents, document)
	}
	if err := cursor.Err(); err != nil {
		return t2i.ImageEditRecipe{}, fmt.Errorf("iterate image edit recipes: %w", err)
	}
	if len(documents) != 1 {
		return t2i.ImageEditRecipe{}, t2i.ErrInvalidImageEditRequest
	}
	return toImageEditRecipe(documents[0])
}

type imageEditRecipeParametersDocument struct {
	Kind            string                     `bson:"kind"`
	ModelSKU        string                     `bson:"model_sku"`
	Prompt          string                     `bson:"prompt"`
	NegativePrompt  string                     `bson:"negative_prompt"`
	Parameters      bson.Raw                   `bson:"parameters"`
	InputRule       imageEditInputRuleDocument `bson:"input_rule"`
	ReferenceAssets []imageEditAssetDocument   `bson:"reference_assets"`
}
type imageEditInputRuleDocument struct {
	UserImageCount int32  `bson:"user_image_count"`
	UserRole       string `bson:"user_role"`
}
type imageEditAssetDocument struct {
	Role string `bson:"role"`
	URL  string `bson:"url"`
}

func toImageEditRecipe(document model.TemplateDocument) (t2i.ImageEditRecipe, error) {
	var stored imageEditRecipeParametersDocument
	if len(document.Parameters) == 0 || bson.Unmarshal(document.Parameters, &stored) != nil || stored.Kind != "image_edit" || stored.InputRule.UserImageCount != 1 || stored.InputRule.UserRole != "source_image" {
		return t2i.ImageEditRecipe{}, t2i.ErrInvalidImageEditRequest
	}
	parameters := map[string]any{}
	if len(stored.Parameters) > 0 && bson.Unmarshal(stored.Parameters, &parameters) != nil {
		return t2i.ImageEditRecipe{}, t2i.ErrInvalidImageEditRequest
	}
	encoded, err := json.Marshal(parameters)
	if err != nil {
		return t2i.ImageEditRecipe{}, fmt.Errorf("encode image edit technical parameters: %w", err)
	}
	references := make([]t2i.ImageEditAsset, 0, len(stored.ReferenceAssets))
	for _, asset := range stored.ReferenceAssets {
		references = append(references, t2i.ImageEditAsset{Role: asset.Role, URL: asset.URL})
	}
	return t2i.ImageEditRecipe{TemplateID: document.TemplateID, Version: document.Version, ModelSKU: stored.ModelSKU, Prompt: stored.Prompt, NegativePrompt: stored.NegativePrompt, Parameters: encoded, ReferenceAssets: references}, nil
}

var _ t2i.ImageEditRecipeReader = (*mongoImageEditRecipeRepository)(nil)
