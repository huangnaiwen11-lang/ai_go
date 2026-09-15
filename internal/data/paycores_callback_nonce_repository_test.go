package data

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// 同一份已验签 nonce 只能由一个并发调用消费；持久化文档只能保留其摘要和回调边界事实。
func TestMongoPayCoresNonce并发消费只成功一次且不保存原始Nonce(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}
	rawNonce := "12345678-1234-4123-8123-" + uuid.NewString()[:12]
	nonceHash := paymentCallbackNonceHash(t, rawNonce)
	// TTL 索引由 Mongo 服务器时钟执行，使用足够靠后的相对时刻避免测试机与服务器时钟漂移导致读回前过期。
	expiresAt := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Millisecond)
	collection := database.Collection(schema.CollectionPaymentCallbackNonces)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = collection.DeleteOne(cleanupContext, bson.D{{Key: "nonce_hash", Value: nonceHash.Hex()}})
	})

	repository := NewPayCoresCallbackNonceRepository(&Data{database: database})
	const attempts = 12
	start := make(chan struct{})
	errorsByAttempt := make(chan error, attempts)
	var waitGroup sync.WaitGroup
	for range attempts {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			errorsByAttempt <- repository.Consume(context.Background(), nonceHash, "POST", "/api/v1/internal/payment-confirmed", expiresAt)
		}()
	}
	close(start)
	waitGroup.Wait()
	close(errorsByAttempt)

	successes := 0
	for err := range errorsByAttempt {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, payments.ErrPaymentCallbackReplayed) {
			t.Fatalf("Consume() error = %v，期望 ErrPaymentCallbackReplayed", err)
		}
	}
	if successes != 1 {
		t.Fatalf("并发 Consume() 成功次数 = %d，期望 1", successes)
	}

	var document bson.M
	if err := collection.FindOne(ctx, bson.D{{Key: "nonce_hash", Value: nonceHash.Hex()}}).Decode(&document); err != nil {
		t.Fatalf("读取已消费 nonce: %v", err)
	}
	if len(document) != 5 {
		t.Fatalf("nonce 文档字段 = %#v，期望仅保存 Mongo _id 和 4 个允许字段", document)
	}
	if document["nonce_hash"] != nonceHash.Hex() || document["method"] != "POST" || document["path"] != "/api/v1/internal/payment-confirmed" {
		t.Fatalf("nonce 文档 = %#v，期望只保留摘要、方法和路径", document)
	}
	storedExpiry, ok := document["expires_at"].(bson.DateTime)
	if !ok || !storedExpiry.Time().UTC().Equal(expiresAt) {
		t.Fatalf("nonce 文档 expires_at = %#v，期望 %s", document["expires_at"], expiresAt)
	}
	if _, found := document[rawNonce]; found {
		t.Fatalf("nonce 文档意外以原 nonce 为字段：%#v", document)
	}
	for _, value := range document {
		if stringValue, ok := value.(string); ok && stringValue == rawNonce {
			t.Fatalf("nonce 文档意外保存原 nonce：%#v", document)
		}
	}
}

// 存储不可用时必须传回可诊断的驱动错误，不能误判为 nonce 已消费。
func TestMongoPayCoresNonce存储不可用时失败关闭(t *testing.T) {
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?directConnection=true").SetServerSelectionTimeout(50 * time.Millisecond))
	if err != nil {
		t.Fatalf("创建不可用 Mongo 客户端: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	repository := NewPayCoresCallbackNonceRepository(&Data{database: client.Database("cling_main")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	nonceHash := paymentCallbackNonceHash(t, "12345678-1234-4123-8123-123456789abc")
	err = repository.Consume(ctx, nonceHash, "POST", "/api/internal/payment-confirmed", time.Now().UTC().Add(2*time.Minute))
	if err == nil {
		t.Fatal("存储不可用时 Consume() error = nil，期望真实错误")
	}
	if errors.Is(err, payments.ErrPaymentCallbackReplayed) {
		t.Fatalf("存储不可用时 Consume() error = %v，不能映射为重放", err)
	}
}

func TestMongoPayCoresNonce拒绝零值摘要且不访问Mongo(t *testing.T) {
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?directConnection=true").SetServerSelectionTimeout(50 * time.Millisecond))
	if err != nil {
		t.Fatalf("创建不可用 Mongo 客户端: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	repository := NewPayCoresCallbackNonceRepository(&Data{database: client.Database("cling_main")})
	err = repository.Consume(context.Background(), payments.PaymentCallbackNonceHash{}, "POST", "/api/internal/payment-confirmed", time.Now().UTC().Add(2*time.Minute))
	if !errors.Is(err, payments.ErrInvalidPaymentCallbackNonce) {
		t.Fatalf("零值摘要 Consume() error = %v，期望 ErrInvalidPaymentCallbackNonce", err)
	}
}

func paymentCallbackNonceHash(t *testing.T, nonce string) payments.PaymentCallbackNonceHash {
	t.Helper()
	nonceHash, err := payments.NewPaymentCallbackNonceHash(nonce)
	if err != nil {
		t.Fatalf("NewPaymentCallbackNonceHash() error = %v", err)
	}
	return nonceHash
}
