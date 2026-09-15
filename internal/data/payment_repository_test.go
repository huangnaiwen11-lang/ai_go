package data

import (
	"context"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 支付回执、账户余额和正向分录必须在同一个本地 Mongo 事务中提交。
func TestMongo支付回执首次入账并重放不重复加钻(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	transactionID := "paycores-" + uuid.NewString()
	accounts := database.Collection(schema.CollectionAccounts)
	receipts := database.Collection(schema.CollectionPaymentReceipts)
	entries := database.Collection(schema.CollectionLedgerEntries)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		var persisted model.PaymentReceiptDocument
		if err := receipts.FindOne(cleanupContext, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}}).Decode(&persisted); err == nil {
			if persisted.LedgerEntryID != "" {
				_, _ = entries.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: persisted.LedgerEntryID}})
			}
			_, _ = receipts.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: persisted.ID}})
		}
		_, _ = accounts.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: userID}})
	})

	businessAt := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	repository := NewPaymentRepository(&Data{client: client, database: database})
	service := payments.NewService(repository, func() time.Time {
		return businessAt
	})
	order := createMongoFrozenPaymentOrder(t, ctx, database, repository, userID, payments.ProviderPayCores, 100, businessAt)
	settlement := verifiedOrderSettlement(order, transactionID, businessAt)

	first, err := service.SettleVerifiedOrder(ctx, settlement)
	if err != nil {
		t.Fatalf("首次 SettleVerifiedOrder() error = %v", err)
	}
	if !first.Applied || first.DiamondBalance != 100 {
		t.Fatalf("首次入账结果 = %#v，期望已入账且余额为 100", first)
	}
	replay, err := service.SettleVerifiedOrder(ctx, settlement)
	if err != nil {
		t.Fatalf("重放 SettleVerifiedOrder() error = %v", err)
	}
	if replay.Applied || replay.DiamondBalance != 100 {
		t.Fatalf("重放结果 = %#v，期望不重复入账且余额为 100", replay)
	}

	var account model.AccountDocument
	if err := accounts.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil {
		t.Fatalf("读取本地账户: %v", err)
	}
	if account.DiamondBalance != 100 || !account.CreatedAt.Equal(businessAt) || !account.UpdatedAt.Equal(businessAt) {
		t.Fatalf("账户事实 = %#v，期望余额 100 且使用冻结业务时间", account)
	}

	var storedReceipt model.PaymentReceiptDocument
	if err := receipts.FindOne(ctx, bson.D{{Key: "external_transaction_id", Value: transactionID}}).Decode(&storedReceipt); err != nil {
		t.Fatalf("读取支付回执: %v", err)
	}
	if storedReceipt.PaymentOrderID != order.ID || storedReceipt.UserID != userID || storedReceipt.DiamondAmount != 100 || !storedReceipt.ReceivedAt.Equal(businessAt) || storedReceipt.LedgerEntryID == "" {
		t.Fatalf("支付回执事实 = %#v，期望持久化用户、钻石、时间和账本关联", storedReceipt)
	}

	var entry model.LedgerEntryDocument
	if err := entries.FindOne(ctx, bson.D{{Key: "_id", Value: storedReceipt.LedgerEntryID}}).Decode(&entry); err != nil {
		t.Fatalf("读取支付账本分录: %v", err)
	}
	if entry.AccountID != userID || entry.DeltaDiamonds != 100 || entry.Reason != "payment_credit" || !entry.CreatedAt.Equal(businessAt) {
		t.Fatalf("支付账本分录 = %#v，期望本地正向 payment_credit 分录", entry)
	}
}

// 支付写操作只能在同一 Mongo 事务中执行，不能允许其他调用方拆开提交回执、余额和账本。
func TestMongo支付仓储拒绝事务外写入(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	transactionID := "paycores-transaction-outside-tx-" + uuid.NewString()
	receipts := database.Collection(schema.CollectionPaymentReceipts)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = receipts.DeleteOne(cleanupContext, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}})
	})

	repository := NewPaymentRepository(&Data{client: client, database: database})
	err := repository.CreateReceipt(ctx, payments.Receipt{
		Provider:              payments.ProviderPayCores,
		ExternalTransactionID: transactionID,
		PaymentOrderID:        "payment-order-outside-tx",
		UserID:                uuid.NewString(),
		DiamondAmount:         100,
		BusinessAt:            time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("事务外 CreateReceipt() error = nil，期望拒绝事务外写入")
	}
	count, countErr := receipts.CountDocuments(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}})
	if countErr != nil || count != 0 {
		t.Fatalf("事务外写入后回执数 = %d, error = %v，期望 0", count, countErr)
	}
}

// 多个并发事务处理同一渠道交易时，Mongo 唯一索引和事务重试必须收敛为一次入账。
func TestMongo支付回执并发重放只入账一次(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	transactionID := "paycores-concurrent-" + uuid.NewString()
	accounts := database.Collection(schema.CollectionAccounts)
	receipts := database.Collection(schema.CollectionPaymentReceipts)
	entries := database.Collection(schema.CollectionLedgerEntries)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		var persisted model.PaymentReceiptDocument
		if err := receipts.FindOne(cleanupContext, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}}).Decode(&persisted); err == nil {
			if persisted.LedgerEntryID != "" {
				_, _ = entries.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: persisted.LedgerEntryID}})
			}
			_, _ = receipts.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: persisted.ID}})
		}
		_, _ = accounts.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: userID}})
	})

	serviceRepository := NewPaymentRepository(&Data{client: client, database: database})
	service := payments.NewService(serviceRepository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	order := createMongoFrozenPaymentOrder(t, ctx, database, serviceRepository, userID, payments.ProviderPayCores, 100, time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	settlement := verifiedOrderSettlement(order, transactionID, time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))

	const callers = 4
	start := make(chan struct{})
	results := make(chan payments.ApplyResult, callers)
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := service.SettleVerifiedOrder(context.Background(), settlement)
			results <- result
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)

	appliedCount := 0
	for err := range errors {
		if err != nil {
			t.Fatalf("并发 SettleVerifiedOrder() error = %v", err)
		}
	}
	for result := range results {
		if result.Applied {
			appliedCount++
		}
		if result.DiamondBalance != 100 {
			t.Fatalf("并发结果 = %#v，期望余额为 100", result)
		}
	}
	if appliedCount != 1 {
		t.Fatalf("并发已入账调用数 = %d，期望 1", appliedCount)
	}

	var account model.AccountDocument
	if err := accounts.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account); err != nil || account.DiamondBalance != 100 {
		t.Fatalf("并发后的账户 = %#v, error = %v，期望余额 100", account, err)
	}
	count, err := receipts.CountDocuments(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}})
	if err != nil || count != 1 {
		t.Fatalf("并发后的支付回执数 = %d, error = %v，期望 1", count, err)
	}
}

// 正向账本分录无法写入时，Mongo 事务必须回滚此前的回执插入和账户 upsert。
func TestMongo支付账本重复键时回滚回执与余额(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化本地 schema: %v", err)
	}

	userID := uuid.NewString()
	transactionID := "paycores-ledger-conflict-" + uuid.NewString()
	ledgerEntryID := paymentCreditLedgerEntryID(payments.ProviderPayCores, transactionID)
	accounts := database.Collection(schema.CollectionAccounts)
	receipts := database.Collection(schema.CollectionPaymentReceipts)
	entries := database.Collection(schema.CollectionLedgerEntries)
	if _, err := entries.InsertOne(ctx, model.LedgerEntryDocument{
		ID:             ledgerEntryID,
		IdempotencyKey: ledgerEntryID,
		AccountID:      "seed-account",
		DeltaDiamonds:  1,
		Reason:         "test_seed",
		CreatedAt:      time.Date(2026, time.September, 9, 11, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("写入冲突账本种子: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = entries.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: ledgerEntryID}})
		_, _ = receipts.DeleteOne(cleanupContext, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}})
		_, _ = accounts.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: userID}})
	})

	serviceRepository := NewPaymentRepository(&Data{client: client, database: database})
	service := payments.NewService(serviceRepository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	order := createMongoFrozenPaymentOrder(t, ctx, database, serviceRepository, userID, payments.ProviderPayCores, 100, time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC))
	_, err := service.SettleVerifiedOrder(ctx, verifiedOrderSettlement(order, transactionID, time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)))
	if err == nil {
		t.Fatal("SettleVerifiedOrder() error = nil，期望账本重复键导致事务失败")
	}

	accountCount, accountErr := accounts.CountDocuments(ctx, bson.D{{Key: "_id", Value: userID}})
	if accountErr != nil || accountCount != 0 {
		t.Fatalf("失败后的账户数 = %d, error = %v，期望 0", accountCount, accountErr)
	}
	receiptCount, receiptErr := receipts.CountDocuments(ctx, bson.D{{Key: "provider", Value: string(payments.ProviderPayCores)}, {Key: "external_transaction_id", Value: transactionID}})
	if receiptErr != nil || receiptCount != 0 {
		t.Fatalf("失败后的支付回执数 = %d, error = %v，期望 0", receiptCount, receiptErr)
	}
	var seededEntry model.LedgerEntryDocument
	if err := entries.FindOne(ctx, bson.D{{Key: "_id", Value: ledgerEntryID}}).Decode(&seededEntry); err != nil || seededEntry.Reason != "test_seed" {
		t.Fatalf("冲突前的账本种子 = %#v, error = %v，期望保持原状", seededEntry, err)
	}
}

// createMongoFrozenPaymentOrder 通过真实商品版本和订单事务准备结算测试所需的本地冻结事实。
func createMongoFrozenPaymentOrder(t *testing.T, ctx context.Context, database *mongo.Database, repository payments.PaymentRepository, userID string, provider payments.Provider, diamondAmount int64, createdAt time.Time) payments.PaymentOrder {
	t.Helper()
	product := payments.PaymentProduct{
		ID:            "coins-" + uuid.NewString(),
		Version:       1,
		DiamondAmount: diamondAmount,
		PublishStatus: payments.ProductPublishStatusPublished,
	}
	orderID := "payment-order-" + uuid.NewString()
	if err := repository.CreateProduct(ctx, product); err != nil {
		t.Fatalf("CreateProduct() error = %v", err)
	}
	order, err := payments.FreezeOrder(payments.CreateOrderInput{
		OrderID:         orderID,
		UserID:          userID,
		Provider:        provider,
		ProviderOrderID: "provider-order-" + uuid.NewString(),
	}, product, createdAt)
	if err != nil {
		t.Fatalf("FreezeOrder() error = %v", err)
	}
	if err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, *order)
	}); err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		_, _ = database.Collection(schema.CollectionPaymentOrders).DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: order.ID}})
		_, _ = database.Collection(schema.CollectionPaymentProducts).DeleteOne(cleanupContext, bson.D{{Key: "product_id", Value: product.ID}, {Key: "version", Value: product.Version}})
	})
	return *order
}

func verifiedOrderSettlement(order payments.PaymentOrder, externalTransactionID string, businessAt time.Time) payments.VerifiedOrderSettlement {
	return payments.VerifiedOrderSettlement{
		PaymentOrderID:        order.ID,
		UserID:                order.UserID,
		Provider:              order.Provider,
		ProviderOrderID:       order.ProviderOrderID,
		ExternalTransactionID: externalTransactionID,
		BusinessAt:            businessAt,
	}
}
