package payments

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	// ErrInvalidPaymentProduct 表示商品版本无法作为本地订单的可信冻结来源。
	ErrInvalidPaymentProduct = errors.New("payments: invalid payment product")
	// ErrInvalidPaymentOrder 表示订单冻结所需的本地业务事实不完整。
	ErrInvalidPaymentOrder = errors.New("payments: invalid payment order")
	// ErrProductNotPublished 表示商品尚未发布，不能进入可结算订单。
	ErrProductNotPublished = errors.New("payments: payment product is not published")
	// ErrPaymentProductAlreadyExists 表示同一商品版本已被保存，不能覆盖既有商品事实。
	ErrPaymentProductAlreadyExists = errors.New("payments: payment product already exists")
	// ErrPaymentOrderAlreadyExists 表示同一支付渠道订单已被冻结，不能重复创建。
	ErrPaymentOrderAlreadyExists = errors.New("payments: payment order already exists")
)

// ProductPublishStatus 表示本地支付商品是否允许创建新订单。
type ProductPublishStatus string

const (
	// ProductPublishStatusDraft 表示商品仅供配置，不可被用户购买。
	ProductPublishStatusDraft ProductPublishStatus = "draft"
	// ProductPublishStatusPublished 表示商品可用于创建新订单。
	ProductPublishStatusPublished ProductPublishStatus = "published"
)

// PaymentOrderStatus 表示一次性本地支付订单的结算状态。
type PaymentOrderStatus string

const (
	// PaymentOrderStatusPending 表示订单已经冻结商品事实，等待可信支付确认。
	PaymentOrderStatusPending PaymentOrderStatus = "pending"
	// PaymentOrderStatusPaid 表示订单已经完成本地入账。
	PaymentOrderStatusPaid PaymentOrderStatus = "paid"
)

// PaymentProduct 是 Go 自有的、可版本化的钻石商品。
// 订单只从该对象复制钻石数，不从支付回调读取 credits 或金额。
type PaymentProduct struct {
	ID            string
	Version       int64
	DiamondAmount int64
	// AmountCents 和 Currency 是 Go 自有商品冻结的法币定价。
	// 旧 IAP 记录可为空；PayCores 建单会额外强制要求这两个字段完整。
	AmountCents   int64
	Currency      string
	Label         string
	PublishStatus ProductPublishStatus
}

// ValidatePayCoresPricing 校验可提交给 PayCores 的法币商品快照。
// IAP 不使用这个方法，因为商店价格由 Apple/Google 自己验签；PayCores 必须使用 Go 冻结价格。
func (order PaymentOrder) ValidatePayCoresPricing() error {
	if order.AmountCents <= 0 || len(order.Currency) != 3 || strings.TrimSpace(order.Currency) != order.Currency || order.Currency != strings.ToUpper(order.Currency) {
		return ErrInvalidPaymentOrder
	}
	return nil
}

// Validate 校验商品版本能否作为本地订单冻结事实的来源。
func (product PaymentProduct) Validate() error {
	if blank(product.ID) || product.Version <= 0 || product.DiamondAmount <= 0 {
		return ErrInvalidPaymentProduct
	}
	if product.PublishStatus != ProductPublishStatusDraft && product.PublishStatus != ProductPublishStatusPublished {
		return ErrInvalidPaymentProduct
	}
	return nil
}

// CreateOrderInput 是创建本地订单的最小可信输入。
// ProviderOrderID 由未来的本地收银台编排生成并交给支付渠道，不使用 Node 订单号。
type CreateOrderInput struct {
	OrderID         string
	UserID          string
	Provider        Provider
	ProviderOrderID string
}

// PaymentOrder 是在创建时冻结的本地结算事实。
type PaymentOrder struct {
	ID              string
	UserID          string
	Provider        Provider
	ProviderOrderID string
	ProductID       string
	ProductVersion  int64
	DiamondAmount   int64
	// AmountCents 和 Currency 从商品版本复制，支付回调不能覆盖这些快照。
	AmountCents int64
	Currency    string
	Status      PaymentOrderStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ValidatePending 校验可首次持久化的冻结订单只能处于 pending 状态。
func (order PaymentOrder) ValidatePending() error {
	if blank(order.ID) || blank(order.UserID) || !order.Provider.Valid() || blank(order.ProviderOrderID) || blank(order.ProductID) || order.ProductVersion <= 0 || order.DiamondAmount <= 0 || order.CreatedAt.IsZero() || order.UpdatedAt.IsZero() || order.Status != PaymentOrderStatusPending {
		return ErrInvalidPaymentOrder
	}
	return nil
}

// PaymentOrderLocator 定义按渠道及其订单号定位本地冻结订单的只读边界。
// 渠道订单号是可信回调携带的关联，返回的本地订单主键仍须交给 SettleVerifiedOrder 做完整一致性校验。
type PaymentOrderLocator interface {
	FindOrderByProviderOrder(context.Context, Provider, string) (*PaymentOrder, error)
}

// ProductOrderRepository 定义支付商品版本和冻结订单的最小持久化边界。
// 写入订单及其状态迁移必须由实现绑定到同一事务，读取可在事务外用于可靠回放。
type ProductOrderRepository interface {
	PaymentOrderLocator
	CreateProduct(context.Context, PaymentProduct) error
	FindProduct(context.Context, string, int64) (*PaymentProduct, error)
	CreateOrder(context.Context, PaymentOrder) error
	FindOrder(context.Context, string) (*PaymentOrder, error)
	MarkOrderPaid(context.Context, string, time.Time) (bool, error)
}

// FreezeOrder 使用已经发布的本地商品创建待支付订单快照。
// 商品后续变更不会回写该快照，确保支付确认始终按创建时业务事实结算。
func FreezeOrder(input CreateOrderInput, product PaymentProduct, createdAt time.Time) (*PaymentOrder, error) {
	if blank(input.OrderID) || blank(input.UserID) || !input.Provider.Valid() || blank(input.ProviderOrderID) || createdAt.IsZero() {
		return nil, ErrInvalidPaymentOrder
	}
	if err := product.Validate(); err != nil {
		return nil, ErrInvalidPaymentOrder
	}
	if product.PublishStatus != ProductPublishStatusPublished {
		return nil, ErrProductNotPublished
	}
	createdAt = createdAt.UTC()
	return &PaymentOrder{
		ID:              input.OrderID,
		UserID:          input.UserID,
		Provider:        input.Provider,
		ProviderOrderID: input.ProviderOrderID,
		ProductID:       product.ID,
		ProductVersion:  product.Version,
		DiamondAmount:   product.DiamondAmount,
		AmountCents:     product.AmountCents,
		Currency:        product.Currency,
		Status:          PaymentOrderStatusPending,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	}, nil
}
