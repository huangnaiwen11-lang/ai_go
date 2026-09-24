package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/generation"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoMappingCatalogRepository 只读取已发布模型目录版本。
// 目录的拉取、校验与发布属于控制面，本仓储不实现任何写入：运行期只允许按
// 已冻结的版本号读取，避免线上映射随目录更新而漂移。
type mongoMappingCatalogRepository struct {
	catalogs *mongo.Collection
}

// NewMappingCatalogRepository 返回只读的已发布目录仓储。
func NewMappingCatalogRepository(data *Data) generation.MappingCatalogStore {
	if data == nil || data.database == nil {
		return &mongoMappingCatalogRepository{}
	}
	return &mongoMappingCatalogRepository{catalogs: data.database.Collection(schema.CollectionGenerationModelMappings)}
}

// PublishedCatalog 按版本读取已发布目录；version 为空时取最新已发布版本。
//
// 两种「读不到」都返回 ErrMappingCatalogUnavailable 并要求调用方 fail closed：
// 尚未发布任何版本，或冻结版本已被回滚/删除。二者运维处置不同（发布 vs 恢复），
// 由调用方记录版本号区分，而不是靠错误类型分叉——任何一条都不允许退回本地执行。
func (r *mongoMappingCatalogRepository) PublishedCatalog(ctx context.Context, version string) (*generation.PublishedMappingCatalog, error) {
	if r == nil || r.catalogs == nil {
		return nil, generation.ErrMappingCatalogUnavailable
	}
	filter := bson.D{{Key: "status", Value: string(generation.MappingCatalogStatusPublished)}}
	findOptions := options.FindOne()
	if version == "" {
		// published_at 相同时用 _id 兜底排序，保证「最新」在并发发布下仍然确定。
		findOptions.SetSort(bson.D{{Key: "published_at", Value: -1}, {Key: "_id", Value: -1}})
	} else {
		filter = append(filter, bson.E{Key: "_id", Value: version})
	}
	var document model.MappingCatalogDocument
	if err := r.catalogs.FindOne(ctx, filter, findOptions).Decode(&document); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, generation.ErrMappingCatalogUnavailable
		}
		return nil, fmt.Errorf("read published mapping catalog: %w", err)
	}
	// 已落库版本仍然要过一遍结构校验：坏版本必须在读取处暴露，
	// 而不是让缺字段的映射继续流到出站请求。
	normalized, err := mappingCatalogFromDocument(document).Normalize()
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

func mappingCatalogFromDocument(document model.MappingCatalogDocument) generation.PublishedMappingCatalog {
	entries := make([]generation.ModelMappingEntry, 0, len(document.Models))
	for _, item := range document.Models {
		sizes := make([]generation.ModelMappingSize, 0, len(item.Sizes))
		for _, size := range item.Sizes {
			sizes = append(sizes, generation.ModelMappingSize{Width: int(size.Width), Height: int(size.Height)})
		}
		durations := make([]int, 0, len(item.Durations))
		for _, duration := range item.Durations {
			durations = append(durations, int(duration))
		}
		templates := make([]generation.ModelMappingTemplate, 0, len(item.Templates))
		for _, template := range item.Templates {
			templates = append(templates, generation.ModelMappingTemplate{Key: template.Key, ID: template.ID})
		}
		entries = append(entries, generation.ModelMappingEntry{
			Capability: item.Capability, ProductKey: item.ProductKey, PublicModel: item.PublicModel,
			AllowedInputs: append([]string(nil), item.AllowedInputs...),
			Sizes:         sizes,
			AspectRatios:  append([]string(nil), item.AspectRatios...),
			Durations:     durations,
			Templates:     templates,
			Enabled:       item.Enabled,
		})
	}
	return generation.PublishedMappingCatalog{
		Version:       document.ID,
		SourceVersion: document.SourceVersion,
		PublishedAt:   document.PublishedAt,
		Entries:       entries,
	}
}

var _ generation.MappingCatalogStore = (*mongoMappingCatalogRepository)(nil)
