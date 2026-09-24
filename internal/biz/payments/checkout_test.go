package payments

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 商店回执只允许按服务端已发布商品的冻结值入账；客户端请求中没有也不能传入钻石数量。
func TestSettleVerifiedStorePurchase按最新发布商品冻结并入账(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	repository.products["coins_100"] = []PaymentProduct{
		{ID: "coins_100", Version: 1, DiamondAmount: 100, PublishStatus: ProductPublishStatusPublished},
		{ID: "coins_100", Version: 2, DiamondAmount: 120, PublishStatus: ProductPublishStatusPublished},
	}
	service := NewCheckoutService(repository, func() time.Time {
		return time.Date(2026, time.September, 10, 10, 0, 0, 0, time.UTC)
	})

	result, err := service.SettleVerifiedStorePurchase(context.Background(), VerifiedStorePurchase{
		Provider:              StoreProviderApple,
		ExternalTransactionID: "apple-transaction-1",
		StoreProductID:        "com.example.coins100",
		ProductID:             "coins_100",
		UserID:                "user-1",
		BusinessAt:            time.Date(2026, time.September, 10, 9, 59, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("SettleVerifiedStorePurchase() error = %v", err)
	}
	if !result.Applied || result.DiamondBalance != 120 {
		t.Fatalf("结算结果 = %#v，期望按服务端商品 v2 入账 120 钻", result)
	}

	order := repository.orderByProviderOrder(ProviderApple, "apple-transaction-1")
	if order == nil || order.ProductVersion != 2 || order.DiamondAmount != 120 || order.Status != PaymentOrderStatusPaid {
		t.Fatalf("冻结订单 = %#v，期望保存商品 v2、120 钻且已完成", order)
	}
}

// Apple 与 Google 的交易号命名空间独立；不能因为字符串碰巧相同就吞掉另一个商店的购买。
func TestSettleVerifiedStorePurchase苹果与Google同交易号独立入账(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	repository.products["coins_100"] = []PaymentProduct{{
		ID: "coins_100", Version: 1, DiamondAmount: 100, PublishStatus: ProductPublishStatusPublished,
	}}
	service := NewCheckoutService(repository, func() time.Time { return fixedPaymentTime() })

	for _, provider := range []StoreProvider{StoreProviderApple, StoreProviderGoogle} {
		result, err := service.SettleVerifiedStorePurchase(context.Background(), VerifiedStorePurchase{
			Provider:              provider,
			ExternalTransactionID: "same-looking-transaction",
			StoreProductID:        "coins100",
			ProductID:             "coins_100",
			UserID:                "user-1",
			BusinessAt:            fixedPaymentTime(),
		})
		if err != nil {
			t.Fatalf("%s 结算 error = %v", provider, err)
		}
		if !result.Applied {
			t.Fatalf("%s 首次结算应入账，结果 = %#v", provider, result)
		}
	}
	if balance := repository.balance("user-1"); balance != 200 {
		t.Fatalf("余额 = %d，期望 Apple 与 Google 各入账一次后的 200", balance)
	}
}

// 同一商店交易不可跨用户占用；后到请求必须失败且不能增加任何人的余额。
func TestSettleVerifiedStorePurchase拒绝跨用户重放(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	repository.products["coins_100"] = []PaymentProduct{{
		ID: "coins_100", Version: 1, DiamondAmount: 100, PublishStatus: ProductPublishStatusPublished,
	}}
	service := NewCheckoutService(repository, fixedPaymentTime)
	first := VerifiedStorePurchase{
		Provider:              StoreProviderGoogle,
		ExternalTransactionID: "google-transaction-owner",
		StoreProductID:        "coins100",
		ProductID:             "coins_100",
		UserID:                "owner-user",
		BusinessAt:            fixedPaymentTime(),
	}
	if _, err := service.SettleVerifiedStorePurchase(context.Background(), first); err != nil {
		t.Fatalf("首次结算 error = %v", err)
	}

	conflict := first
	conflict.UserID = "other-user"
	if _, err := service.SettleVerifiedStorePurchase(context.Background(), conflict); !errors.Is(err, ErrPaymentOrderMismatch) {
		t.Fatalf("跨用户重放 error = %v，期望 ErrPaymentOrderMismatch", err)
	}
	if balance := repository.balance("owner-user"); balance != 100 {
		t.Fatalf("原用户余额 = %d，期望仍为 100", balance)
	}
	if balance := repository.balance("other-user"); balance != 0 {
		t.Fatalf("其他用户余额 = %d，期望 0", balance)
	}
}

// PayCores 只可收到 Go 本地订单冻结的价格、币种和钻石数；回调关联使用其返回订单号。
func TestCreatePayCoresCheckout冻结商品并绑定渠道订单号(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	repository.products["coins_100"] = []PaymentProduct{{
		ID: "coins_100", Version: 1, DiamondAmount: 100, AmountCents: 999, Currency: "USD", Label: "100 Diamonds", PublishStatus: ProductPublishStatusPublished,
	}}
	creator := &recordingPayCoresCreator{result: PayCoresCheckoutResult{ProviderOrderID: "pco_1", CheckoutURL: "http://127.0.0.1:19082/checkout/pco_1"}}
	service := NewCheckoutServiceWithPayCores(repository, creator, fixedPaymentTime)
	service.newOrderID = func() (string, error) { return "local-payment-1", nil }

	checkout, err := service.CreatePayCoresCheckout(context.Background(), "user-1", "coins_100")
	if err != nil {
		t.Fatalf("CreatePayCoresCheckout() error = %v", err)
	}
	if creator.request.AmountCents != 999 || creator.request.Currency != "USD" || creator.request.Credits != 100 || creator.request.ClientRequestID != "local-payment-1" {
		t.Fatalf("发送给 PayCores 的冻结事实 = %#v", creator.request)
	}
	if checkout.Order.ProviderOrderID != "pco_1" || checkout.CheckoutURL != "http://127.0.0.1:19082/checkout/pco_1" {
		t.Fatalf("收银台结果 = %#v，期望绑定渠道订单并返回跳转地址", checkout)
	}
}

func TestCreatePayCoresCheckoutForChannel保留WebGooglePay渠道(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	repository.products["coins_100"] = []PaymentProduct{{ID: "coins_100", Version: 1, Label: "100 Diamonds", DiamondAmount: 100, AmountCents: 999, Currency: "USD", PublishStatus: ProductPublishStatusPublished}}
	creator := &recordingPayCoresCreator{result: PayCoresCheckoutResult{ProviderOrderID: "pco-google", CheckoutURL: "https://checkout.example.test/pco-google"}}
	service := NewCheckoutServiceWithPayCores(repository, creator, fixedPaymentTime)
	service.newOrderID = func() (string, error) { return "local-google-web", nil }
	if _, err := service.CreatePayCoresCheckoutForChannel(context.Background(), "user-1", "coins_100", PaymentChannelSelection{Provider: "shinningpay", Account: "us_googlepay", ClientDevicePlatform: "web"}); err != nil {
		t.Fatal(err)
	}
	if creator.request.Provider != "shinningpay" || creator.request.Account != "us_googlepay" || creator.request.ClientDevicePlatform != "web" {
		t.Fatalf("channel request = %#v", creator.request)
	}
}

func TestCreatePayCoresCheckoutForChannel复用ClientRequestID订单(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	repository.products["coins_100"] = []PaymentProduct{{ID: "coins_100", Version: 1, Label: "100 Diamonds", DiamondAmount: 100, AmountCents: 999, Currency: "USD", PublishStatus: ProductPublishStatusPublished}}
	creator := &recordingPayCoresCreator{result: PayCoresCheckoutResult{ProviderOrderID: "pco-idempotent", CheckoutURL: "https://checkout.example.test/pco-idempotent"}}
	service := NewCheckoutServiceWithPayCores(repository, creator, fixedPaymentTime)
	requestID := "web-checkout-retry-1"
	first, err := service.CreatePayCoresCheckoutForChannel(context.Background(), "user-1", "coins_100", PaymentChannelSelection{Provider: "shinningpay", Account: "us_googlepay", ClientDevicePlatform: "web", ClientRequestID: requestID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreatePayCoresCheckoutForChannel(context.Background(), "user-1", "coins_100", PaymentChannelSelection{Provider: "shinningpay", Account: "us_googlepay", ClientDevicePlatform: "web", ClientRequestID: requestID})
	if err != nil {
		t.Fatal(err)
	}
	if first.Order.ID != second.Order.ID || second.Order.ProviderOrderID != "pco-idempotent" || creator.request.ClientRequestID != requestID {
		t.Fatalf("重试未复用订单/幂等键：first=%#v second=%#v request=%#v", first, second, creator.request)
	}
}

func TestCreatePayCoresCheckoutForChannel拒绝同一ClientRequestID切换渠道(t *testing.T) {
	for _, test := range []struct {
		name      string
		selection PaymentChannelSelection
	}{
		{name: "provider", selection: PaymentChannelSelection{Provider: "other-provider", Account: "us_googlepay", ClientDevicePlatform: "web"}},
		{name: "account", selection: PaymentChannelSelection{Provider: "shinningpay", Account: "other-account", ClientDevicePlatform: "web"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newCheckoutMemoryRepository()
			repository.products["coins_100"] = []PaymentProduct{{ID: "coins_100", Version: 1, Label: "100 Diamonds", DiamondAmount: 100, AmountCents: 999, Currency: "USD", PublishStatus: ProductPublishStatusPublished}}
			creator := &recordingPayCoresCreator{result: PayCoresCheckoutResult{ProviderOrderID: "pco-idempotent", CheckoutURL: "https://checkout.example.test/pco-idempotent"}}
			service := NewCheckoutServiceWithPayCores(repository, creator, fixedPaymentTime)
			requestID := "web-checkout-channel-immutable"
			if _, err := service.CreatePayCoresCheckoutForChannel(context.Background(), "user-1", "coins_100", PaymentChannelSelection{Provider: "shinningpay", Account: "us_googlepay", ClientDevicePlatform: "web", ClientRequestID: requestID}); err != nil {
				t.Fatal(err)
			}

			test.selection.ClientRequestID = requestID
			_, err := service.CreatePayCoresCheckoutForChannel(context.Background(), "user-1", "coins_100", test.selection)
			if creator.calls != 1 {
				t.Errorf("PayCores 建单次数 = %d，期望渠道冲突在第二次外部调用前被拒绝", creator.calls)
			}
			if !errors.Is(err, ErrPaymentOrderMismatch) {
				t.Errorf("切换渠道 error = %v，期望 ErrPaymentOrderMismatch", err)
			}
		})
	}
}

func TestCreatePayCoresCheckout重复本地订单的绑定CAS被同值请求抢先完成时收敛成功(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	originalProduct := PaymentProduct{ID: "coins_100", Version: 1, Label: "100 Diamonds", DiamondAmount: 100, AmountCents: 999, Currency: "USD", PublishStatus: ProductPublishStatusPublished}
	repository.products["coins_100"] = []PaymentProduct{
		originalProduct,
		{ID: "coins_100", Version: 2, Label: "200 Diamonds", DiamondAmount: 200, AmountCents: 1999, Currency: "USD", PublishStatus: ProductPublishStatusPublished},
	}
	requestID := "web-checkout-cas-race"
	orderID := clientPaymentOrderID("user-1", "coins_100", requestID)
	originalOrder, err := FreezeOrder(CreateOrderInput{OrderID: orderID, UserID: "user-1", Provider: ProviderPayCores, ProviderOrderID: "local-paycores-" + orderID}, originalProduct, fixedPaymentTime())
	if err != nil {
		t.Fatal(err)
	}
	repository.orders[orderID] = *originalOrder
	repository.bindRaceProviderOrderID = "pco-idempotent"
	creator := &recordingPayCoresCreator{result: PayCoresCheckoutResult{ProviderOrderID: "pco-idempotent", CheckoutURL: "https://checkout.example.test/pco-idempotent"}}
	service := NewCheckoutServiceWithPayCores(repository, creator, fixedPaymentTime)

	checkout, err := service.CreatePayCoresCheckoutForChannel(context.Background(), "user-1", "coins_100", PaymentChannelSelection{ClientRequestID: requestID})
	if err != nil {
		t.Fatalf("CreatePayCoresCheckout() error = %v", err)
	}
	if checkout.Order.ProviderOrderID != "pco-idempotent" || checkout.Order.ProductVersion != 1 || checkout.Order.AmountCents != 999 || checkout.Order.DiamondAmount != 100 || creator.request.AmountCents != 999 || creator.request.Credits != 100 {
		t.Fatalf("CAS 收敛结果 = %#v，请求 = %#v，期望复用 v1 冻结订单且不改金额与钻石", checkout.Order, creator.request)
	}
}

func TestCreatePayCoresCheckout绑定CAS失败且渠道订单号不同时拒绝(t *testing.T) {
	repository := newCheckoutMemoryRepository()
	repository.products["coins_100"] = []PaymentProduct{{ID: "coins_100", Version: 1, Label: "100 Diamonds", DiamondAmount: 100, AmountCents: 999, Currency: "USD", PublishStatus: ProductPublishStatusPublished}}
	repository.bindRaceProviderOrderID = "pco-other"
	creator := &recordingPayCoresCreator{result: PayCoresCheckoutResult{ProviderOrderID: "pco-response", CheckoutURL: "https://checkout.example.test/pco-response"}}
	service := NewCheckoutServiceWithPayCores(repository, creator, fixedPaymentTime)
	requestID := "web-checkout-cas-conflict"

	if _, err := service.CreatePayCoresCheckoutForChannel(context.Background(), "user-1", "coins_100", PaymentChannelSelection{ClientRequestID: requestID}); !errors.Is(err, ErrPaymentOrderMismatch) {
		t.Fatalf("CreatePayCoresCheckout() error = %v，期望 ErrPaymentOrderMismatch", err)
	}
	stored, err := repository.FindOrder(context.Background(), clientPaymentOrderID("user-1", "coins_100", requestID))
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.ProviderOrderID != "pco-other" || stored.AmountCents != 999 || stored.DiamondAmount != 100 {
		t.Fatalf("冲突后的本地订单 = %#v，期望拒绝响应且不改冻结事实", stored)
	}
}

type recordingPayCoresCreator struct {
	request PayCoresCheckoutRequest
	result  PayCoresCheckoutResult
	calls   int
}

func (creator *recordingPayCoresCreator) CreatePayCoresOrder(_ context.Context, request PayCoresCheckoutRequest) (PayCoresCheckoutResult, error) {
	creator.calls++
	creator.request = request
	return creator.result, nil
}

// checkoutMemoryRepository 是领域测试的最小内存事务替身，只模拟本用例依赖的支付事实。
type checkoutMemoryRepository struct {
	mu                      sync.Mutex
	products                map[string][]PaymentProduct
	orders                  map[string]PaymentOrder
	receipts                map[receiptKey]Receipt
	balances                map[string]int64
	credits                 map[receiptKey]PaymentCredit
	bindRaceProviderOrderID string
}

func newCheckoutMemoryRepository() *checkoutMemoryRepository {
	return &checkoutMemoryRepository{
		products: make(map[string][]PaymentProduct),
		orders:   make(map[string]PaymentOrder),
		receipts: make(map[receiptKey]Receipt),
		balances: make(map[string]int64),
		credits:  make(map[receiptKey]PaymentCredit),
	}
}

func (repository *checkoutMemoryRepository) WithinTx(_ context.Context, operation func(context.Context) error) error {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return operation(context.Background())
}

func (repository *checkoutMemoryRepository) FindLatestPublishedProduct(_ context.Context, productID string) (*PaymentProduct, error) {
	for index := len(repository.products[productID]) - 1; index >= 0; index-- {
		product := repository.products[productID][index]
		if product.PublishStatus == ProductPublishStatusPublished {
			return &product, nil
		}
	}
	return nil, nil
}

func (repository *checkoutMemoryRepository) CreateProduct(_ context.Context, product PaymentProduct) error {
	repository.products[product.ID] = append(repository.products[product.ID], product)
	return nil
}

func (repository *checkoutMemoryRepository) FindProduct(_ context.Context, productID string, version int64) (*PaymentProduct, error) {
	for _, product := range repository.products[productID] {
		if product.Version == version {
			return &product, nil
		}
	}
	return nil, nil
}

func (repository *checkoutMemoryRepository) CreateOrder(_ context.Context, order PaymentOrder) error {
	for _, existing := range repository.orders {
		if existing.Provider == order.Provider && existing.ProviderOrderID == order.ProviderOrderID {
			return ErrPaymentOrderAlreadyExists
		}
	}
	repository.orders[order.ID] = order
	return nil
}

func (repository *checkoutMemoryRepository) FindOrder(_ context.Context, orderID string) (*PaymentOrder, error) {
	order, ok := repository.orders[orderID]
	if !ok {
		return nil, nil
	}
	return &order, nil
}

func (repository *checkoutMemoryRepository) FindOrderByProviderOrder(_ context.Context, provider Provider, providerOrderID string) (*PaymentOrder, error) {
	return repository.orderByProviderOrder(provider, providerOrderID), nil
}

func (repository *checkoutMemoryRepository) BindPayCoresProviderOrder(_ context.Context, localOrderID, provisionalProviderOrderID, providerOrderID string, updatedAt time.Time) (bool, error) {
	order, ok := repository.orders[localOrderID]
	if !ok || order.Provider != ProviderPayCores || order.ProviderOrderID != provisionalProviderOrderID || order.Status != PaymentOrderStatusPending {
		return false, nil
	}
	if repository.bindRaceProviderOrderID != "" {
		order.ProviderOrderID = repository.bindRaceProviderOrderID
		order.UpdatedAt = updatedAt.UTC()
		repository.orders[localOrderID] = order
		return false, nil
	}
	for _, existing := range repository.orders {
		if existing.Provider == ProviderPayCores && existing.ProviderOrderID == providerOrderID {
			return false, ErrPaymentOrderAlreadyExists
		}
	}
	order.ProviderOrderID = providerOrderID
	order.UpdatedAt = updatedAt.UTC()
	repository.orders[localOrderID] = order
	return true, nil
}

func (repository *checkoutMemoryRepository) MarkOrderPaid(_ context.Context, orderID string, updatedAt time.Time) (bool, error) {
	order, ok := repository.orders[orderID]
	if !ok || order.Status != PaymentOrderStatusPending {
		return false, nil
	}
	order.Status = PaymentOrderStatusPaid
	order.UpdatedAt = updatedAt.UTC()
	repository.orders[orderID] = order
	return true, nil
}

func (repository *checkoutMemoryRepository) FindReceipt(_ context.Context, provider Provider, externalTransactionID string) (*Receipt, error) {
	receipt, ok := repository.receipts[receiptKey{provider: provider, externalTransactionID: externalTransactionID}]
	if !ok {
		return nil, nil
	}
	return &receipt, nil
}

func (repository *checkoutMemoryRepository) FindDiamondBalance(_ context.Context, userID string) (int64, error) {
	return repository.balances[userID], nil
}

func (repository *checkoutMemoryRepository) CreateReceipt(_ context.Context, receipt Receipt) error {
	key := receiptKey{provider: receipt.Provider, externalTransactionID: receipt.ExternalTransactionID}
	if _, exists := repository.receipts[key]; exists {
		return ErrReceiptAlreadyExists
	}
	repository.receipts[key] = receipt
	return nil
}

func (repository *checkoutMemoryRepository) CreditDiamonds(_ context.Context, userID string, diamonds int64, _ time.Time) (int64, error) {
	repository.balances[userID] += diamonds
	return repository.balances[userID], nil
}

func (repository *checkoutMemoryRepository) AppendPaymentCredit(_ context.Context, credit PaymentCredit) error {
	key := receiptKey{provider: credit.Receipt.Provider, externalTransactionID: credit.Receipt.ExternalTransactionID}
	repository.credits[key] = credit
	return nil
}

func (repository *checkoutMemoryRepository) orderByProviderOrder(provider Provider, providerOrderID string) *PaymentOrder {
	for _, order := range repository.orders {
		if order.Provider == provider && order.ProviderOrderID == providerOrderID {
			copy := order
			return &copy
		}
	}
	return nil
}

func (repository *checkoutMemoryRepository) balance(userID string) int64 {
	return repository.balances[userID]
}

var _ PaymentRepository = (*checkoutMemoryRepository)(nil)
