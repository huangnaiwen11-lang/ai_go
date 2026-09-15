package data

import (
	"context"
	"errors"
	"fmt"

	"ai-business-service/internal/biz/entitlement"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type mongoSubscriptionRepository struct {
	subscriptions *mongo.Collection
}

// NewSubscriptionRepository 返回服务端自有订阅投影的只读仓储。
func NewSubscriptionRepository(data *Data) entitlement.SubscriptionReader {
	if data == nil || data.database == nil {
		return &mongoSubscriptionRepository{}
	}
	return &mongoSubscriptionRepository{
		subscriptions: data.database.Collection(schema.CollectionSubscriptions),
	}
}

// FindByUserID 读取用户订阅权益快照；投影不存在时返回 nil，nil。
func (repository *mongoSubscriptionRepository) FindByUserID(ctx context.Context, userID string) (*entitlement.SubscriptionSnapshot, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.SubscriptionDocument
	err := repository.subscriptions.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find subscription projection for user %q: %w", userID, err)
	}
	return toBizSubscription(document), nil
}

func (repository *mongoSubscriptionRepository) ready() error {
	if repository == nil || repository.subscriptions == nil {
		return errors.New("subscription repository is not configured")
	}
	return nil
}

func toBizSubscription(document model.SubscriptionDocument) *entitlement.SubscriptionSnapshot {
	return &entitlement.SubscriptionSnapshot{
		Status:        entitlement.SubscriptionStatus(document.Status),
		BillingPeriod: entitlement.SubscriptionBillingPeriod(document.BillingPeriod),
		StartsAt:      document.StartsAt,
		ExpiresAt:     document.ExpiresAt,
		CreatedAt:     document.CreatedAt,
		UpdatedAt:     document.UpdatedAt,
	}
}

var _ entitlement.SubscriptionReader = (*mongoSubscriptionRepository)(nil)
