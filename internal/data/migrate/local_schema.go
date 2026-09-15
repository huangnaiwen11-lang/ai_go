// Package migrate 提供本地 MongoDB 业务 schema 的初始化能力。
package migrate

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// namespaceExistsCode 表示集合已存在，初始化时可安全忽略该错误。
const namespaceExistsCode int32 = 48

// Initializer 在已验证的本地 MongoDB 数据库上创建业务集合及索引。
type Initializer struct {
	database *mongo.Database
}

// NewInitializer 构造本地 MongoDB schema 初始化器。
func NewInitializer(database *mongo.Database) *Initializer {
	return &Initializer{database: database}
}

// Ensure 幂等地创建 schema 中声明的全部集合和索引。
func (initializer *Initializer) Ensure(ctx context.Context) error {
	if initializer == nil || initializer.database == nil {
		return errors.New("local MongoDB schema initializer is not configured")
	}

	for _, collection := range schema.AllCollections() {
		if err := initializer.ensureCollection(ctx, collection); err != nil {
			return err
		}
	}

	indexesByCollection := make(map[string][]mongo.IndexModel)
	for _, spec := range schema.AllIndexes() {
		indexOptions := options.Index().SetName(spec.Name)
		if spec.Unique {
			indexOptions.SetUnique(true)
		}
		if spec.Sparse {
			indexOptions.SetSparse(true)
		}
		if spec.ExpireAfterSeconds != nil {
			indexOptions.SetExpireAfterSeconds(*spec.ExpireAfterSeconds)
		}
		if len(spec.PartialFilter) > 0 {
			indexOptions.SetPartialFilterExpression(spec.PartialFilter)
		}
		indexesByCollection[spec.Collection] = append(indexesByCollection[spec.Collection], mongo.IndexModel{
			Keys:    spec.Keys,
			Options: indexOptions,
		})
	}

	for _, collection := range schema.AllCollections() {
		indexes := indexesByCollection[collection]
		if len(indexes) == 0 {
			continue
		}
		if _, err := initializer.database.Collection(collection).Indexes().CreateMany(ctx, indexes); err != nil {
			return fmt.Errorf("create indexes for collection %q: %w", collection, err)
		}
	}

	return nil
}

func (initializer *Initializer) ensureCollection(ctx context.Context, name string) error {
	err := initializer.database.CreateCollection(ctx, name)
	if err == nil {
		return nil
	}

	var commandError mongo.CommandError
	if errors.As(err, &commandError) && commandError.Code == namespaceExistsCode {
		return nil
	}
	return fmt.Errorf("create collection %q: %w", name, err)
}
