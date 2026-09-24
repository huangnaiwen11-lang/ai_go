package payments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrInvalidStorePurchase 表示验证器输出的商店交易事实不完整或不可信。
	ErrInvalidStorePurchase = errors.New("payments: invalid verified store purchase")
)

// StoreProvider 表示商店验证器的来源。它与账本 Provider 分离，避免传输层将渠道细节
// 或原始回执格式传入结算领域。
type StoreProvider string

const (
	// StoreProviderApple 表示 Apple App Store 已验签购买。
	StoreProviderApple StoreProvider = "apple"
	// StoreProviderGoogle 表示 Google Play 已验签购买。
	StoreProviderGoogle StoreProvider = "google"
)

// Valid 判断商店来源是否属于当前受支持的受控验证器。
func (provider StoreProvider) Valid() bool {
	return provider == StoreProviderApple || provider == StoreProviderGoogle
}

// paymentProvider 转换到账本层用于唯一回执和订单关联的渠道命名空间。
func (provider StoreProvider) paymentProvider() Provider {
	switch provider {
	case StoreProviderApple:
		return ProviderApple
	case StoreProviderGoogle:
		return ProviderGoogle
	default:
		return ""
	}
}

// VerifiedStorePurchase 是 IAP 验证器产生的最小可信购买事实。
// 它刻意不包含客户端金额、钻石、余额、VIP 或原始回执：权益只能由本地商品快照决定。
type VerifiedStorePurchase struct {
	Provider              StoreProvider
	ExternalTransactionID string
	StoreProductID        string
	ProductID             string
	UserID                string
	BusinessAt            time.Time
}

// Normalize 校验验证器输出并为缺失业务时间使用服务端时钟。
func (purchase VerifiedStorePurchase) Normalize(fallback time.Time) (VerifiedStorePurchase, error) {
	if !purchase.Provider.Valid() || blank(purchase.ExternalTransactionID) || blank(purchase.StoreProductID) || blank(purchase.ProductID) || blank(purchase.UserID) {
		return VerifiedStorePurchase{}, ErrInvalidStorePurchase
	}
	if purchase.BusinessAt.IsZero() {
		purchase.BusinessAt = fallback
	}
	if purchase.BusinessAt.IsZero() {
		return VerifiedStorePurchase{}, ErrInvalidStorePurchase
	}
	purchase.BusinessAt = purchase.BusinessAt.UTC()
	return purchase, nil
}

// CheckoutRepository 扩展支付账本仓储以提供当前可售商品读取。
// 读取由事务调用方执行，确保订单创建时冻结的版本与持久化校验在同一 Mongo 事务内。
type CheckoutRepository interface {
	PaymentRepository
	FindLatestPublishedProduct(context.Context, string) (*PaymentProduct, error)
	BindPayCoresProviderOrder(context.Context, string, string, string, time.Time) (bool, error)
}

// PayCoresCheckoutRequest 是 Go 领域交给外部建单适配器的受控事实。
// 适配器不得向其中追加余额、VIP、Node 用户资料或客户端价格。
type PayCoresCheckoutRequest struct {
	UserID               string
	ProductID            string
	AmountCents          int64
	Currency             string
	Credits              int64
	Label                string
	ClientRequestID      string
	Provider             string
	Account              string
	ClientDevicePlatform string
}

// PaymentChannelSelection is the user-visible PayCores channel selected by
// the Web checkout. Provider/account are opaque identifiers owned by PayCores;
// the client cannot alter price or credits through them.
type PaymentChannelSelection struct {
	Provider             string
	Account              string
	ClientDevicePlatform string
	ClientRequestID      string
}

func (selection PaymentChannelSelection) Validate() error {
	selection = selection.normalized()
	provider := selection.Provider
	account := selection.Account
	platform := selection.ClientDevicePlatform
	if provider == "" {
		if account != "" {
			return ErrInvalidPaymentOrder
		}
		return nil
	}
	if strings.HasSuffix(account, "_googlepay") && platform != "web" && platform != "android" {
		return ErrInvalidPaymentOrder
	}
	if strings.HasSuffix(account, "_applepay") && platform != "ios" {
		return ErrInvalidPaymentOrder
	}
	if requestID := strings.TrimSpace(selection.ClientRequestID); requestID != "" {
		if len(requestID) > 128 || strings.IndexFunc(requestID, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return ErrInvalidPaymentOrder
		}
	}
	return nil
}

func (selection PaymentChannelSelection) normalized() PaymentChannelSelection {
	selection.Provider = strings.TrimSpace(selection.Provider)
	selection.Account = strings.ToLower(strings.TrimSpace(selection.Account))
	selection.ClientDevicePlatform = strings.ToLower(strings.TrimSpace(selection.ClientDevicePlatform))
	selection.ClientRequestID = strings.TrimSpace(selection.ClientRequestID)
	if selection.ClientDevicePlatform == "" {
		selection.ClientDevicePlatform = "web"
	}
	return selection
}

// PayCoresCheckoutResult 是外部建单成功后的最小可信响应。
type PayCoresCheckoutResult struct {
	ProviderOrderID string
	CheckoutURL     string
}

// PayCoresCheckoutCreator 把 HTTP 细节隔离在支付领域之外。
type PayCoresCheckoutCreator interface {
	CreatePayCoresOrder(context.Context, PayCoresCheckoutRequest) (PayCoresCheckoutResult, error)
}

// PayCoresCheckout 是绑定真实渠道订单后的收银台响应。
type PayCoresCheckout struct {
	Order       *PaymentOrder
	CheckoutURL string
}

// CheckoutService 负责两类本地支付事实：收银台待支付订单及验证完成后的商店购买入账。
// 它不调用 PayCores、Apple 或 Google；外部交互必须在其边界之外完成并提供已验证结果。
type CheckoutService struct {
	repository CheckoutRepository
	settler    *Service
	paycores   PayCoresCheckoutCreator
	now        func() time.Time
	newOrderID func() (string, error)
}

// CreatePendingPayCoresOrder 为本地收银台测试创建待确认的 Go 自有订单。
// 它只冻结服务端商品和受控的默认 Web 渠道，不调用 PayCores，也不写入客户端金额或钻石数。
func (service *CheckoutService) CreatePendingPayCoresOrder(ctx context.Context, userID, productID string) (*PaymentOrder, error) {
	return service.createPendingPayCoresOrder(ctx, userID, productID, PaymentChannelSelection{ClientDevicePlatform: "web"})
}

func (service *CheckoutService) createPendingPayCoresOrder(ctx context.Context, userID, productID string, channel PaymentChannelSelection) (*PaymentOrder, error) {
	if service == nil || service.repository == nil || service.newOrderID == nil || blank(userID) || blank(productID) {
		return nil, ErrInvalidPaymentOrder
	}
	clientRequestID := channel.ClientRequestID
	var created *PaymentOrder
	err := service.repository.WithinTx(ctx, func(txCtx context.Context) error {
		product, err := service.repository.FindLatestPublishedProduct(txCtx, productID)
		if err != nil {
			return err
		}
		if product == nil {
			return ErrProductNotPublished
		}
		orderID := ""
		if strings.TrimSpace(clientRequestID) != "" {
			orderID = clientPaymentOrderID(userID, productID, clientRequestID)
		} else {
			orderID, err = service.newOrderID()
			if err != nil {
				return fmt.Errorf("create local PayCores payment order id: %w", err)
			}
		}
		order, err := FreezeOrder(CreateOrderInput{
			OrderID:               orderID,
			UserID:                userID,
			Provider:              ProviderPayCores,
			ProviderOrderID:       "local-paycores-" + orderID,
			ChannelProvider:       channel.Provider,
			ChannelAccount:        channel.Account,
			ChannelDevicePlatform: channel.ClientDevicePlatform,
		}, *product, service.now())
		if err != nil {
			return err
		}
		if err := service.repository.CreateOrder(txCtx, *order); err != nil {
			return err
		}
		created = order
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrPaymentOrderAlreadyExists) && strings.TrimSpace(clientRequestID) != "" {
			existing, lookupErr := service.repository.FindOrder(ctx, clientPaymentOrderID(userID, productID, clientRequestID))
			if lookupErr != nil {
				return nil, lookupErr
			}
			if existing != nil && existing.UserID == userID && existing.ProductID == productID && existing.Provider == ProviderPayCores {
				return existing, nil
			}
		}
		return nil, err
	}
	return created, nil
}

// NewCheckoutService 创建本地支付订单与 IAP 结算服务。
func NewCheckoutService(repository CheckoutRepository, now func() time.Time) *CheckoutService {
	return NewCheckoutServiceWithPayCores(repository, nil, now)
}

// NewCheckoutServiceWithPayCores 装配本地订单服务与可替换的 PayCores 建单边界。
// 未传入创建器时仍可用于本地 IAP 与旧的待确认订单测试。
func NewCheckoutServiceWithPayCores(repository CheckoutRepository, creator PayCoresCheckoutCreator, now func() time.Time) *CheckoutService {
	if now == nil {
		now = time.Now
	}
	return &CheckoutService{
		repository: repository,
		settler:    NewService(repository, now),
		paycores:   creator,
		now:        now,
		newOrderID: randomCheckoutOrderID,
	}
}

// CreatePayCoresCheckout 先冻结 Go 自有订单，再请求 PayCores，最后原子绑定渠道订单号。
// 请求失败不会入账；回调只会按已绑定订单的本地快照结算，绝不采信对方传回的金额或 credits。
func (service *CheckoutService) CreatePayCoresCheckout(ctx context.Context, userID, productID string) (PayCoresCheckout, error) {
	return service.CreatePayCoresCheckoutForChannel(ctx, userID, productID, PaymentChannelSelection{ClientDevicePlatform: "web"})
}

// CreatePayCoresCheckoutForChannel creates the same frozen order while
// preserving the selected PayCores provider/account for routing.
func (service *CheckoutService) CreatePayCoresCheckoutForChannel(ctx context.Context, userID, productID string, selection PaymentChannelSelection) (PayCoresCheckout, error) {
	if service == nil || service.paycores == nil {
		return PayCoresCheckout{}, ErrDependenciesUnavailable
	}
	channel := selection.normalized()
	if err := channel.Validate(); err != nil {
		return PayCoresCheckout{}, err
	}
	order, err := service.createPendingPayCoresOrder(ctx, userID, productID, channel)
	if err != nil {
		return PayCoresCheckout{}, err
	}
	if !order.matchesPayCoresChannel(channel) {
		return PayCoresCheckout{}, ErrPaymentOrderMismatch
	}
	if err := order.ValidatePayCoresPricing(); err != nil {
		return PayCoresCheckout{}, err
	}
	product, err := service.repository.FindProduct(ctx, order.ProductID, order.ProductVersion)
	if err != nil {
		return PayCoresCheckout{}, err
	}
	if product == nil {
		return PayCoresCheckout{}, ErrInvalidPaymentOrder
	}
	orderClientRequestID := channel.ClientRequestID
	if orderClientRequestID == "" {
		orderClientRequestID = order.ID
	}
	response, err := service.paycores.CreatePayCoresOrder(ctx, PayCoresCheckoutRequest{
		UserID: userID, ProductID: order.ProductID, AmountCents: order.AmountCents, Currency: order.Currency,
		Credits: order.DiamondAmount, Label: product.Label, ClientRequestID: orderClientRequestID,
		Provider: channel.Provider, Account: channel.Account, ClientDevicePlatform: channel.ClientDevicePlatform,
	})
	if err != nil {
		return PayCoresCheckout{}, err
	}
	if blank(response.ProviderOrderID) || blank(response.CheckoutURL) {
		return PayCoresCheckout{}, ErrInvalidPaymentOrder
	}
	if order.ProviderOrderID != "local-paycores-"+order.ID {
		if order.ProviderOrderID != response.ProviderOrderID {
			return PayCoresCheckout{}, ErrPaymentOrderMismatch
		}
		order.UpdatedAt = service.now().UTC()
		return PayCoresCheckout{Order: order, CheckoutURL: response.CheckoutURL}, nil
	}
	bound, err := service.repository.BindPayCoresProviderOrder(ctx, order.ID, order.ProviderOrderID, response.ProviderOrderID, service.now())
	if err != nil {
		return PayCoresCheckout{}, err
	}
	if !bound {
		// 同一幂等请求可能已由并发调用完成绑定；只允许完全相同的渠道订单和冻结快照收敛成功。
		current, lookupErr := service.repository.FindOrder(ctx, order.ID)
		if lookupErr != nil {
			return PayCoresCheckout{}, lookupErr
		}
		if current == nil || current.ID != order.ID || current.UserID != order.UserID || current.Provider != order.Provider || !current.matchesPayCoresChannel(channel) || current.ProviderOrderID != response.ProviderOrderID || current.ProductID != order.ProductID || current.ProductVersion != order.ProductVersion || current.DiamondAmount != order.DiamondAmount || current.AmountCents != order.AmountCents || current.Currency != order.Currency || (current.Status != PaymentOrderStatusPending && current.Status != PaymentOrderStatusPaid) {
			return PayCoresCheckout{}, ErrPaymentOrderMismatch
		}
		return PayCoresCheckout{Order: current, CheckoutURL: response.CheckoutURL}, nil
	}
	order.ProviderOrderID = response.ProviderOrderID
	order.UpdatedAt = service.now().UTC()
	return PayCoresCheckout{Order: order, CheckoutURL: response.CheckoutURL}, nil
}

func (order PaymentOrder) matchesPayCoresChannel(selection PaymentChannelSelection) bool {
	return order.ChannelProvider == selection.Provider && order.ChannelAccount == selection.Account && order.ChannelDevicePlatform == selection.ClientDevicePlatform
}

// SettleVerifiedStorePurchase 根据已经验签的商店交易创建或复用冻结订单，再按订单快照结算。
// 同一商店交易重放不会二次入账；交易被其他用户或不同商品占用时必须明确拒绝。
func (service *CheckoutService) SettleVerifiedStorePurchase(ctx context.Context, purchase VerifiedStorePurchase) (ApplyResult, error) {
	if service == nil || service.repository == nil || service.settler == nil || service.newOrderID == nil {
		return ApplyResult{}, ErrDependenciesUnavailable
	}
	normalized, err := purchase.Normalize(service.now())
	if err != nil {
		return ApplyResult{}, err
	}
	provider := normalized.Provider.paymentProvider()
	if !provider.Valid() {
		return ApplyResult{}, ErrInvalidStorePurchase
	}

	order, err := service.freezeStoreOrder(ctx, normalized, provider)
	if err != nil {
		return ApplyResult{}, err
	}
	return service.settler.SettleVerifiedOrder(ctx, VerifiedOrderSettlement{
		PaymentOrderID:        order.ID,
		UserID:                normalized.UserID,
		Provider:              provider,
		ProviderOrderID:       normalized.ExternalTransactionID,
		ExternalTransactionID: normalized.ExternalTransactionID,
		BusinessAt:            normalized.BusinessAt,
	})
}

// freezeStoreOrder 将外部交易号同时作为商店订单关联和回执幂等键的来源。
// 新建订单失败于唯一键时只允许收敛到完全一致的既有订单，防止跨用户或换商品重放。
func (service *CheckoutService) freezeStoreOrder(ctx context.Context, purchase VerifiedStorePurchase, provider Provider) (*PaymentOrder, error) {
	var created *PaymentOrder
	err := service.repository.WithinTx(ctx, func(txCtx context.Context) error {
		product, err := service.repository.FindLatestPublishedProduct(txCtx, purchase.ProductID)
		if err != nil {
			return err
		}
		if product == nil {
			return ErrProductNotPublished
		}
		orderID, err := service.newOrderID()
		if err != nil {
			return fmt.Errorf("create local store payment order id: %w", err)
		}
		order, err := FreezeOrder(CreateOrderInput{
			OrderID:         orderID,
			UserID:          purchase.UserID,
			Provider:        provider,
			ProviderOrderID: purchase.ExternalTransactionID,
		}, *product, purchase.BusinessAt)
		if err != nil {
			return err
		}
		if err := service.repository.CreateOrder(txCtx, *order); err != nil {
			return err
		}
		created = order
		return nil
	})
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, ErrPaymentOrderAlreadyExists) {
		return nil, err
	}
	existing, lookupErr := service.repository.FindOrderByProviderOrder(ctx, provider, purchase.ExternalTransactionID)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if existing == nil || existing.UserID != purchase.UserID || existing.ProductID != purchase.ProductID || existing.Provider != provider || existing.ProviderOrderID != purchase.ExternalTransactionID {
		// 与已存在订单的用户、商品或渠道不一致时复用统一订单关联错误，
		// 让回调与 IAP 两条支付入口对同类攻击返回一致的领域语义。
		return nil, ErrPaymentOrderMismatch
	}
	return existing, nil
}

// randomCheckoutOrderID 生成只用于 Go 自有订单主键的随机标识；它绝不与 Node 或真实渠道订单混用。
func randomCheckoutOrderID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "local-payment-" + hex.EncodeToString(bytes), nil
}

func clientPaymentOrderID(userID, productID, clientRequestID string) string {
	digest := sha256.Sum256([]byte(userID + "\x00" + productID + "\x00" + strings.TrimSpace(clientRequestID)))
	return "client-payment-" + hex.EncodeToString(digest[:])
}
