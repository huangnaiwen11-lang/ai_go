package data

import (
	"context"
	"fmt"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/model"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// NewPaymentReadRepository 复用共享 Mongo 客户端，只向读用例提供收窄后的仓储接口。
func NewPaymentReadRepository(data *Data) payments.ReadRepository {
	repository, _ := NewPaymentRepository(data).(payments.ReadRepository)
	return repository
}

// publishedPaymentSnapshotsPipeline 使用不可变版本索引语义，先匹配发布态再按每个 ID 取最高版本。
// 价格过滤留给业务层，不能在 $group 前过滤无效价格而意外售卖同 ID 的历史版本。
func publishedPaymentSnapshotsPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{{Key: "publish_status", Value: "published"}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "product_id", Value: 1}, {Key: "version", Value: -1}}}},
		bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$product_id"}, {Key: "snapshot", Value: bson.D{{Key: "$first", Value: "$$ROOT"}}}}}},
		bson.D{{Key: "$replaceRoot", Value: bson.D{{Key: "newRoot", Value: "$snapshot"}}}},
		bson.D{{Key: "$sort", Value: bson.D{{Key: "product_id", Value: 1}}}},
	}
}

func (repository *mongoPaymentRepository) ListPublishedProductSnapshots(ctx context.Context) ([]payments.PaymentProduct, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	cursor, err := repository.products.Aggregate(ctx, publishedPaymentSnapshotsPipeline())
	if err != nil {
		return nil, fmt.Errorf("list published payment snapshots: %w", err)
	}
	defer cursor.Close(ctx)
	var documents []model.PaymentProductDocument
	if err := cursor.All(ctx, &documents); err != nil {
		return nil, fmt.Errorf("decode published payment snapshots: %w", err)
	}
	products := make([]payments.PaymentProduct, 0, len(documents))
	for _, document := range documents {
		products = append(products, paymentProductSnapshotToBiz(document))
	}
	return products, nil
}

func paymentProductSnapshotToBiz(document model.PaymentProductDocument) payments.PaymentProduct {
	return payments.PaymentProduct{ID: document.ProductID, Version: document.Version, DiamondAmount: document.DiamondAmount, AmountCents: document.AmountCents, Currency: document.Currency, Label: document.Label, PublishStatus: payments.ProductPublishStatus(document.PublishStatus)}
}

var _ payments.ReadRepository = (*mongoPaymentRepository)(nil)
