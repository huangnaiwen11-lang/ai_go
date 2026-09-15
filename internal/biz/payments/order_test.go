package payments

import (
	"errors"
	"testing"
	"time"
)

// 订单必须冻结已发布商品的钻石数，之后商品改价不能影响该订单。
func TestFreezeOrder冻结已发布商品的钻石数(t *testing.T) {
	createdAt := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	order, err := FreezeOrder(CreateOrderInput{
		OrderID:         "order-1",
		UserID:          "user-1",
		Provider:        ProviderPayCores,
		ProviderOrderID: "paycores-order-1",
	}, PaymentProduct{
		ID:            "coins_100",
		Version:       3,
		DiamondAmount: 100,
		AmountCents:   999,
		Currency:      "USD",
		PublishStatus: ProductPublishStatusPublished,
	}, createdAt)
	if err != nil {
		t.Fatalf("FreezeOrder() error = %v", err)
	}
	if order.ProductID != "coins_100" || order.ProductVersion != 3 || order.DiamondAmount != 100 || order.AmountCents != 999 || order.Currency != "USD" || order.Status != PaymentOrderStatusPending || !order.CreatedAt.Equal(createdAt) {
		t.Fatalf("冻结订单 = %#v，期望保留已发布商品的版本、钻石数、价格、币种和创建时间", order)
	}
}

// 下架或草稿商品不能创建本地支付订单，防止未发布价格进入结算事实。
func TestFreezeOrder拒绝未发布商品(t *testing.T) {
	_, err := FreezeOrder(CreateOrderInput{
		OrderID:         "order-2",
		UserID:          "user-1",
		Provider:        ProviderPayCores,
		ProviderOrderID: "paycores-order-2",
	}, PaymentProduct{
		ID:            "coins_100",
		Version:       1,
		DiamondAmount: 100,
		PublishStatus: ProductPublishStatusDraft,
	}, time.Now())
	if !errors.Is(err, ErrProductNotPublished) {
		t.Fatalf("FreezeOrder() error = %v，期望 ErrProductNotPublished", err)
	}
}
