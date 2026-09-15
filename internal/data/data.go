// Package data 实现各业务模块声明的仓储接口，并拥有 MongoDB 持久化客户端。
package data

import (
	"context"
	"fmt"
	"time"

	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/conf"

	"github.com/google/wire"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const mongoConnectionTimeout = 5 * time.Second

// Data 持有进程级 MongoDB 客户端和 Go 主站独立数据库句柄。
// Client 不向 biz 或 service 层导出；业务模块只能依赖仓储接口和 shared.TxRunner。
type Data struct {
	client   *mongo.Client
	database *mongo.Database
}

// NewData 创建并验证独立 cling_main 的 MongoDB 客户端。
// 连接前复用配置校验，避免独立命令或测试绕过启动入口直连到非本地 MongoDB。
func NewData(config *conf.Data) (*Data, func(), error) {
	if err := conf.ValidateLocalMongo(config); err != nil {
		return nil, nil, fmt.Errorf("validate local MongoDB config: %w", err)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(config.GetMongo().GetUri()))
	if err != nil {
		return nil, nil, fmt.Errorf("create MongoDB client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), mongoConnectionTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, fmt.Errorf("ping MongoDB: %w", err)
	}

	data := &Data{
		client:   client,
		database: client.Database(config.GetMongo().GetDatabase()),
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), mongoConnectionTimeout)
		defer cancel()
		_ = client.Disconnect(ctx)
	}
	return data, cleanup, nil
}

// NewTxRunner 将 MongoDB 事务能力以业务无关的接口注入上层。
func NewTxRunner(data *Data) shared.TxRunner {
	return NewMongoTxRunner(data.client)
}

// ProviderSet 是持久化层的依赖注入入口。
// 禁止在此处保留 Todo 或其他示例仓储，具体仓储必须归属到明确的业务模块。
var ProviderSet = wire.NewSet(
	NewData,
	NewTxRunner,
	NewLocalSchemaInitializer,
	NewUserRepository,
	NewAuthUserRepository,
	NewIdentityRepository,
	NewSessionRepository,
	NewAuthSessionRepository,
	NewCredentialRepository,
	NewAccountRepository,
	NewTemplateRepository,
	NewSubscriptionRepository,
	NewCreationRepository,
	NewLedgerRepository,
	NewOutboxRepository,
	NewGenerationSubmissionRepository,
	NewGenerationCallbackRepository,
	NewGenerationCallbackStore,
	NewGenerationCallbackLinker,
	NewFeedbackRepository,
	NewNotificationRepository,
)
