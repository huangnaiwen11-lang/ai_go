package sessionauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestMongoSessionAuth封禁后旧会话立即失效(t *testing.T) {
	mongoURI := os.Getenv("CLING_TEST_MONGO_URI")
	if mongoURI == "" {
		t.Skip("CLING_TEST_MONGO_URI 未设置，跳过本机 rs0 集成测试")
	}
	dataConfig := &conf.Data{Mongo: &conf.Data_Mongo{
		Uri:                  mongoURI,
		Database:             "cling_main",
		ReplicaSet:           "rs0",
		TransactionsRequired: true,
	}}
	if err := conf.ValidateLocalMongo(dataConfig); err != nil {
		t.Fatalf("测试 Mongo 配置必须是本机 cling_main/rs0: %v", err)
	}

	storage, cleanupStorage, err := data.NewData(dataConfig)
	if err != nil {
		t.Fatalf("连接本机 MongoDB: %v", err)
	}
	defer cleanupStorage()

	seedClient, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		t.Fatalf("创建本机测试写入客户端: %v", err)
	}
	// Cleanup 按后进先出执行。先注册客户端释放，再注册精确文档删除，确保删除时
	// 测试写入客户端仍处于连接状态。
	t.Cleanup(func() { _ = seedClient.Disconnect(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := data.NewLocalSchemaInitializer(storage).Ensure(ctx); err != nil {
		t.Fatalf("初始化本机测试 schema: %v", err)
	}

	userID := uuid.NewString()
	sessionID := uuid.NewString()
	database := seedClient.Database("cling_main")
	users := database.Collection(schema.CollectionUsers)
	sessions := database.Collection(schema.CollectionSessions)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, deleteErr := sessions.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: sessionID}}); deleteErr != nil {
			t.Errorf("精确删除测试会话 %q: %v", sessionID, deleteErr)
		}
		if _, deleteErr := users.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: userID}}); deleteErr != nil {
			t.Errorf("精确删除测试用户 %q: %v", userID, deleteErr)
		}
	})

	now := time.Now().UTC()
	if _, err := users.InsertOne(ctx, model.UserDocument{
		ID:             userID,
		AccountStatus:  string(identity.AccountStatusNormal),
		BindingState:   string(identity.BindingStateBound),
		Timezone:       "Asia/Shanghai",
		SessionVersion: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("写入随机测试用户: %v", err)
	}
	if _, err := sessions.InsertOne(ctx, model.SessionDocument{
		ID:             sessionID,
		UserID:         userID,
		SessionVersion: 1,
		ExpiresAt:      now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("写入随机测试会话: %v", err)
	}

	identityUsecase := identity.NewUsecase(
		data.NewUserRepository(storage),
		data.NewIdentityRepository(storage),
		data.NewSessionRepository(storage),
		data.NewTxRunner(storage),
	)
	authenticator := NewAuthenticator(identityUsecase)
	request := httptest.NewRequest(http.MethodPost, "/candidate", nil)
	request.Header.Set("Authorization", "Bearer "+sessionID)
	if authenticated, authenticateErr := authenticator.Authenticate(request); authenticateErr != nil || authenticated == nil || authenticated.UserID != userID {
		t.Fatalf("封禁前 Authenticate() = %#v, %v，期望认证为 %q", authenticated, authenticateErr, userID)
	}

	if _, err := identityUsecase.ChangeAccountStatus(ctx, userID, identity.AccountStatusBanned); err != nil {
		t.Fatalf("封禁随机测试用户: %v", err)
	}
	if authenticated, authenticateErr := authenticator.Authenticate(request); !errors.Is(authenticateErr, shared.ErrUnauthenticated) || authenticated != nil {
		t.Fatalf("封禁后 Authenticate() = %#v, %v，期望 ErrUnauthenticated", authenticated, authenticateErr)
	}
}
