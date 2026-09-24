package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-business-service/internal/biz/adminpricing"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoAdminPricingRepository struct{ data *Data }

func NewAdminPricingRepository(data *Data) adminpricing.Repository {
	return &mongoAdminPricingRepository{data: data}
}

func (r *mongoAdminPricingRepository) collection() (*mongo.Collection, error) {
	if r == nil || r.data == nil || r.data.database == nil {
		return nil, fmt.Errorf("admin pricing repository unavailable")
	}
	return r.data.database.Collection(schema.CollectionAdminPricingConfigs), nil
}

func pricingDocumentID(key, environment string) string { return key + "|" + environment }

func (r *mongoAdminPricingRepository) LoadSystemConfig(ctx context.Context, key, environment string) (*adminpricing.SystemConfig, error) {
	collection, err := r.collection()
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	environment = strings.TrimSpace(environment)
	if key == "" || environment == "" {
		return nil, adminpricing.ErrInvalid
	}
	var document model.AdminPricingConfigDocument
	read := func(env string) error {
		return collection.FindOne(ctx, bson.M{"key": key, "environment": env, "enabled": true}).Decode(&document)
	}
	if environment == "all" {
		err = read("all")
	} else {
		err = read(environment)
		if errors.Is(err, mongo.ErrNoDocuments) {
			err = read("all")
		}
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, adminpricing.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load admin pricing config: %w", err)
	}
	return pricingConfigFromDocument(document), nil
}

func (r *mongoAdminPricingRepository) SaveSystemConfig(ctx context.Context, actor adminpricing.Actor, input adminpricing.SystemConfigInput) (adminpricing.SystemConfig, error) {
	collection, err := r.collection()
	if err != nil {
		return adminpricing.SystemConfig{}, err
	}
	key := strings.TrimSpace(input.Key)
	environment := strings.TrimSpace(input.Environment)
	if key == "" || environment == "" || input.Value == nil {
		return adminpricing.SystemConfig{}, adminpricing.ErrInvalid
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := pricingDocumentID(key, environment)
	var result adminpricing.SystemConfig
	writeErr := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		var existing model.AdminPricingConfigDocument
		version := int64(1)
		createdAt := now
		if readErr := collection.FindOne(tx, bson.M{"_id": id}).Decode(&existing); readErr == nil {
			version = existing.Version + 1
			if !existing.CreatedAt.IsZero() {
				createdAt = existing.CreatedAt
			}
		} else if !errors.Is(readErr, mongo.ErrNoDocuments) {
			return fmt.Errorf("load existing admin pricing config: %w", readErr)
		}
		document := model.AdminPricingConfigDocument{
			ID: id, Key: key, Value: input.Value, Category: input.Category,
			Description: input.Description, Environment: environment, Enabled: true,
			UpdatedBy: actor.ID, ChangeReason: input.ChangeReason, Version: version,
			CreatedAt: createdAt, UpdatedAt: now,
		}
		if _, err := collection.ReplaceOne(tx, bson.M{"_id": id}, document, options.Replace().SetUpsert(true)); err != nil {
			return fmt.Errorf("save admin pricing config: %w", err)
		}
		audit := bson.M{
			"_id": uuid.NewString(), "actor_id": actor.ID, "target_id": id,
			"action": "admin_pricing_config_update", "config_key": key,
			"environment": environment, "version": version, "change_reason": input.ChangeReason,
			"created_at": now,
		}
		if _, err := r.data.database.Collection(schema.CollectionAdminAudit).InsertOne(tx, audit); err != nil {
			return fmt.Errorf("write admin pricing audit: %w", err)
		}
		result = *pricingConfigFromDocument(document)
		return nil
	})
	if writeErr != nil {
		return adminpricing.SystemConfig{}, writeErr
	}
	return result, nil
}

func (r *mongoAdminPricingRepository) DeleteSystemConfig(ctx context.Context, actor adminpricing.Actor, key, environment string) (adminpricing.SystemConfig, error) {
	collection, err := r.collection()
	if err != nil {
		return adminpricing.SystemConfig{}, err
	}
	key, environment = strings.TrimSpace(key), strings.TrimSpace(environment)
	if key == "" || environment == "" {
		return adminpricing.SystemConfig{}, adminpricing.ErrInvalid
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := pricingDocumentID(key, environment)
	var result adminpricing.SystemConfig
	writeErr := NewTxRunner(r.data).WithinTx(ctx, func(tx context.Context) error {
		var document model.AdminPricingConfigDocument
		if err := collection.FindOne(tx, bson.M{"_id": id}).Decode(&document); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return adminpricing.ErrNotFound
			}
			return fmt.Errorf("load admin pricing config for delete: %w", err)
		}
		document.Enabled = false
		document.UpdatedBy = actor.ID
		document.UpdatedAt = now
		document.Version++
		if _, err := collection.ReplaceOne(tx, bson.M{"_id": id}, document); err != nil {
			return fmt.Errorf("disable admin pricing config: %w", err)
		}
		if _, err := r.data.database.Collection(schema.CollectionAdminAudit).InsertOne(tx, bson.M{
			"_id": uuid.NewString(), "actor_id": actor.ID, "target_id": id,
			"action": "admin_pricing_config_delete", "config_key": key,
			"environment": environment, "version": document.Version, "created_at": now,
		}); err != nil {
			return fmt.Errorf("write admin pricing delete audit: %w", err)
		}
		result = *pricingConfigFromDocument(document)
		return nil
	})
	if writeErr != nil {
		return adminpricing.SystemConfig{}, writeErr
	}
	return result, nil
}

func pricingConfigFromDocument(document model.AdminPricingConfigDocument) *adminpricing.SystemConfig {
	value := normalizePricingBSONValue(document.Value)
	updatedAt := document.UpdatedAt.UTC()
	createdAt := document.CreatedAt.UTC()
	return &adminpricing.SystemConfig{
		Key: document.Key, Value: value, Category: document.Category,
		Description: document.Description, Environment: document.Environment,
		Enabled: document.Enabled, UpdatedAt: &updatedAt, CreatedAt: &createdAt,
		UpdatedBy: document.UpdatedBy,
	}
}

func normalizePricingBSONValue(value any) any {
	switch typed := value.(type) {
	case bson.D:
		out := make(map[string]any, len(typed))
		for _, item := range typed {
			out[item.Key] = normalizePricingBSONValue(item.Value)
		}
		return out
	case bson.M:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = normalizePricingBSONValue(item)
		}
		return out
	case bson.A:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = normalizePricingBSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = normalizePricingBSONValue(item)
		}
		return out
	default:
		return value
	}
}
