package data

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"ai-business-service/internal/conf"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const testMongoURIEnv = "CLING_TEST_MONGO_URI"

func TestNewMongoTestContextHasFiveSecondDeadline(t *testing.T) {
	ctx, cancel := newMongoTestContext()
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("MongoDB 测试 context 缺少 deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second || remaining < 4*time.Second {
		t.Fatalf("MongoDB 测试 context 剩余时间 = %s，要求接近 5 秒", remaining)
	}
}

func TestMongoTxRunnerRollsBackAllWritesWhenCallbackFails(t *testing.T) {
	client := newLocalMongoClient(t)
	collection := client.Database("cling_main").Collection("tx_runner_test_" + uuid.NewString())
	runner := NewMongoTxRunner(client)
	callbackErr := errors.New("模拟预扣后的业务失败")
	ctx, cancel := newMongoTestContext()
	defer cancel()

	err := runner.WithinTx(ctx, func(txContext context.Context) error {
		if _, err := collection.InsertOne(txContext, bson.M{"state": "held"}); err != nil {
			return err
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("WithinTx() error = %v, want callback error", err)
	}

	count, err := collection.CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if count != 0 {
		t.Fatalf("document count = %d, want 0 after transaction rollback", count)
	}
}

func newMongoTestContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// newLocalMongoClient 只连接测试显式指定且经同一配置校验的本地副本集。
// 未提供地址时跳过集成测试，避免日常单元测试意外连接任何环境。
func newLocalMongoClient(t *testing.T) *mongo.Client {
	t.Helper()
	uri := os.Getenv(testMongoURIEnv)
	if uri == "" {
		t.Skipf("set %s to run MongoDB transaction integration tests", testMongoURIEnv)
	}
	if err := conf.ValidateLocalMongo(&conf.Data{
		Mongo: &conf.Data_Mongo{
			Uri:                  uri,
			Database:             "cling_main",
			ReplicaSet:           "rs0",
			TransactionsRequired: true,
		},
	}); err != nil {
		t.Fatalf("unsafe MongoDB test URI: %v", err)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect local MongoDB: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Disconnect(ctx); err != nil {
			t.Errorf("disconnect local MongoDB: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("ping local MongoDB: %v", err)
	}
	return client
}
