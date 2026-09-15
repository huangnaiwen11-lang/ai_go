// Command local-payment-product-fixture 准备仅供本机钱包页面联调的支付商品快照。
//
// 它只允许连接本机 MongoDB，不调用 PayCores、商店内购或任何真实支付渠道；若发现
// 同一商品版本已存在但内容不同，会明确失败而不是改写已有事实。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	defaultLocalPaymentMongoURI = "mongodb://127.0.0.1:27017/?replicaSet=rs0&directConnection=true"
	localPaymentFixtureTimeout  = 10 * time.Second
)

func main() {
	mongoURI := flag.String("mongo-uri", defaultLocalPaymentMongoURI, "仅允许本机 rs0 MongoDB 连接串")
	flag.Parse()

	config, err := localPaymentMongoConfig(*mongoURI)
	if err != nil {
		fmt.Fprintln(os.Stderr, "本地 MongoDB 配置无效。")
		os.Exit(2)
	}
	created, err := seedLocalPaymentProduct(context.Background(), config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "本地支付商品快照初始化失败。")
		os.Exit(1)
	}
	if created {
		fmt.Println("已创建本地钱包联调商品：100 钻石（USD 9.99）。")
		return
	}
	fmt.Println("本地钱包联调商品已存在且快照一致，未作任何改写。")
}

// localPaymentMongoConfig 固化本地 fixture 的数据库、事务副本集与连接隔离约束。
func localPaymentMongoConfig(uri string) (*conf.Data, error) {
	config := &conf.Data{Mongo: &conf.Data_Mongo{
		Uri:                  uri,
		Database:             "cling_main",
		ReplicaSet:           "rs0",
		TransactionsRequired: true,
	}}
	if err := conf.ValidateLocalMongo(config); err != nil {
		return nil, err
	}
	return config, nil
}

// localPaymentProduct 是固定且可审计的本地商品版本；前端只能读取它，不能提交金额或钻石数。
func localPaymentProduct() payments.PaymentProduct {
	return payments.PaymentProduct{
		ID:            "local_coins_100",
		Version:       1,
		DiamondAmount: 100,
		AmountCents:   999,
		Currency:      "USD",
		Label:         "100 钻石（本地测试）",
		PublishStatus: payments.ProductPublishStatusPublished,
	}
}

// seedLocalPaymentProduct 建索引后写入一个不可覆盖的商品快照；它不创建订单或账本分录。
func seedLocalPaymentProduct(parent context.Context, config *conf.Data) (bool, error) {
	if config == nil || config.GetMongo() == nil {
		return false, errors.New("local MongoDB config is required")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(config.GetMongo().GetUri()))
	if err != nil {
		return false, fmt.Errorf("connect local MongoDB: %w", err)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	ctx, cancel := context.WithTimeout(parent, localPaymentFixtureTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		return false, fmt.Errorf("ping local MongoDB: %w", err)
	}
	database := client.Database(config.GetMongo().GetDatabase())
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		return false, fmt.Errorf("initialize local MongoDB schema: %w", err)
	}
	return ensureLocalPaymentProduct(ctx, database, localPaymentProduct())
}

// ensureLocalPaymentProduct 只新增缺失的固定版本；已存在的内容必须逐字段一致，防止本地
// 联调命令意外覆盖手工建立的价格、钻石数或发布状态。
func ensureLocalPaymentProduct(ctx context.Context, database *mongo.Database, product payments.PaymentProduct) (bool, error) {
	if database == nil {
		return false, errors.New("local MongoDB database is required")
	}
	if err := product.Validate(); err != nil {
		return false, fmt.Errorf("validate local payment product: %w", err)
	}
	products := database.Collection(schema.CollectionPaymentProducts)
	filter := bson.D{{Key: "product_id", Value: product.ID}, {Key: "version", Value: product.Version}}

	var existing model.PaymentProductDocument
	err := products.FindOne(ctx, filter).Decode(&existing)
	if err == nil {
		if paymentProductDocumentMatchesFixture(existing, product) {
			return false, nil
		}
		return false, fmt.Errorf("local payment product %q version %d already exists with different snapshot", product.ID, product.Version)
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return false, fmt.Errorf("find local payment product %q version %d: %w", product.ID, product.Version, err)
	}

	document := model.PaymentProductDocument{
		ProductID:     product.ID,
		Version:       product.Version,
		DiamondAmount: product.DiamondAmount,
		AmountCents:   product.AmountCents,
		Currency:      product.Currency,
		Label:         product.Label,
		PublishStatus: string(product.PublishStatus),
	}
	if _, err := products.InsertOne(ctx, document); err == nil {
		return true, nil
	} else if !mongo.IsDuplicateKeyError(err) {
		return false, fmt.Errorf("insert local payment product %q version %d: %w", product.ID, product.Version, err)
	}

	// 并发执行时，唯一索引会替我们裁定哪一个进程创建；随后只接受完全相同的快照。
	if err := products.FindOne(ctx, filter).Decode(&existing); err != nil {
		return false, fmt.Errorf("read concurrently created local payment product %q version %d: %w", product.ID, product.Version, err)
	}
	if !paymentProductDocumentMatchesFixture(existing, product) {
		return false, fmt.Errorf("local payment product %q version %d was concurrently created with different snapshot", product.ID, product.Version)
	}
	return false, nil
}

// paymentProductDocumentMatchesFixture 用严格全字段比较确认已存在商品就是预期的不可变快照。
func paymentProductDocumentMatchesFixture(document model.PaymentProductDocument, product payments.PaymentProduct) bool {
	return document.ProductID == product.ID &&
		document.Version == product.Version &&
		document.DiamondAmount == product.DiamondAmount &&
		document.AmountCents == product.AmountCents &&
		document.Currency == product.Currency &&
		document.Label == product.Label &&
		document.PublishStatus == string(product.PublishStatus)
}
