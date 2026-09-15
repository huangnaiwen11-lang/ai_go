package data

import (
	"context"
	"fmt"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoTemplateRepository struct {
	collection *mongo.Collection
}

// NewTemplateRepository 返回模板目录只读仓储的领域接口实现。
func NewTemplateRepository(data *Data) catalog.TemplateRepository {
	return &mongoTemplateRepository{collection: data.database.Collection(schema.CollectionTemplates)}
}

// ListEnabled 只读取启用中的模板。
// 审核状态、内容面筛选和跨端排序均由 biz/catalog 负责，避免数据层混入业务规则。
func (repository *mongoTemplateRepository) ListEnabled(ctx context.Context) ([]catalog.Template, error) {
	cursor, err := repository.collection.Find(
		ctx,
		bson.D{{Key: "enabled", Value: true}},
		options.Find().SetSort(bson.D{
			{Key: "content_surface", Value: 1},
			{Key: "sort_order", Value: 1},
			{Key: "template_id", Value: 1},
			{Key: "version", Value: 1},
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("list enabled templates: %w", err)
	}
	defer cursor.Close(ctx)

	templates := make([]catalog.Template, 0)
	for cursor.Next(ctx) {
		var document model.TemplateDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode template: %w", err)
		}
		templates = append(templates, toBizTemplate(document))
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate enabled templates: %w", err)
	}
	return templates, nil
}

func toBizTemplate(document model.TemplateDocument) catalog.Template {
	return catalog.Template{
		ID:              document.ID,
		TemplateID:      document.TemplateID,
		Version:         document.Version,
		ContentSurface:  catalog.ContentSurface(document.ContentSurface),
		ProductMode:     catalog.ProductMode(document.Mode),
		SortOrder:       document.SortOrder,
		Enabled:         document.Enabled,
		Title:           document.Title,
		CoverURL:        document.CoverURL,
		VideoURL:        document.VideoURL,
		PreviewVideoURL: document.PreviewVideoURL,
		Tag:             document.Tag,
		Badge:           document.Badge,
	}
}

var _ catalog.TemplateRepository = (*mongoTemplateRepository)(nil)
