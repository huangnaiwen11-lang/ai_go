package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/biz/t2i"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	creationStepAssetOwnerType = "creation_step"
	creationStepAssetKind      = "result"
	creationStepAssetAvailable = "available"
)

// mongoImageStatusRepository 只读取当前用户归属的创作和可信回调已写入的结果资产。
type mongoImageStatusRepository struct {
	creations *mongo.Collection
	steps     *mongo.Collection
	assets    *mongo.Collection
}

// NewImageStatusRepository 创建图片状态读取仓储。
func NewImageStatusRepository(data *Data) t2i.ImageStatusReader {
	if data == nil || data.database == nil {
		return &mongoImageStatusRepository{}
	}
	return &mongoImageStatusRepository{
		creations: data.database.Collection(schema.CollectionCreations),
		steps:     data.database.Collection(schema.CollectionCreationSteps),
		assets:    data.database.Collection(schema.CollectionAssets),
	}
}

// FindOwnedImages 仅返回 userID 所属的请求创作，并按请求 ID 顺序排列。
// 成功创作必须存在最终步骤的可用结果资产，否则返回数据不一致错误而不是 completed 空结果。
func (repository *mongoImageStatusRepository) FindOwnedImages(ctx context.Context, userID string, ids []string) ([]t2i.OwnedImageCreation, error) {
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.assets == nil {
		return nil, errors.New("image status repository is not configured")
	}
	if userID == "" || len(ids) == 0 {
		return nil, errors.New("image status query is invalid")
	}
	cursor, err := repository.creations.Find(ctx, bson.D{
		{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
		{Key: "user_id", Value: userID},
		// 图片状态读取不能仅靠调用路径断言类型：同一用户的视频创作若被误传 ID，
		// 必须在 Mongo 查询边界过滤，避免被投影为图片作品。
		{Key: "product_output", Value: string(entitlement.ProductOutputImage)},
	})
	if err != nil {
		return nil, fmt.Errorf("find owned image creations: %w", err)
	}
	defer cursor.Close(ctx)

	found := make(map[string]model.CreationDocument, len(ids))
	for cursor.Next(ctx) {
		var document model.CreationDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode owned image creation: %w", err)
		}
		found[document.ID] = document
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate owned image creations: %w", err)
	}

	records := make([]t2i.OwnedImageCreation, 0, len(found))
	for _, id := range ids {
		document, exists := found[id]
		if !exists {
			continue
		}
		record := t2i.OwnedImageCreation{ID: document.ID, Status: creations.CreationStatus(document.Status)}
		if record.Status == creations.CreationStatusSucceeded {
			resultURL, err := repository.finalResultURL(ctx, document.ID)
			if err != nil {
				return nil, err
			}
			record.ResultURL = resultURL
		}
		records = append(records, record)
	}
	return records, nil
}

func (repository *mongoImageStatusRepository) finalResultURL(ctx context.Context, creationID string) (string, error) {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{{Key: "creation_id", Value: creationID}}, options.FindOne().SetSort(bson.D{{Key: "sequence", Value: -1}})).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errors.New("succeeded creation has no final step")
	}
	if err != nil {
		return "", fmt.Errorf("find final creation step: %w", err)
	}
	var asset model.AssetDocument
	err = repository.assets.FindOne(ctx, bson.D{
		{Key: "owner_type", Value: creationStepAssetOwnerType},
		{Key: "owner_id", Value: step.ID},
		{Key: "asset_kind", Value: creationStepAssetKind},
		{Key: "status", Value: creationStepAssetAvailable},
	}, options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})).Decode(&asset)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errors.New("succeeded creation has no final result asset")
	}
	if err != nil {
		return "", fmt.Errorf("find final result asset: %w", err)
	}
	if asset.StorageKey == "" {
		return "", errors.New("final result asset has no storage key")
	}
	return asset.StorageKey, nil
}

var _ t2i.ImageStatusReader = (*mongoImageStatusRepository)(nil)
