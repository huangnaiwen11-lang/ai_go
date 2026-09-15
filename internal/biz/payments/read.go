package payments

import (
	"context"
	"sort"
	"strings"

	"ai-business-service/internal/biz/shared"
)

// ReadRepository 仅暴露自有商品和冻结订单查询，读取流程不能获得写账本或渠道查询能力。
type ReadRepository interface {
	ListPublishedProductSnapshots(context.Context) ([]PaymentProduct, error)
	FindOrder(context.Context, string) (*PaymentOrder, error)
}

// OrderStatus 是冻结订单的客户端投影；两个完成标记只由本地原子入账状态派生。
type OrderStatus struct {
	OrderID         string
	ProductID       string
	Status          PaymentOrderStatus
	Provider        Provider
	AmountCents     int64
	Currency        string
	Credits         int64
	PaymentReceived bool
	BackendReady    bool
}

// ReadUsecase 提供只读支付查询，不注入 PayCores 或结算服务。
type ReadUsecase struct{ repository ReadRepository }

func NewReadUsecase(repository ReadRepository) *ReadUsecase {
	return &ReadUsecase{repository: repository}
}

// ListProducts 按 ID 升序返回最新已发布且可支付的版本，不把无效新价格降级到旧版本。
func (usecase *ReadUsecase) ListProducts(ctx context.Context) ([]PaymentProduct, error) {
	if usecase == nil || usecase.repository == nil {
		return nil, ErrDependenciesUnavailable
	}
	snapshots, err := usecase.repository.ListPublishedProductSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]PaymentProduct, len(snapshots))
	for _, product := range snapshots {
		if product.PublishStatus != ProductPublishStatusPublished {
			continue
		}
		current, found := latest[product.ID]
		if !found || product.Version > current.Version {
			latest[product.ID] = product
		}
	}
	products := make([]PaymentProduct, 0, len(latest))
	for _, product := range latest {
		// 当前 PayCores create-order 固定 USD，适配器也不发送币种；不能将其它币种展示为可支付商品。
		if product.Validate() != nil || product.AmountCents <= 0 || product.Currency != "USD" || strings.TrimSpace(product.Label) == "" {
			continue
		}
		products = append(products, product)
	}
	sort.Slice(products, func(i, j int) bool { return products[i].ID < products[j].ID })
	return products, nil
}

// OrderStatus 先检查登录归属，再投影冻结价格；不存在与非本人订单统一不可见。
func (usecase *ReadUsecase) OrderStatus(ctx context.Context, userID, orderID string) (OrderStatus, error) {
	if blank(userID) {
		return OrderStatus{}, shared.ErrUnauthenticated
	}
	if usecase == nil || usecase.repository == nil {
		return OrderStatus{}, ErrDependenciesUnavailable
	}
	if blank(orderID) {
		return OrderStatus{}, ErrPaymentOrderNotFound
	}
	order, err := usecase.repository.FindOrder(ctx, orderID)
	if err != nil {
		return OrderStatus{}, err
	}
	if order == nil || order.UserID != userID || order.ID != orderID {
		return OrderStatus{}, ErrPaymentOrderNotFound
	}
	if err := validateSettlementOrder(*order); err != nil {
		return OrderStatus{}, err
	}
	paid := order.Status == PaymentOrderStatusPaid
	return OrderStatus{OrderID: order.ID, ProductID: order.ProductID, Status: order.Status, Provider: order.Provider, AmountCents: order.AmountCents, Currency: order.Currency, Credits: order.DiamondAmount, PaymentReceived: paid, BackendReady: paid}, nil
}
