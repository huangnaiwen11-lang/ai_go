package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 已发布商品的版本和订单冻结快照都只能由本地 Mongo 保存，后续商品变更不能改写既有订单。
func TestMongo支付商品版本与冻结订单快照(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	productID := "coins-" + uuid.NewString()
	orderID := "order-" + uuid.NewString()
	providerOrderID := "provider-order-" + uuid.NewString()
	products := database.Collection(schema.CollectionPaymentProducts)
	orders := database.Collection(schema.CollectionPaymentOrders)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = products.DeleteMany(cleanupContext, bson.D{{Key: "product_id", Value: productID}})
		_, _ = orders.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: orderID}})
	})

	repository := NewPaymentRepository(&Data{client: client, database: database})
	productV1 := payments.PaymentProduct{
		ID:            productID,
		Version:       1,
		DiamondAmount: 100,
		PublishStatus: payments.ProductPublishStatusPublished,
	}
	if err := repository.CreateProduct(ctx, productV1); err != nil {
		t.Fatalf("CreateProduct() error = %v", err)
	}
	storedProduct, err := repository.FindProduct(ctx, productID, 1)
	if err != nil {
		t.Fatalf("FindProduct() error = %v", err)
	}
	if storedProduct == nil || *storedProduct != productV1 {
		t.Fatalf("保存的商品 = %#v，期望 %#v", storedProduct, productV1)
	}

	createdAt := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	order, err := payments.FreezeOrder(payments.CreateOrderInput{
		OrderID:         orderID,
		UserID:          "user-" + uuid.NewString(),
		Provider:        payments.ProviderPayCores,
		ProviderOrderID: providerOrderID,
	}, productV1, createdAt)
	if err != nil {
		t.Fatalf("FreezeOrder() error = %v", err)
	}
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, *order)
	}); err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}

	productV2 := productV1
	productV2.Version = 2
	productV2.DiamondAmount = 200
	if err := repository.CreateProduct(ctx, productV2); err != nil {
		t.Fatalf("CreateProduct() 保存新版本 error = %v", err)
	}
	storedOrder, err := repository.FindOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("FindOrder() error = %v", err)
	}
	if storedOrder == nil || storedOrder.ProductID != productID || storedOrder.ProductVersion != 1 || storedOrder.DiamondAmount != 100 {
		t.Fatalf("冻结订单 = %#v，期望保留 v1 的商品标识、版本和钻石数", storedOrder)
	}

	var document model.PaymentOrderDocument
	if err := orders.FindOne(ctx, bson.D{{Key: "_id", Value: orderID}}).Decode(&document); err != nil {
		t.Fatalf("读取订单文档: %v", err)
	}
	if document.ProductID != productID || document.ProductVersion != 1 || document.DiamondAmount != 100 {
		t.Fatalf("订单文档 = %#v，期望完整保存商品冻结快照", document)
	}
}

// 草稿商品即使已经持久化，也不能被转换为可写入的冻结订单。
func TestMongo支付草稿商品拒绝创建冻结订单(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	productID := "draft-coins-" + uuid.NewString()
	products := database.Collection(schema.CollectionPaymentProducts)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = products.DeleteMany(cleanupContext, bson.D{{Key: "product_id", Value: productID}})
	})

	repository := NewPaymentRepository(&Data{client: client, database: database})
	draft := payments.PaymentProduct{ID: productID, Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusDraft}
	if err := repository.CreateProduct(ctx, draft); err != nil {
		t.Fatalf("CreateProduct() 保存草稿 error = %v", err)
	}
	_, err := payments.FreezeOrder(payments.CreateOrderInput{
		OrderID:         "order-" + uuid.NewString(),
		UserID:          "user-" + uuid.NewString(),
		Provider:        payments.ProviderPayCores,
		ProviderOrderID: "provider-order-" + uuid.NewString(),
	}, draft, time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	if !errors.Is(err, payments.ErrProductNotPublished) {
		t.Fatalf("FreezeOrder() error = %v，期望 ErrProductNotPublished", err)
	}
}

// 写入订单必须附着在活跃事务上，防止未来的回执、状态迁移和入账被拆分提交。
func TestMongo支付订单拒绝事务外写入(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	orderID := "order-outside-tx-" + uuid.NewString()
	orders := database.Collection(schema.CollectionPaymentOrders)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = orders.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: orderID}})
	})

	repository := NewPaymentRepository(&Data{client: client, database: database})
	err := repository.CreateOrder(ctx, payments.PaymentOrder{
		ID:              orderID,
		UserID:          "user-" + uuid.NewString(),
		Provider:        payments.ProviderPayCores,
		ProviderOrderID: "provider-order-" + uuid.NewString(),
		ProductID:       "coins-100",
		ProductVersion:  1,
		DiamondAmount:   100,
		Status:          payments.PaymentOrderStatusPending,
		CreatedAt:       time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("事务外 CreateOrder() error = nil，期望拒绝")
	}
	count, countErr := orders.CountDocuments(ctx, bson.D{{Key: "_id", Value: orderID}})
	if countErr != nil || count != 0 {
		t.Fatalf("事务外订单数 = %d, error = %v，期望 0", count, countErr)
	}
}

// 同一支付渠道和渠道订单号只能冻结一次，唯一索引必须在并列事务间保持该约束。
func TestMongo支付订单拒绝重复渠道订单号(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	providerOrderID := "provider-order-" + uuid.NewString()
	productID := "coins-" + uuid.NewString()
	products := database.Collection(schema.CollectionPaymentProducts)
	orders := database.Collection(schema.CollectionPaymentOrders)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = products.DeleteOne(cleanupContext, bson.D{{Key: "product_id", Value: productID}, {Key: "version", Value: 1}})
		_, _ = orders.DeleteMany(cleanupContext, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "provider_order_id", Value: providerOrderID}})
	})

	repository := NewPaymentRepository(&Data{client: client, database: database})
	if err := repository.CreateProduct(ctx, payments.PaymentProduct{ID: productID, Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusPublished}); err != nil {
		t.Fatalf("CreateProduct() 种子 error = %v", err)
	}
	newOrder := func(id string) payments.PaymentOrder {
		at := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
		return payments.PaymentOrder{ID: id, UserID: "user-" + uuid.NewString(), Provider: payments.ProviderPayCores, ProviderOrderID: providerOrderID, ProductID: productID, ProductVersion: 1, DiamondAmount: 100, Status: payments.PaymentOrderStatusPending, CreatedAt: at, UpdatedAt: at}
	}
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, newOrder("order-"+uuid.NewString()))
	}); err != nil {
		t.Fatalf("首次 CreateOrder() error = %v", err)
	}
	err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, newOrder("order-"+uuid.NewString()))
	})
	if !errors.Is(err, payments.ErrPaymentOrderAlreadyExists) {
		t.Fatalf("重复 CreateOrder() error = %v，期望 ErrPaymentOrderAlreadyExists", err)
	}
	count, countErr := orders.CountDocuments(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "provider_order_id", Value: providerOrderID}})
	if countErr != nil || count != 1 {
		t.Fatalf("重复写入后的订单数 = %d, error = %v，期望 1", count, countErr)
	}
}

// 订单只允许在事务内从 pending 原子迁移为 paid，重复迁移不能覆盖已经结算的订单事实。
func TestMongo支付订单原子迁移为已支付(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	orderID := "order-transition-" + uuid.NewString()
	productID := "coins-" + uuid.NewString()
	products := database.Collection(schema.CollectionPaymentProducts)
	orders := database.Collection(schema.CollectionPaymentOrders)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = products.DeleteOne(cleanupContext, bson.D{{Key: "product_id", Value: productID}, {Key: "version", Value: 1}})
		_, _ = orders.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: orderID}})
	})

	repository := NewPaymentRepository(&Data{client: client, database: database})
	if err := repository.CreateProduct(ctx, payments.PaymentProduct{ID: productID, Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusPublished}); err != nil {
		t.Fatalf("CreateProduct() 种子 error = %v", err)
	}
	createdAt := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	order := payments.PaymentOrder{ID: orderID, UserID: "user-" + uuid.NewString(), Provider: payments.ProviderPayCores, ProviderOrderID: "provider-order-" + uuid.NewString(), ProductID: productID, ProductVersion: 1, DiamondAmount: 100, Status: payments.PaymentOrderStatusPending, CreatedAt: createdAt, UpdatedAt: createdAt}
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, order)
	}); err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}

	paidAt := createdAt.Add(time.Minute)
	var transitioned bool
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		transitioned, err = repository.MarkOrderPaid(txCtx, orderID, paidAt)
		return err
	}); err != nil {
		t.Fatalf("首次 MarkOrderPaid() error = %v", err)
	}
	if !transitioned {
		t.Fatal("首次 MarkOrderPaid() transitioned = false，期望 pending → paid")
	}
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		transitioned, err = repository.MarkOrderPaid(txCtx, orderID, paidAt.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("重复 MarkOrderPaid() error = %v", err)
	}
	if transitioned {
		t.Fatal("重复 MarkOrderPaid() transitioned = true，期望仅迁移一次")
	}

	stored, err := repository.FindOrder(ctx, orderID)
	if err != nil {
		t.Fatalf("FindOrder() error = %v", err)
	}
	if stored == nil || stored.Status != payments.PaymentOrderStatusPaid || !stored.UpdatedAt.Equal(paidAt) {
		t.Fatalf("迁移后的订单 = %#v，期望状态 paid 且更新时间为首次迁移时间", stored)
	}
}

// 订单状态迁移不能在事务外提交，失败后必须保持原有 pending 冻结事实。
func TestMongo支付订单拒绝事务外迁移为已支付(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	orderID := "order-transition-outside-tx-" + uuid.NewString()
	productID := "coins-" + uuid.NewString()
	products := database.Collection(schema.CollectionPaymentProducts)
	orders := database.Collection(schema.CollectionPaymentOrders)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = products.DeleteOne(cleanupContext, bson.D{{Key: "product_id", Value: productID}, {Key: "version", Value: 1}})
		_, _ = orders.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: orderID}})
	})

	repository := NewPaymentRepository(&Data{client: client, database: database})
	if err := repository.CreateProduct(ctx, payments.PaymentProduct{ID: productID, Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusPublished}); err != nil {
		t.Fatalf("CreateProduct() 种子 error = %v", err)
	}
	createdAt := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	order := payments.PaymentOrder{ID: orderID, UserID: "user-" + uuid.NewString(), Provider: payments.ProviderPayCores, ProviderOrderID: "provider-order-" + uuid.NewString(), ProductID: productID, ProductVersion: 1, DiamondAmount: 100, Status: payments.PaymentOrderStatusPending, CreatedAt: createdAt, UpdatedAt: createdAt}
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, order)
	}); err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}

	transitioned, err := repository.MarkOrderPaid(ctx, orderID, createdAt.Add(time.Minute))
	if err == nil {
		t.Fatal("事务外 MarkOrderPaid() error = nil，期望拒绝")
	}
	if transitioned {
		t.Fatal("事务外 MarkOrderPaid() transitioned = true，期望 false")
	}

	stored, findErr := repository.FindOrder(ctx, orderID)
	if findErr != nil {
		t.Fatalf("FindOrder() error = %v", findErr)
	}
	if stored == nil || stored.Status != payments.PaymentOrderStatusPending || !stored.UpdatedAt.Equal(createdAt) {
		t.Fatalf("事务外迁移后的订单 = %#v，期望保持 pending 和原更新时间", stored)
	}
}

// 直接构造订单也必须满足冻结状态机的本地事实，不能绕过已发布商品版本和金额快照校验。
func TestMongo支付订单拒绝非法冻结事实(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewPaymentRepository(&Data{client: client, database: database})
	products := database.Collection(schema.CollectionPaymentProducts)
	orders := database.Collection(schema.CollectionPaymentOrders)
	createdAt := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		product *payments.PaymentProduct
		order   func() payments.PaymentOrder
	}{
		{
			name:    "已支付状态",
			product: &payments.PaymentProduct{ID: "coins-" + uuid.NewString(), Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusPublished},
			order: func() payments.PaymentOrder {
				return payments.PaymentOrder{ID: "order-" + uuid.NewString(), UserID: "user-" + uuid.NewString(), Provider: payments.ProviderPayCores, ProviderOrderID: "provider-order-" + uuid.NewString(), ProductID: "", ProductVersion: 1, DiamondAmount: 100, Status: payments.PaymentOrderStatusPaid, CreatedAt: createdAt, UpdatedAt: createdAt}
			},
		},
		{
			name: "未知商品",
			order: func() payments.PaymentOrder {
				return payments.PaymentOrder{ID: "order-" + uuid.NewString(), UserID: "user-" + uuid.NewString(), Provider: payments.ProviderPayCores, ProviderOrderID: "provider-order-" + uuid.NewString(), ProductID: "unknown-" + uuid.NewString(), ProductVersion: 1, DiamondAmount: 100, Status: payments.PaymentOrderStatusPending, CreatedAt: createdAt, UpdatedAt: createdAt}
			},
		},
		{
			name:    "草稿商品",
			product: &payments.PaymentProduct{ID: "coins-" + uuid.NewString(), Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusDraft},
			order: func() payments.PaymentOrder {
				return payments.PaymentOrder{ID: "order-" + uuid.NewString(), UserID: "user-" + uuid.NewString(), Provider: payments.ProviderPayCores, ProviderOrderID: "provider-order-" + uuid.NewString(), ProductID: "", ProductVersion: 1, DiamondAmount: 100, Status: payments.PaymentOrderStatusPending, CreatedAt: createdAt, UpdatedAt: createdAt}
			},
		},
		{
			name:    "冻结金额不匹配",
			product: &payments.PaymentProduct{ID: "coins-" + uuid.NewString(), Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusPublished},
			order: func() payments.PaymentOrder {
				return payments.PaymentOrder{ID: "order-" + uuid.NewString(), UserID: "user-" + uuid.NewString(), Provider: payments.ProviderPayCores, ProviderOrderID: "provider-order-" + uuid.NewString(), ProductID: "", ProductVersion: 1, DiamondAmount: 200, Status: payments.PaymentOrderStatusPending, CreatedAt: createdAt, UpdatedAt: createdAt}
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			order := testCase.order()
			if testCase.product != nil {
				product := *testCase.product
				if err := repository.CreateProduct(ctx, product); err != nil {
					t.Fatalf("CreateProduct() 种子 error = %v", err)
				}
				order.ProductID = product.ID
				t.Cleanup(func() {
					cleanupContext, cleanupCancel := newMongoTestContext()
					defer cleanupCancel()
					_, _ = products.DeleteOne(cleanupContext, bson.D{{Key: "product_id", Value: product.ID}, {Key: "version", Value: product.Version}})
				})
			}
			t.Cleanup(func() {
				cleanupContext, cleanupCancel := newMongoTestContext()
				defer cleanupCancel()
				_, _ = orders.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: order.ID}})
			})

			err := repository.WithinTx(ctx, func(txCtx context.Context) error {
				return repository.CreateOrder(txCtx, order)
			})
			if !errors.Is(err, payments.ErrInvalidPaymentOrder) && !errors.Is(err, payments.ErrProductNotPublished) {
				t.Fatalf("CreateOrder() error = %v，期望拒绝非法冻结事实", err)
			}
			count, countErr := orders.CountDocuments(ctx, bson.D{{Key: "_id", Value: order.ID}})
			if countErr != nil || count != 0 {
				t.Fatalf("被拒绝后的订单数 = %d, error = %v，期望 0", count, countErr)
			}
		})
	}
}

// 商品版本是订单快照的可信来源，非法商品事实不能被持久化。
func TestMongo支付商品拒绝非法版本(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	repository := NewPaymentRepository(&Data{client: client, database: database})
	products := database.Collection(schema.CollectionPaymentProducts)
	tests := []struct {
		name    string
		product payments.PaymentProduct
	}{
		{name: "空商品标识", product: payments.PaymentProduct{Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusPublished}},
		{name: "非正版本", product: payments.PaymentProduct{ID: "coins-" + uuid.NewString(), Version: 0, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatusPublished}},
		{name: "非正钻石数", product: payments.PaymentProduct{ID: "coins-" + uuid.NewString(), Version: 1, DiamondAmount: 0, PublishStatus: payments.ProductPublishStatusPublished}},
		{name: "未知发布状态", product: payments.PaymentProduct{ID: "coins-" + uuid.NewString(), Version: 1, DiamondAmount: 100, PublishStatus: payments.ProductPublishStatus("retired")}},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Cleanup(func() {
				cleanupContext, cleanupCancel := newMongoTestContext()
				defer cleanupCancel()
				_, _ = products.DeleteOne(cleanupContext, bson.D{{Key: "product_id", Value: testCase.product.ID}, {Key: "version", Value: testCase.product.Version}})
			})
			err := repository.CreateProduct(ctx, testCase.product)
			if err == nil {
				t.Fatal("CreateProduct() error = nil，期望拒绝非法商品版本")
			}
			count, countErr := products.CountDocuments(ctx, bson.D{{Key: "product_id", Value: testCase.product.ID}, {Key: "version", Value: testCase.product.Version}})
			if countErr != nil || count != 0 {
				t.Fatalf("被拒绝后的商品数 = %d, error = %v，期望 0", count, countErr)
			}
		})
	}
}
