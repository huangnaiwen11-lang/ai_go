package main

import (
	"testing"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/model"
)

func TestLocalPaymentProduct是可购买的固定本地快照(t *testing.T) {
	product := localPaymentProduct()

	want := payments.PaymentProduct{
		ID:            "local_coins_100",
		Version:       1,
		DiamondAmount: 100,
		AmountCents:   999,
		Currency:      "USD",
		Label:         "100 钻石（本地测试）",
		PublishStatus: payments.ProductPublishStatusPublished,
	}
	if product != want {
		t.Fatalf("本地支付商品 = %#v，期望 %#v", product, want)
	}
	if err := product.Validate(); err != nil {
		t.Fatalf("本地支付商品必须是可冻结的有效商品: %v", err)
	}
}

func TestPaymentProductDocumentMatchesFixture只接受完全一致的已存在快照(t *testing.T) {
	product := localPaymentProduct()
	exact := model.PaymentProductDocument{
		ProductID:     product.ID,
		Version:       product.Version,
		DiamondAmount: product.DiamondAmount,
		AmountCents:   product.AmountCents,
		Currency:      product.Currency,
		Label:         product.Label,
		PublishStatus: string(product.PublishStatus),
	}
	if !paymentProductDocumentMatchesFixture(exact, product) {
		t.Fatal("完全一致的已存在快照应被安全复用")
	}

	exact.AmountCents++
	if paymentProductDocumentMatchesFixture(exact, product) {
		t.Fatal("不同价格的同名商品不能被本地 fixture 覆盖")
	}
}

func TestLocalPaymentMongoConfig拒绝非本地连接(t *testing.T) {
	if _, err := localPaymentMongoConfig("mongodb://example.com:27017/?replicaSet=rs0"); err == nil {
		t.Fatal("远程 MongoDB 连接不应被本地支付 fixture 接受")
	}
}
