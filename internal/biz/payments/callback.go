package payments

import (
	"context"
	"errors"
	"time"
)

const paymentCallbackNonceTTL = 2 * time.Minute

var (
	// ErrInvalidVerifiedPaymentConfirmation 表示验签边界没有产出可用于本地结算的完整关联。
	ErrInvalidVerifiedPaymentConfirmation = errors.New("payments: invalid verified payment confirmation")
	// ErrConfirmedCallbackDependenciesUnavailable 表示回调编排尚未获得 nonce 存储或订单结算依赖。
	ErrConfirmedCallbackDependenciesUnavailable = errors.New("payments: confirmed callback dependencies unavailable")
)

// VerifiedPaymentConfirmation 是从验签边界映射到 payments 的受控领域对象。
// 它只保留用户、本地订单、渠道交易三项关联和 nonce 摘要，不接收 HTTP、签名、原 nonce 或金额权益事实。
type VerifiedPaymentConfirmation struct {
	UserID        string
	OrderID       string
	ProviderTxnID string
	NonceHash     PaymentCallbackNonceHash
}

// NewVerifiedPaymentConfirmation 在受控边界将验签后的三个关联与 nonce 摘要映射为 payments DO。
func NewVerifiedPaymentConfirmation(userID, orderID, providerTxnID string, nonceHash PaymentCallbackNonceHash) (VerifiedPaymentConfirmation, error) {
	confirmation := VerifiedPaymentConfirmation{
		UserID:        userID,
		OrderID:       orderID,
		ProviderTxnID: providerTxnID,
		NonceHash:     nonceHash,
	}
	if !confirmation.Valid() {
		return VerifiedPaymentConfirmation{}, ErrInvalidVerifiedPaymentConfirmation
	}
	return confirmation, nil
}

// Valid 判断验签边界投影是否足以关联既有冻结订单。
func (confirmation VerifiedPaymentConfirmation) Valid() bool {
	return !blank(confirmation.UserID) && !blank(confirmation.OrderID) && !blank(confirmation.ProviderTxnID) && confirmation.NonceHash.Valid()
}

type verifiedOrderSettler interface {
	SettleVerifiedOrder(context.Context, VerifiedOrderSettlement) (ApplyResult, error)
}

// ConfirmedCallbackUsecase 编排已验签的 PayCores 回调：先原子消费 nonce，再结算既有冻结订单。
// callbackPath 是组合期固定的已验签路径，不能由 Handle 的回调输入提供。
type ConfirmedCallbackUsecase struct {
	nonces       PaymentCallbackNonceStore
	orders       PaymentOrderLocator
	settler      verifiedOrderSettler
	callbackPath string
	now          func() time.Time
}

// NewConfirmedCallbackUsecase 创建仅处理验签成功投影的回调编排器。
func NewConfirmedCallbackUsecase(nonces PaymentCallbackNonceStore, orders PaymentOrderLocator, settler verifiedOrderSettler, callbackPath string, now func() time.Time) *ConfirmedCallbackUsecase {
	if now == nil {
		now = time.Now
	}
	return &ConfirmedCallbackUsecase{nonces: nonces, orders: orders, settler: settler, callbackPath: callbackPath, now: now}
}

// Handle 消费 nonce 后调用既有 SettleVerifiedOrder；nonce 重放或存储错误会直接返回且绝不结算。
func (usecase *ConfirmedCallbackUsecase) Handle(ctx context.Context, confirmation VerifiedPaymentConfirmation) (ApplyResult, error) {
	if usecase == nil || usecase.nonces == nil || usecase.orders == nil || usecase.settler == nil || usecase.now == nil || !confirmedCallbackPath(usecase.callbackPath) {
		return ApplyResult{}, ErrConfirmedCallbackDependenciesUnavailable
	}
	if !confirmation.Valid() {
		return ApplyResult{}, ErrInvalidVerifiedPaymentConfirmation
	}

	businessAt := usecase.now().UTC()
	if businessAt.IsZero() {
		return ApplyResult{}, ErrConfirmedCallbackDependenciesUnavailable
	}
	// nonce 唯一消费是防重放门禁；一经消费不可回滚，定位或结算失败也不能绕过该门禁再次执行。
	if err := usecase.nonces.Consume(ctx, confirmation.NonceHash, "POST", usecase.callbackPath, businessAt.Add(paymentCallbackNonceTTL)); err != nil {
		return ApplyResult{}, err
	}
	order, err := usecase.orders.FindOrderByProviderOrder(ctx, ProviderPayCores, confirmation.OrderID)
	if err != nil {
		return ApplyResult{}, err
	}
	if order == nil || blank(order.ID) {
		return ApplyResult{}, ErrPaymentOrderNotFound
	}
	return usecase.settler.SettleVerifiedOrder(ctx, verifiedOrderSettlement(confirmation, order.ID, businessAt))
}

// verifiedOrderSettlement 显式完成受控关联到既有本地订单结算命令的映射。
// 回调的 OrderID 只作为 PayCores 渠道订单号；本地订单主键必须由 PaymentOrderLocator 查询取得。
// 金额和钻石数仍仅从既有冻结订单快照读取。
func verifiedOrderSettlement(confirmation VerifiedPaymentConfirmation, paymentOrderID string, businessAt time.Time) VerifiedOrderSettlement {
	return VerifiedOrderSettlement{
		PaymentOrderID:        paymentOrderID,
		UserID:                confirmation.UserID,
		Provider:              ProviderPayCores,
		ProviderOrderID:       confirmation.OrderID,
		ExternalTransactionID: confirmation.ProviderTxnID,
		BusinessAt:            businessAt,
	}
}

func confirmedCallbackPath(path string) bool {
	return path == "/api/internal/payment-confirmed" || path == "/api/v1/internal/payment-confirmed"
}

var _ verifiedOrderSettler = (*Service)(nil)
