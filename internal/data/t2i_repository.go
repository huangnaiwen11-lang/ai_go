package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/biz/t2i"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const freeformTemplateID = "t2i-freeform"

// mongoFreeformRecipeRepository 只负责读取内部自由文生图配方。
// 它不返回通用模板目录，也不允许调用方按客户端字段指定模板或版本。
type mongoFreeformRecipeRepository struct {
	collection *mongo.Collection
	// templateID 由构造器固定注入，HTTP 请求不能影响其值。
	// 生产构造器始终使用 freeformTemplateID；测试可注入独立标识以隔离本地 fixture。
	templateID string
}

// NewFreeformRecipeRepository 创建自由文生图模板配方的只读仓储。
func NewFreeformRecipeRepository(data *Data) t2i.FreeformRecipeReader {
	return &mongoFreeformRecipeRepository{
		collection: data.database.Collection(schema.CollectionTemplates),
		templateID: freeformTemplateID,
	}
}

// LoadFreeformRecipe 读取唯一启用的安全图片模板版本。
// 多版本同时启用或参数不完整都会失败关闭，避免创建时随机选择技术配方。
func (repository *mongoFreeformRecipeRepository) LoadFreeformRecipe(ctx context.Context) (t2i.FreeformRecipe, error) {
	if repository == nil || repository.collection == nil {
		return t2i.FreeformRecipe{}, errors.New("freeform recipe repository is not configured")
	}
	cursor, err := repository.collection.Find(ctx, bson.D{
		{Key: "template_id", Value: repository.templateID},
		{Key: "enabled", Value: true},
		{Key: "mode", Value: string(catalog.ProductModeTemplateImage)},
		{Key: "content_surface", Value: string(catalog.ContentSurfaceSFW)},
	}, options.Find().SetSort(bson.D{{Key: "version", Value: -1}}).SetLimit(2))
	if err != nil {
		return t2i.FreeformRecipe{}, fmt.Errorf("find freeform template: %w", err)
	}
	defer cursor.Close(ctx)

	documents := make([]model.TemplateDocument, 0, 2)
	for cursor.Next(ctx) {
		var document model.TemplateDocument
		if err := cursor.Decode(&document); err != nil {
			return t2i.FreeformRecipe{}, fmt.Errorf("decode freeform template: %w", err)
		}
		documents = append(documents, document)
	}
	if err := cursor.Err(); err != nil {
		return t2i.FreeformRecipe{}, fmt.Errorf("iterate freeform templates: %w", err)
	}
	if len(documents) != 1 {
		return t2i.FreeformRecipe{}, errors.New("freeform template must have exactly one enabled version")
	}
	return toFreeformRecipe(documents[0])
}

// freeformRecipeParametersDocument 是模板 BSON 中仅供服务端读取的技术配方结构。
// 内层 parameters 是技术参数对象；模型、回调或工作流不能从 HTTP 请求传入此结构。
type freeformRecipeParametersDocument struct {
	ModelSKU   string   `bson:"model_sku"`
	Parameters bson.Raw `bson:"parameters"`
}

func toFreeformRecipe(document model.TemplateDocument) (t2i.FreeformRecipe, error) {
	var stored freeformRecipeParametersDocument
	if len(document.Parameters) == 0 || bson.Unmarshal(document.Parameters, &stored) != nil || len(stored.Parameters) == 0 {
		return t2i.FreeformRecipe{}, errors.New("invalid freeform template parameters")
	}
	var parameters map[string]any
	if err := bson.Unmarshal(stored.Parameters, &parameters); err != nil || parameters == nil {
		return t2i.FreeformRecipe{}, errors.New("invalid freeform technical parameters")
	}
	jsonParameters, err := json.Marshal(parameters)
	if err != nil {
		return t2i.FreeformRecipe{}, fmt.Errorf("encode freeform technical parameters: %w", err)
	}
	return t2i.FreeformRecipe{
		TemplateID: document.TemplateID,
		Version:    document.Version,
		ModelSKU:   stored.ModelSKU,
		Parameters: jsonParameters,
	}, nil
}

var _ t2i.FreeformRecipeReader = (*mongoFreeformRecipeRepository)(nil)
