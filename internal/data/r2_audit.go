package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/r2audit"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// NewR2AuditAssetIndex returns the read-only Mongo side of the R2 orphan
// audit. It deliberately is not registered in Data.ProviderSet: the audit is
// an explicit local/container operation, not a request-path dependency.
func NewR2AuditAssetIndex(data *Data) r2audit.AssetIndex {
	if data == nil {
		return &mongoR2AuditAssetIndex{}
	}
	return &mongoR2AuditAssetIndex{assets: data.database.Collection(schema.CollectionAssets)}
}

type mongoR2AuditAssetIndex struct {
	assets *mongo.Collection
}

// ReferencedStorageKeys returns the subset held by live, creation-step assets.
// A stale/deleted asset does not authorize retaining an object as a published
// result, and results from other ownership domains are outside this B2B audit.
func (index *mongoR2AuditAssetIndex) ReferencedStorageKeys(ctx context.Context, storageKeys []string) (map[string]struct{}, error) {
	if index == nil || index.assets == nil {
		return nil, errors.New("r2 audit asset index is not configured")
	}
	if len(storageKeys) == 0 {
		return map[string]struct{}{}, nil
	}
	unique := make([]string, 0, len(storageKeys))
	seen := make(map[string]struct{}, len(storageKeys))
	for _, storageKey := range storageKeys {
		if storageKey == "" {
			return nil, errors.New("r2 audit storage key is invalid")
		}
		if _, exists := seen[storageKey]; exists {
			continue
		}
		seen[storageKey] = struct{}{}
		unique = append(unique, storageKey)
	}
	cursor, err := index.assets.Find(ctx, bson.D{
		{Key: "owner_type", Value: "creation_step"},
		{Key: "status", Value: "available"},
		{Key: "storage_key", Value: bson.D{{Key: "$in", Value: unique}}},
	}, options.Find().SetProjection(bson.D{{Key: "storage_key", Value: int32(1)}}))
	if err != nil {
		return nil, fmt.Errorf("find R2 audit asset references: %w", err)
	}
	defer cursor.Close(ctx)
	referenced := make(map[string]struct{}, len(unique))
	for cursor.Next(ctx) {
		var document struct {
			StorageKey string `bson:"storage_key"`
		}
		if err := cursor.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode R2 audit asset reference: %w", err)
		}
		if _, requested := seen[document.StorageKey]; requested {
			referenced[document.StorageKey] = struct{}{}
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate R2 audit asset references: %w", err)
	}
	return referenced, nil
}

var _ r2audit.AssetIndex = (*mongoR2AuditAssetIndex)(nil)
