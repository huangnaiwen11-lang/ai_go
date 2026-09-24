package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/data/schema"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const runtimeAppNamespaceExistsCode int32 = 48

// RuntimeAppSchemaInitializer creates only the collection and indexes needed
// by the read-only runtime App resolver. It deliberately does not initialize
// the wider business schema during Gateway startup.
type RuntimeAppSchemaInitializer struct {
	database *mongo.Database
}

func NewRuntimeAppSchemaInitializer(data *Data) *RuntimeAppSchemaInitializer {
	if data == nil {
		return &RuntimeAppSchemaInitializer{}
	}
	return &RuntimeAppSchemaInitializer{database: data.database}
}

func (initializer *RuntimeAppSchemaInitializer) Ensure(ctx context.Context) error {
	if initializer == nil || initializer.database == nil {
		return errors.New("runtime App schema initializer is not configured")
	}

	if err := initializer.ensureAppsCollection(ctx); err != nil {
		return err
	}
	indexes := schema.RuntimeAppIndexes()
	models := make([]mongo.IndexModel, 0, len(indexes))
	for _, spec := range indexes {
		models = append(models, mongo.IndexModel{
			Keys: spec.Keys,
			Options: options.Index().
				SetName(spec.Name).
				SetCollation(spec.Collation),
		})
	}
	if _, err := initializer.database.Collection(schema.CollectionApps).Indexes().CreateMany(ctx, models); err != nil {
		return fmt.Errorf("create runtime App indexes: %w", err)
	}
	return nil
}

func (initializer *RuntimeAppSchemaInitializer) ensureAppsCollection(ctx context.Context) error {
	err := initializer.database.CreateCollection(ctx, schema.CollectionApps)
	if err == nil {
		return nil
	}
	var commandError mongo.CommandError
	if errors.As(err, &commandError) && commandError.Code == runtimeAppNamespaceExistsCode {
		return nil
	}
	return fmt.Errorf("create runtime App collection: %w", err)
}
