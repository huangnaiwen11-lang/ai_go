package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/biz/entitlement"
	bizvideo "ai-business-service/internal/biz/video"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoVideoStatusRepository 只读取当前用户名下的视频创作与可信回调结果资产。
type mongoVideoStatusRepository struct {
	creations *mongo.Collection
	steps     *mongo.Collection
	assets    *mongo.Collection
}

// NewVideoStatusRepository 创建视频状态读取仓储。
func NewVideoStatusRepository(data *Data) bizvideo.VideoStatusReader {
	if data == nil || data.database == nil {
		return &mongoVideoStatusRepository{}
	}
	return &mongoVideoStatusRepository{
		creations: data.database.Collection(schema.CollectionCreations),
		steps:     data.database.Collection(schema.CollectionCreationSteps),
		assets:    data.database.Collection(schema.CollectionAssets),
	}
}

// FindOwnedVideos 通过单次创作查询完成任务 ID、归属和视频输出类型三重限制。
func (repository *mongoVideoStatusRepository) FindOwnedVideos(ctx context.Context, userID string, ids []string) ([]bizvideo.OwnedVideoCreation, error) {
	if repository == nil || repository.creations == nil || repository.steps == nil || repository.assets == nil {
		return nil, errors.New("video status repository is not configured")
	}
	if userID == "" || len(ids) == 0 {
		return nil, errors.New("video status query is invalid")
	}
	cursor, err := repository.creations.Find(ctx, bson.D{
		{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}},
		{Key: "user_id", Value: userID},
		{Key: "product_output", Value: string(entitlement.ProductOutputVideo)},
	})
	if err != nil {
		return nil, fmt.Errorf("find owned video creations: %w", err)
	}
	defer cursor.Close(ctx)

	found := make(map[string]model.CreationDocument, len(ids))
	for cursor.Next(ctx) {
		var document model.CreationDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode owned video creation: %w", err)
		}
		found[document.ID] = document
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate owned video creations: %w", err)
	}

	records := make([]bizvideo.OwnedVideoCreation, 0, len(found))
	for _, id := range ids {
		document, exists := found[id]
		if !exists {
			continue
		}
		record := bizvideo.OwnedVideoCreation{
			ID:              document.ID,
			Status:          creations.CreationStatus(document.Status),
			DurationSeconds: document.VideoDurationSeconds,
		}
		if record.Status == creations.CreationStatusSucceeded {
			resultURL, err := repository.finalVideoResultURL(ctx, document.ID)
			if err != nil {
				return nil, err
			}
			record.ResultURL = resultURL
		}
		records = append(records, record)
	}
	return records, nil
}

func (repository *mongoVideoStatusRepository) finalVideoResultURL(ctx context.Context, creationID string) (string, error) {
	var step model.CreationStepDocument
	err := repository.steps.FindOne(ctx, bson.D{{Key: "creation_id", Value: creationID}}, options.FindOne().SetSort(bson.D{{Key: "sequence", Value: -1}})).Decode(&step)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errors.New("succeeded video creation has no final step")
	}
	if err != nil {
		return "", fmt.Errorf("find final video creation step: %w", err)
	}
	var asset model.AssetDocument
	err = repository.assets.FindOne(ctx, bson.D{
		{Key: "owner_type", Value: creationStepAssetOwnerType},
		{Key: "owner_id", Value: step.ID},
		{Key: "asset_kind", Value: creationStepAssetKind},
		{Key: "status", Value: creationStepAssetAvailable},
	}, options.FindOne().SetSort(bson.D{{Key: "created_at", Value: -1}})).Decode(&asset)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errors.New("succeeded video creation has no final result asset")
	}
	if err != nil {
		return "", fmt.Errorf("find final video result asset: %w", err)
	}
	if asset.StorageKey == "" {
		return "", errors.New("final video result asset has no storage key")
	}
	return asset.StorageKey, nil
}

var _ bizvideo.VideoStatusReader = (*mongoVideoStatusRepository)(nil)
