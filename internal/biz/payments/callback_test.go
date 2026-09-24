package payments

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestConfirmedCallback成功时先消费Nonce再按准确关联结算(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	confirmation := fixture.confirmation(t)

	result, err := fixture.usecase.Handle(context.Background(), confirmation)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !result.Applied || result.DiamondBalance != 42 {
		t.Fatalf("Handle() result = %#v，期望结算结果", result)
	}
	if got, want := fixture.events, []string{"consume", "locate", "settle"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", got, want)
	}
	if fixture.nonces.hash != confirmation.NonceHash || fixture.nonces.method != "POST" || fixture.nonces.path != "/api/v1/internal/payment-confirmed" {
		t.Fatalf("nonce 消费参数 = %#v，期望受控摘要与固定边界", fixture.nonces)
	}
	if want := fixture.now.Add(2 * time.Minute); !fixture.nonces.expiresAt.Equal(want) {
		t.Fatalf("nonce 过期时间 = %s，期望 %s", fixture.nonces.expiresAt, want)
	}
	if fixture.locator.provider != ProviderPayCores || fixture.locator.orderID != confirmation.OrderID {
		t.Fatalf("订单定位关联 = provider=%q, orderID=%q，期望 %q/%q", fixture.locator.provider, fixture.locator.orderID, ProviderPayCores, confirmation.OrderID)
	}
	if got, want := fixture.settler.settlement, (VerifiedOrderSettlement{
		PaymentOrderID:        "payment-order-1",
		UserID:                "user-1",
		Provider:              ProviderPayCores,
		ProviderOrderID:       "provider-order-1",
		ExternalTransactionID: "provider-txn-1",
		BusinessAt:            fixture.now,
	}); got != want {
		t.Fatalf("结算关联 = %#v，期望 %#v", got, want)
	}
}

func TestConfirmedCallbackNonce已重放时不结算(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	fixture.nonces.err = ErrPaymentCallbackReplayed

	_, err := fixture.usecase.Handle(context.Background(), fixture.confirmation(t))
	if !errors.Is(err, ErrPaymentCallbackReplayed) {
		t.Fatalf("Handle() error = %v，期望 ErrPaymentCallbackReplayed", err)
	}
	if fixture.locator.calls != 0 || fixture.settler.calls != 0 {
		t.Fatalf("FindOrderByProviderOrder()/SettleVerifiedOrder() 调用次数 = %d/%d，期望 0/0", fixture.locator.calls, fixture.settler.calls)
	}
}

func TestConfirmedCallbackNonce存储失败时不结算(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	storeErr := errors.New("nonce storage unavailable")
	fixture.nonces.err = storeErr

	_, err := fixture.usecase.Handle(context.Background(), fixture.confirmation(t))
	if !errors.Is(err, storeErr) {
		t.Fatalf("Handle() error = %v，期望 nonce store 错误", err)
	}
	if fixture.locator.calls != 0 || fixture.settler.calls != 0 {
		t.Fatalf("FindOrderByProviderOrder()/SettleVerifiedOrder() 调用次数 = %d/%d，期望 0/0", fixture.locator.calls, fixture.settler.calls)
	}
}

func TestConfirmedCallback验签投影不完整时不消费Nonce或结算(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	valid := fixture.confirmation(t)
	cases := []struct {
		name   string
		mutate func(*VerifiedPaymentConfirmation)
	}{
		{name: "用户", mutate: func(confirmation *VerifiedPaymentConfirmation) { confirmation.UserID = " " }},
		{name: "订单", mutate: func(confirmation *VerifiedPaymentConfirmation) { confirmation.OrderID = "" }},
		{name: "渠道交易", mutate: func(confirmation *VerifiedPaymentConfirmation) { confirmation.ProviderTxnID = "" }},
		{name: "nonce 摘要", mutate: func(confirmation *VerifiedPaymentConfirmation) { confirmation.NonceHash = PaymentCallbackNonceHash{} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			confirmation := valid
			testCase.mutate(&confirmation)
			_, err := fixture.usecase.Handle(context.Background(), confirmation)
			if !errors.Is(err, ErrInvalidVerifiedPaymentConfirmation) {
				t.Fatalf("Handle() error = %v，期望 ErrInvalidVerifiedPaymentConfirmation", err)
			}
			if fixture.nonces.calls != 0 || fixture.locator.calls != 0 || fixture.settler.calls != 0 {
				t.Fatalf("nonce/locate/settle 调用次数 = %d/%d/%d，期望 0/0/0", fixture.nonces.calls, fixture.locator.calls, fixture.settler.calls)
			}
		})
	}
}

func TestConfirmedCallback结算失败必须传播(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	settlementErr := errors.New("settlement failed")
	fixture.settler.err = settlementErr

	_, err := fixture.usecase.Handle(context.Background(), fixture.confirmation(t))
	if !errors.Is(err, settlementErr) {
		t.Fatalf("Handle() error = %v，期望结算错误", err)
	}
	if got, want := fixture.events, []string{"consume", "locate", "settle"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", got, want)
	}
}

func TestConfirmedCallback订单定位失败时传播且不结算(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	locateErr := errors.New("payment order lookup failed")
	fixture.locator.err = locateErr

	_, err := fixture.usecase.Handle(context.Background(), fixture.confirmation(t))
	if !errors.Is(err, locateErr) {
		t.Fatalf("Handle() error = %v，期望订单定位错误", err)
	}
	if got, want := fixture.events, []string{"consume", "locate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", got, want)
	}
	if fixture.settler.calls != 0 {
		t.Fatalf("SettleVerifiedOrder() 调用次数 = %d，期望 0", fixture.settler.calls)
	}
}

func TestConfirmedCallback渠道订单绑定稍晚时在同次投递内恢复(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	fixture.locator.orders = []*PaymentOrder{nil, {ID: "payment-order-1"}}

	result, err := fixture.usecase.Handle(context.Background(), fixture.confirmation(t))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if !result.Applied || result.DiamondBalance != 42 {
		t.Fatalf("Handle() result = %#v，期望结算结果", result)
	}
	if got, want := fixture.events, []string{"consume", "locate", "locate", "settle"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", got, want)
	}
	if fixture.nonces.calls != 1 {
		t.Fatalf("Consume() 调用次数 = %d，期望 1", fixture.nonces.calls)
	}
}

func TestConfirmedCallback渠道订单绑定宽限后仍不存在时返回未找到(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	fixture.locator.order = nil

	_, err := fixture.usecase.Handle(context.Background(), fixture.confirmation(t))
	if !errors.Is(err, ErrPaymentOrderNotFound) {
		t.Fatalf("Handle() error = %v，期望 ErrPaymentOrderNotFound", err)
	}
	if got, want := fixture.events, []string{"consume", "locate", "locate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", got, want)
	}
	if fixture.locator.calls != 2 || fixture.settler.calls != 0 {
		t.Fatalf("FindOrderByProviderOrder()/SettleVerifiedOrder() 调用次数 = %d/%d，期望 2/0", fixture.locator.calls, fixture.settler.calls)
	}
}

func TestConfirmedCallback等待渠道订单绑定时Context取消(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	fixture.locator.order = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture.locator.afterCall = func(call int) {
		if call == 1 {
			cancel()
		}
	}

	_, err := fixture.usecase.Handle(ctx, fixture.confirmation(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle() error = %v，期望 context.Canceled", err)
	}
	if got, want := fixture.events, []string{"consume", "locate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", got, want)
	}
	if fixture.locator.calls != 1 || fixture.settler.calls != 0 {
		t.Fatalf("FindOrderByProviderOrder()/SettleVerifiedOrder() 调用次数 = %d/%d，期望 1/0", fixture.locator.calls, fixture.settler.calls)
	}
}

func TestConfirmedCallback订单关联不一致由既有结算语义拒绝(t *testing.T) {
	fixture := newConfirmedCallbackFixture(t)
	fixture.settler.err = ErrPaymentOrderMismatch

	_, err := fixture.usecase.Handle(context.Background(), fixture.confirmation(t))
	if !errors.Is(err, ErrPaymentOrderMismatch) {
		t.Fatalf("Handle() error = %v，期望 ErrPaymentOrderMismatch", err)
	}
	if got, want := fixture.events, []string{"consume", "locate", "settle"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("调用顺序 = %#v，期望 %#v", got, want)
	}
}

func TestConfirmedCallbackNonce消费后定位或结算瞬时失败仍拒绝重放(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		prepare func(*confirmedCallbackFixture, error)
		want    []string
	}{
		{
			name: "订单定位失败",
			prepare: func(fixture *confirmedCallbackFixture, transient error) {
				fixture.locator.err = transient
			},
			want: []string{"consume", "locate", "consume"},
		},
		{
			name: "订单结算失败",
			prepare: func(fixture *confirmedCallbackFixture, transient error) {
				fixture.settler.err = transient
			},
			want: []string{"consume", "locate", "settle", "consume"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newConfirmedCallbackFixture(t)
			fixture.nonces.replayAfterFirst = true
			transient := errors.New("transient downstream failure")
			testCase.prepare(fixture, transient)
			confirmation := fixture.confirmation(t)

			if _, err := fixture.usecase.Handle(context.Background(), confirmation); !errors.Is(err, transient) {
				t.Fatalf("首次 Handle() error = %v，期望瞬时下游错误", err)
			}
			if _, err := fixture.usecase.Handle(context.Background(), confirmation); !errors.Is(err, ErrPaymentCallbackReplayed) {
				t.Fatalf("重放 Handle() error = %v，期望 ErrPaymentCallbackReplayed", err)
			}
			if got := fixture.events; !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("调用顺序 = %#v，期望 %#v", got, testCase.want)
			}
			if fixture.locator.calls != 1 {
				t.Fatalf("FindOrderByProviderOrder() 调用次数 = %d，期望 1", fixture.locator.calls)
			}
			if testCase.name == "订单定位失败" && fixture.settler.calls != 0 {
				t.Fatalf("定位失败时 SettleVerifiedOrder() 调用次数 = %d，期望 0", fixture.settler.calls)
			}
			if testCase.name == "订单结算失败" && fixture.settler.calls != 1 {
				t.Fatalf("结算失败时 SettleVerifiedOrder() 调用次数 = %d，期望 1", fixture.settler.calls)
			}
		})
	}
}

func TestConfirmedCallback只接收三项已验证关联和受控Nonce摘要(t *testing.T) {
	typeOfConfirmation := reflect.TypeFor[VerifiedPaymentConfirmation]()
	wantFields := []string{"UserID", "OrderID", "ProviderTxnID", "NonceHash"}
	if typeOfConfirmation.NumField() != len(wantFields) {
		t.Fatalf("VerifiedPaymentConfirmation 字段数 = %d，期望 %d", typeOfConfirmation.NumField(), len(wantFields))
	}
	for index, want := range wantFields {
		field := typeOfConfirmation.Field(index)
		if field.Name != want {
			t.Fatalf("字段[%d] = %q，期望 %q", index, field.Name, want)
		}
	}
	nonceHashField, ok := typeOfConfirmation.FieldByName("NonceHash")
	if !ok {
		t.Fatal("VerifiedPaymentConfirmation 缺少 NonceHash")
	}
	if got, want := nonceHashField.Type, reflect.TypeFor[PaymentCallbackNonceHash](); got != want {
		t.Fatalf("NonceHash 类型 = %v，期望 %v", got, want)
	}
}

type confirmedCallbackFixture struct {
	usecase *ConfirmedCallbackUsecase
	nonces  *memoryPaymentCallbackNonceStore
	locator *memoryPaymentOrderLocator
	settler *memoryVerifiedOrderSettler
	events  []string
	now     time.Time
}

func newConfirmedCallbackFixture(t *testing.T) *confirmedCallbackFixture {
	t.Helper()
	fixture := &confirmedCallbackFixture{now: time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)}
	fixture.nonces = &memoryPaymentCallbackNonceStore{events: &fixture.events}
	fixture.locator = &memoryPaymentOrderLocator{events: &fixture.events, order: &PaymentOrder{ID: "payment-order-1"}}
	fixture.settler = &memoryVerifiedOrderSettler{events: &fixture.events, result: ApplyResult{Applied: true, DiamondBalance: 42}}
	fixture.usecase = NewConfirmedCallbackUsecase(fixture.nonces, fixture.locator, fixture.settler, "/api/v1/internal/payment-confirmed", func() time.Time { return fixture.now })
	return fixture
}

func (fixture *confirmedCallbackFixture) confirmation(t *testing.T) VerifiedPaymentConfirmation {
	t.Helper()
	nonceHash, err := NewPaymentCallbackNonceHash("12345678-1234-4123-8123-123456789abc")
	if err != nil {
		t.Fatalf("NewPaymentCallbackNonceHash() error = %v", err)
	}
	confirmation, err := NewVerifiedPaymentConfirmation("user-1", "provider-order-1", "provider-txn-1", nonceHash)
	if err != nil {
		t.Fatalf("NewVerifiedPaymentConfirmation() error = %v", err)
	}
	return confirmation
}

type memoryPaymentOrderLocator struct {
	events    *[]string
	err       error
	calls     int
	provider  Provider
	orderID   string
	order     *PaymentOrder
	orders    []*PaymentOrder
	afterCall func(int)
}

func (locator *memoryPaymentOrderLocator) FindOrderByProviderOrder(_ context.Context, provider Provider, providerOrderID string) (*PaymentOrder, error) {
	locator.calls++
	locator.provider, locator.orderID = provider, providerOrderID
	*locator.events = append(*locator.events, "locate")
	if locator.afterCall != nil {
		locator.afterCall(locator.calls)
	}
	if len(locator.orders) > 0 {
		order := locator.orders[0]
		locator.orders = locator.orders[1:]
		return order, locator.err
	}
	return locator.order, locator.err
}

type memoryPaymentCallbackNonceStore struct {
	events           *[]string
	err              error
	replayAfterFirst bool
	calls            int
	hash             PaymentCallbackNonceHash
	method           string
	path             string
	expiresAt        time.Time
}

func (store *memoryPaymentCallbackNonceStore) Consume(_ context.Context, nonceHash PaymentCallbackNonceHash, method, path string, expiresAt time.Time) error {
	store.calls++
	store.hash, store.method, store.path, store.expiresAt = nonceHash, method, path, expiresAt
	*store.events = append(*store.events, "consume")
	if store.replayAfterFirst && store.calls > 1 {
		return ErrPaymentCallbackReplayed
	}
	return store.err
}

type memoryVerifiedOrderSettler struct {
	events     *[]string
	err        error
	calls      int
	settlement VerifiedOrderSettlement
	result     ApplyResult
}

func (settler *memoryVerifiedOrderSettler) SettleVerifiedOrder(_ context.Context, settlement VerifiedOrderSettlement) (ApplyResult, error) {
	settler.calls++
	settler.settlement = settlement
	*settler.events = append(*settler.events, "settle")
	return settler.result, settler.err
}
