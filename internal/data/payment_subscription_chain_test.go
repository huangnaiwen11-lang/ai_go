package data

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/adminview"
	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestMongoPaymentSubscriptionAcceptanceChain 验证本地支付与后台订阅权益的连续验收链：
// 商品展示 → PayCores fake 建单 → 已验签回调入账 → 后台订阅发放 → 有效期内续期。
//
// PayCores 只使用进程内 fake，回调也从受控 confirmation 对象进入领域层；测试不会
// 读取或发送真实支付密钥、不会访问 Apple/Google/PayCores 网络端点。
func TestMongoPaymentSubscriptionAcceptanceChain(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("payment_subscription_chain_" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	ctx, cancel := newMongoTestContext()
	defer cancel()
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if err := database.Drop(cleanupContext); err != nil {
			t.Errorf("drop isolated payment/subscription database: %v", err)
		}
	})
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("初始化隔离 Mongo schema: %v", err)
	}

	storage := &Data{client: client, database: database}
	paymentRepository := NewPaymentRepository(storage)
	repository, ok := paymentRepository.(payments.CheckoutRepository)
	if !ok || repository == nil {
		t.Fatal("NewPaymentRepository() 未实现 CheckoutRepository")
	}
	readRepository, ok := paymentRepository.(payments.ReadRepository)
	if !ok || readRepository == nil {
		t.Fatal("NewPaymentRepository() 未实现 ReadRepository")
	}

	userID := "payment-subscription-user-" + uuid.NewString()
	initialBalance := int64(11)
	if _, err := database.Collection(schema.CollectionUsers).InsertOne(ctx, model.UserDocument{
		ID:             userID,
		DisplayName:    "payment-chain-user",
		AccountStatus:  "normal",
		BindingState:   "bound",
		Role:           "user",
		Timezone:       "Asia/Shanghai",
		SessionVersion: 1,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed payment-chain user: %v", err)
	}
	if _, err := database.Collection(schema.CollectionAccounts).InsertOne(ctx, model.AccountDocument{
		ID: userID, DiamondBalance: initialBalance, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed payment-chain account: %v", err)
	}

	product := payments.PaymentProduct{
		ID:            "vip-30d-" + uuid.NewString(),
		Version:       1,
		DiamondAmount: 300,
		AmountCents:   1999,
		Currency:      "USD",
		Label:         "VIP 30 days",
		PublishStatus: payments.ProductPublishStatusPublished,
	}
	if err := repository.CreateProduct(ctx, product); err != nil {
		t.Fatalf("CreateProduct() error = %v", err)
	}

	// 商品展示必须来自已发布的 Go 商品快照，并保留价格、币种和权益数量。
	products, err := payments.NewReadUsecase(readRepository).ListProducts(ctx)
	if err != nil {
		t.Fatalf("ListProducts() error = %v", err)
	}
	var listed *payments.PaymentProduct
	for index := range products {
		if products[index].ID == product.ID {
			listed = &products[index]
			break
		}
	}
	if listed == nil || *listed != product {
		t.Fatalf("商品展示 = %#v，期望已发布商品快照 %#v", listed, product)
	}

	fakeProvider := &paymentSubscriptionPayCoresFake{
		result: payments.PayCoresCheckoutResult{ProviderOrderID: "fake-provider-order-" + uuid.NewString(), CheckoutURL: "http://127.0.0.1/fake-checkout"},
	}
	paymentAt := time.Now().UTC().Truncate(time.Millisecond)
	checkout := payments.NewCheckoutServiceWithPayCores(repository, fakeProvider, func() time.Time { return paymentAt })
	checkoutResult, err := checkout.CreatePayCoresCheckoutForChannel(ctx, userID, product.ID, payments.PaymentChannelSelection{
		Provider:             "fake_paycores",
		Account:              "test_googlepay",
		ClientDevicePlatform: "web",
		ClientRequestID:      "acceptance-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("CreatePayCoresCheckoutForChannel() error = %v", err)
	}
	if checkoutResult.Order == nil || checkoutResult.Order.Status != payments.PaymentOrderStatusPending || checkoutResult.Order.DiamondAmount != product.DiamondAmount || fakeProvider.calls != 1 {
		t.Fatalf("建单事实 = %#v，fake calls=%d，期望 pending + 冻结商品权益 + 1 次 provider 调用", checkoutResult.Order, fakeProvider.calls)
	}
	if fakeProvider.request.AmountCents != product.AmountCents || fakeProvider.request.Currency != product.Currency || fakeProvider.request.Credits != product.DiamondAmount {
		t.Fatalf("fake PayCores 请求 = %#v，期望仅使用服务端冻结商品事实", fakeProvider.request)
	}

	transactionID := "fake-transaction-" + uuid.NewString()
	nonceHash, err := payments.NewPaymentCallbackNonceHash("acceptance-nonce-" + uuid.NewString())
	if err != nil {
		t.Fatalf("创建 callback nonce 摘要: %v", err)
	}
	settler := payments.NewService(repository, func() time.Time { return paymentAt })
	callback := payments.NewConfirmedCallbackUsecase(
		NewPayCoresCallbackNonceRepository(storage),
		repository,
		settler,
		"/api/v1/internal/payment-confirmed",
		func() time.Time { return paymentAt },
	)
	confirmation, err := payments.NewVerifiedPaymentConfirmation(userID, checkoutResult.Order.ProviderOrderID, transactionID, nonceHash)
	if err != nil {
		t.Fatalf("构造已验签 payment confirmation: %v", err)
	}
	settled, err := callback.Handle(ctx, confirmation)
	if err != nil {
		t.Fatalf("首次支付回调结算 error = %v", err)
	}
	if !settled.Applied || settled.DiamondBalance != initialBalance+product.DiamondAmount {
		t.Fatalf("首次支付结算 = %#v，期望余额 %d", settled, initialBalance+product.DiamondAmount)
	}
	if _, err := callback.Handle(ctx, confirmation); !errors.Is(err, payments.ErrPaymentCallbackReplayed) {
		t.Fatalf("同 nonce 回调 error = %v，期望 ErrPaymentCallbackReplayed", err)
	}

	paidOrder, err := repository.FindOrder(ctx, checkoutResult.Order.ID)
	if err != nil || paidOrder == nil || paidOrder.Status != payments.PaymentOrderStatusPaid {
		t.Fatalf("回调后的订单 = %#v, error=%v，期望 paid", paidOrder, err)
	}
	if balance, err := repository.FindDiamondBalance(ctx, userID); err != nil || balance != initialBalance+product.DiamondAmount {
		t.Fatalf("回调后的余额 = %d, error=%v，期望 %d", balance, err, initialBalance+product.DiamondAmount)
	}
	if count, err := database.Collection(schema.CollectionLedgerEntries).CountDocuments(ctx, bson.D{{Key: "account_id", Value: userID}, {Key: "reason", Value: "payment_credit"}}); err != nil || count != 1 {
		t.Fatalf("支付入账账本数 = %d, error=%v，期望 1", count, err)
	}

	// 订阅发放是后台权益事实：与支付订单、钻石账本分离，但必须写入同事务审计。
	adminOperations := adminview.NewOperations(NewAdminViewRepository(storage))
	firstGrant, err := adminOperations.GrantSubscription(ctx, adminview.Actor{ID: "acceptance-admin", Role: "super_admin"}, adminview.SubscriptionGrant{
		UserID: userID, Tier: "vip", Days: 30, Reason: "payment acceptance grant",
	})
	if err != nil || firstGrant.Action != "created" || firstGrant.Tier != "vip" {
		t.Fatalf("首次订阅发放 = %#v, error=%v，期望 created/vip", firstGrant, err)
	}
	var firstSubscription model.SubscriptionDocument
	if err := database.Collection(schema.CollectionSubscriptions).FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&firstSubscription); err != nil {
		t.Fatalf("读取首次订阅快照: %v", err)
	}
	if firstSubscription.Status != "active" || !firstSubscription.ExpiresAt.After(firstSubscription.StartsAt) {
		t.Fatalf("首次订阅快照 = %#v，期望 active 且有有效期", firstSubscription)
	}

	secondGrant, err := adminOperations.GrantSubscription(ctx, adminview.Actor{ID: "acceptance-admin", Role: "super_admin"}, adminview.SubscriptionGrant{
		UserID: userID, Tier: "vip", Days: 7, Reason: "payment acceptance renewal",
	})
	if err != nil || secondGrant.Action != "extended" {
		t.Fatalf("有效期内续期 = %#v, error=%v，期望 extended", secondGrant, err)
	}
	var secondSubscription model.SubscriptionDocument
	if err := database.Collection(schema.CollectionSubscriptions).FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&secondSubscription); err != nil {
		t.Fatalf("读取续期订阅快照: %v", err)
	}
	wantExpiry := firstSubscription.ExpiresAt.AddDate(0, 0, 7)
	if delta := secondSubscription.ExpiresAt.Sub(wantExpiry); delta > time.Millisecond || delta < -time.Millisecond {
		t.Fatalf("续期后的 expires_at = %s，期望在 %s ±1ms 内", secondSubscription.ExpiresAt, wantExpiry)
	}
	if !secondSubscription.CreatedAt.Equal(firstSubscription.CreatedAt) || !secondSubscription.StartsAt.Equal(firstSubscription.StartsAt) {
		t.Fatalf("续期不应重置 created_at/starts_at: first=%#v second=%#v", firstSubscription, secondSubscription)
	}
	if count, err := database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.D{{Key: "target_id", Value: userID}, {Key: "action", Value: "wallet_grant_subscription"}}); err != nil || count != 2 {
		t.Fatalf("订阅审计数 = %d, error=%v，期望首次发放与续期各 1 条", count, err)
	}
}

type paymentSubscriptionPayCoresFake struct {
	request payments.PayCoresCheckoutRequest
	result  payments.PayCoresCheckoutResult
	calls   int
}

func (fake *paymentSubscriptionPayCoresFake) CreatePayCoresOrder(_ context.Context, request payments.PayCoresCheckoutRequest) (payments.PayCoresCheckoutResult, error) {
	fake.calls++
	fake.request = request
	return fake.result, nil
}

var _ payments.PayCoresCheckoutCreator = (*paymentSubscriptionPayCoresFake)(nil)
