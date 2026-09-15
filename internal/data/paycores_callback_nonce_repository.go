package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// mongoPayCoresCallbackNonceRepository 只持久化 Go 自有 PayCores 回调的 nonce 摘要。
// 它与生成回调 receipt 及 Node 的 PaycoresCallbackNonce 保持完全隔离。
type mongoPayCoresCallbackNonceRepository struct {
	collection *mongo.Collection
}

// NewPayCoresCallbackNonceRepository 创建已验证 PayCores 回调的防重放存储实现。
func NewPayCoresCallbackNonceRepository(data *Data) payments.PaymentCallbackNonceStore {
	if data == nil || data.database == nil {
		return &mongoPayCoresCallbackNonceRepository{}
	}
	return &mongoPayCoresCallbackNonceRepository{collection: data.database.Collection(schema.CollectionPaymentCallbackNonces)}
}

// Consume 原子消费已验证 nonce；唯一键冲突只表示同一 nonce 已被并发或重放请求先行消费。
func (repository *mongoPayCoresCallbackNonceRepository) Consume(ctx context.Context, nonceHash payments.PaymentCallbackNonceHash, method, path string, expiresAt time.Time) error {
	if repository == nil || repository.collection == nil {
		return errors.New("paycores callback nonce repository is not configured")
	}
	if !nonceHash.Valid() || strings.TrimSpace(method) == "" || strings.TrimSpace(path) == "" || expiresAt.IsZero() {
		return fmt.Errorf("paycores callback nonce requires hash, method, path, and expiry: %w", payments.ErrInvalidPaymentCallbackNonce)
	}
	document := model.PayCoresCallbackNonceDocument{
		NonceHash: nonceHash.Hex(),
		Method:    method,
		Path:      path,
		ExpiresAt: expiresAt.UTC(),
	}
	if _, err := repository.collection.InsertOne(ctx, document); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("consume paycores callback nonce: %w: %w", payments.ErrPaymentCallbackReplayed, err)
		}
		return fmt.Errorf("consume paycores callback nonce: %w", err)
	}
	return nil
}

var _ payments.PaymentCallbackNonceStore = (*mongoPayCoresCallbackNonceRepository)(nil)
