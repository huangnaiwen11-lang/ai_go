package data

import (
	"context"
	"errors"

	"ai-business-service/internal/biz/shared"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// MongoTxRunner 将 MongoDB Session Transaction 封装为业务层可用的原子写入边界。
// 业务回调只能使用传入的事务 context；仓储必须将该 context 传入每次 MongoDB 写入。
type MongoTxRunner struct {
	client *mongo.Client
}

// NewMongoTxRunner 创建事务运行器。MongoDB Client 仅保留在 data 层内部。
func NewMongoTxRunner(client *mongo.Client) *MongoTxRunner {
	return &MongoTxRunner{client: client}
}

// WithinTx 在同一个 MongoDB Session Transaction 中执行操作。
// MongoDB 可能重试回调，所以调用方必须使用稳定幂等键和唯一索引保护业务事实。
func (runner *MongoTxRunner) WithinTx(ctx context.Context, operation func(context.Context) error) error {
	if runner == nil || runner.client == nil {
		return errors.New("MongoDB transaction runner is not initialized")
	}
	if operation == nil {
		return errors.New("MongoDB transaction operation is required")
	}

	session, err := runner.client.StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(transactionContext context.Context) (any, error) {
		return nil, operation(transactionContext)
	})
	return err
}

// 编译期断言：data 层实现业务层声明的事务边界，业务层无需依赖 MongoDB Driver。
var _ shared.TxRunner = (*MongoTxRunner)(nil)
