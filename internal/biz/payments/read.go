package payments

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"ai-business-service/internal/biz/shared"
)

// PaymentMethodsRequest is routing context for the external checkout catalog.
type PaymentMethodsRequest struct {
	UserID               string
	ProductID            string
	Country              string
	ClientDevicePlatform string
}

type PaymentMethod struct {
	Provider      string
	Account       string
	Label         string
	MethodType    string
	Icon          string
	CheckoutType  string
	DiscountRules map[string]PaymentDiscountRule
}

type PaymentDiscountRule struct {
	Enabled  bool
	Discount float64
}

// ReadRepository 仅暴露自有商品和冻结订单查询，读取流程不能获得写账本或渠道查询能力。
type ReadRepository interface {
	ListPublishedProductSnapshots(context.Context) ([]PaymentProduct, error)
	FindOrder(context.Context, string) (*PaymentOrder, error)
}

// OrderStatus 是冻结订单的客户端投影。PaymentReceived 可来自经冻结事实核对的
// PayCores 只读状态；BackendReady 只能由本地原子入账状态派生。
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

// PayCoresOrderStatusSnapshot 是只读渠道快照，绝不可用作入账依据。
type PayCoresOrderStatusSnapshot struct {
	OrderID, ProductID, Status    string
	AmountUSD                     float64
	PaymentReceived, BackendReady bool
}

type PayCoresOrderStatusReader interface {
	GetPayCoresOrderStatus(context.Context, string, string) (PayCoresOrderStatusSnapshot, error)
}

// ReadUsecase 不持有写订单或结算能力；渠道回查仅补充展示信息。
type ReadUsecase struct {
	repository     ReadRepository
	paycoresStatus PayCoresOrderStatusReader
}

func NewReadUsecase(repository ReadRepository) *ReadUsecase {
	return &ReadUsecase{repository: repository}
}

func NewReadUsecaseWithPayCoresStatus(repository ReadRepository, status PayCoresOrderStatusReader) *ReadUsecase {
	return &ReadUsecase{repository: repository, paycoresStatus: status}
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
	result := OrderStatus{OrderID: order.ID, ProductID: order.ProductID, Status: order.Status, Provider: order.Provider, AmountCents: order.AmountCents, Currency: order.Currency, Credits: order.DiamondAmount, PaymentReceived: paid, BackendReady: paid}
	if paid || order.Provider != ProviderPayCores || usecase.paycoresStatus == nil || order.ProviderOrderID == "local-paycores-"+order.ID {
		return result, nil
	}
	// 先检查归属再请求 PayCores；回查失败时保持本地 pending，不延迟或改变账本结算。
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	remote, err := usecase.paycoresStatus.GetPayCoresOrderStatus(statusCtx, order.ProviderOrderID, userID)
	if err == nil && remote.OrderID == order.ProviderOrderID && remote.ProductID == order.ProductID &&
		order.Currency == "USD" && !math.IsNaN(remote.AmountUSD) && !math.IsInf(remote.AmountUSD, 0) &&
		math.Abs(remote.AmountUSD*100-float64(order.AmountCents)) < 1e-6 &&
		remote.Status == "paid" && remote.PaymentReceived {
		result.PaymentReceived = true
	}
	return result, nil
}
