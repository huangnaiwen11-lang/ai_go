package payments

import (
	"context"
	"errors"
	"time"
)

const paymentCreditReason = "payment_credit"

var (
	// ErrPaymentOrderNotFound 表示已验证回调引用的本地订单不存在。
	ErrPaymentOrderNotFound = errors.New("payments: payment order not found")
	// ErrPaymentOrderMismatch 表示回调携带的订单关联与本地冻结事实不一致。
	ErrPaymentOrderMismatch = errors.New("payments: payment order association does not match")
	// ErrPaymentSettlementInconsistent 表示订单、回执或账本的本地事务事实出现不应存在的组合。
	ErrPaymentSettlementInconsistent = errors.New("payments: payment settlement facts are inconsistent")
)

// Repository 是支付领域需要的最小原子持久化边界。
// 实现必须确保 WithinTx 内的任一写入失败都会回滚回执、余额和账本分录。
type Repository interface {
	WithinTx(context.Context, func(context.Context) error) error
	FindReceipt(context.Context, Provider, string) (*Receipt, error)
	FindDiamondBalance(context.Context, string) (int64, error)
	CreateReceipt(context.Context, Receipt) error
	CreditDiamonds(context.Context, string, int64, time.Time) (int64, error)
	AppendPaymentCredit(context.Context, PaymentCredit) error
}

// PaymentRepository 聚合支付回执入账与商品订单的本地 Mongo 持久化能力。
// 保留 Repository 的最小入账边界，避免现有纯领域测试的伪仓储承担无关职责。
type PaymentRepository interface {
	Repository
	ProductOrderRepository
}

// PaymentCredit 是支付成功后写入本地账本的正向分录。
// 回执与原因一同传给仓储，防止持久化层自行猜测分录语义。
type PaymentCredit struct {
	Receipt   Receipt
	Reason    string
	CreatedAt time.Time
}

// ApplyResult 是一次回执处理后的稳定响应投影。
// Applied 为 false 仅表示同一业务事实已入账，绝不表示渠道支付失败。
type ApplyResult struct {
	Applied        bool
	DiamondBalance int64
}

// VerifiedOrderSettlement 是渠道已完成校验后提交给本地订单结算层的最小关联事实。
// 它刻意没有金额或钻石数，入账数量只能来自 PaymentOrder 的冻结快照。
type VerifiedOrderSettlement struct {
	PaymentOrderID        string
	UserID                string
	Provider              Provider
	ProviderOrderID       string
	ExternalTransactionID string
	BusinessAt            time.Time
}

// Service 编排已经验证的支付回执入账。
// 它不注册 HTTP 路由，不调用支付渠道，也不读取或写入 Node 钱包。
type Service struct {
	repository Repository
	now        func() time.Time
}

// NewService 创建本地支付回执入账服务。
func NewService(repository Repository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, now: now}
}

// applyReceipt 在同一事务中创建唯一回执、增加本地钻石余额并记录 payment_credit 分录。
// 相同的可信回执重放会返回首次入账后的当前余额，但不会再次增加钻石。
// 该方法仅供同包测试和事务内复用；包外结算必须使用 SettleVerifiedOrder。
func (service *Service) applyReceipt(ctx context.Context, receipt Receipt) (ApplyResult, error) {
	if service == nil || service.repository == nil {
		return ApplyResult{}, ErrDependenciesUnavailable
	}

	normalized, err := receipt.Normalize(service.now())
	if err != nil {
		return ApplyResult{}, err
	}

	var result ApplyResult
	err = service.repository.WithinTx(ctx, func(txCtx context.Context) error {
		var applyErr error
		result, applyErr = service.applyReceiptWithinTx(txCtx, normalized)
		return applyErr
	})
	if err != nil {
		if errors.Is(err, ErrReceiptAlreadyExists) {
			return service.resolveExistingReceipt(ctx, normalized)
		}
		return ApplyResult{}, err
	}
	return result, nil
}

// SettleVerifiedOrder 确认已验证回调对应的本地订单，并按订单冻结钻石数原子入账。
// 渠道、渠道订单号、用户和外部交易号只用于关联验证，绝不构成金额或钻石的输入来源。
func (service *Service) SettleVerifiedOrder(ctx context.Context, settlement VerifiedOrderSettlement) (ApplyResult, error) {
	if service == nil || service.repository == nil {
		return ApplyResult{}, ErrDependenciesUnavailable
	}
	repository, ok := service.repository.(PaymentRepository)
	if !ok {
		return ApplyResult{}, ErrDependenciesUnavailable
	}
	if blank(settlement.PaymentOrderID) || blank(settlement.UserID) || !settlement.Provider.Valid() || blank(settlement.ProviderOrderID) || blank(settlement.ExternalTransactionID) || settlement.BusinessAt.IsZero() {
		return ApplyResult{}, ErrInvalidReceipt
	}

	var result ApplyResult
	err := repository.WithinTx(ctx, func(txCtx context.Context) error {
		order, err := repository.FindOrder(txCtx, settlement.PaymentOrderID)
		if err != nil {
			return err
		}
		if order == nil {
			return ErrPaymentOrderNotFound
		}
		if err := validateSettlementOrder(*order); err != nil {
			return err
		}
		if order.UserID != settlement.UserID || order.Provider != settlement.Provider || order.ProviderOrderID != settlement.ProviderOrderID {
			return ErrPaymentOrderMismatch
		}

		receipt, err := (Receipt{
			Provider:              order.Provider,
			ExternalTransactionID: settlement.ExternalTransactionID,
			PaymentOrderID:        order.ID,
			UserID:                order.UserID,
			DiamondAmount:         order.DiamondAmount,
			BusinessAt:            settlement.BusinessAt,
		}).Normalize(service.now())
		if err != nil {
			return err
		}

		existing, err := repository.FindReceipt(txCtx, receipt.Provider, receipt.ExternalTransactionID)
		if err != nil {
			return err
		}
		if existing != nil {
			if order.Status == PaymentOrderStatusPaid {
				if !receipt.SameFact(*existing) {
					return ErrPaymentSettlementInconsistent
				}
				balance, err := repository.FindDiamondBalance(txCtx, receipt.UserID)
				if err != nil {
					return err
				}
				result = ApplyResult{Applied: false, DiamondBalance: balance}
				return nil
			}
			if !receipt.SameFact(*existing) {
				return ErrReceiptConflict
			}
			if order.Status != PaymentOrderStatusPending {
				return ErrPaymentSettlementInconsistent
			}
			return ErrPaymentSettlementInconsistent
		}
		if order.Status != PaymentOrderStatusPending {
			return ErrPaymentSettlementInconsistent
		}
		transitioned, err := repository.MarkOrderPaid(txCtx, order.ID, receipt.BusinessAt)
		if err != nil {
			return err
		}
		if !transitioned {
			return ErrPaymentSettlementInconsistent
		}
		result, err = service.recordReceiptWithinTx(txCtx, receipt)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrReceiptAlreadyExists) {
			return service.resolveVerifiedOrderSettlement(ctx, repository, settlement)
		}
		return ApplyResult{}, err
	}
	return result, nil
}

// applyReceiptWithinTx 是 ApplyReceipt 与订单结算共用的回执、余额、账本事务内路径。
func (service *Service) applyReceiptWithinTx(ctx context.Context, receipt Receipt) (ApplyResult, error) {
	existing, err := service.repository.FindReceipt(ctx, receipt.Provider, receipt.ExternalTransactionID)
	if err != nil {
		return ApplyResult{}, err
	}
	if existing != nil {
		if !receipt.SameFact(*existing) {
			return ApplyResult{}, ErrReceiptConflict
		}
		balance, err := service.repository.FindDiamondBalance(ctx, receipt.UserID)
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Applied: false, DiamondBalance: balance}, nil
	}
	return service.recordReceiptWithinTx(ctx, receipt)
}

// recordReceiptWithinTx 写入首次出现的回执、余额和本地 payment_credit 分录。
func (service *Service) recordReceiptWithinTx(ctx context.Context, receipt Receipt) (ApplyResult, error) {
	if err := service.repository.CreateReceipt(ctx, receipt); err != nil {
		return ApplyResult{}, err
	}
	balance, err := service.repository.CreditDiamonds(ctx, receipt.UserID, receipt.DiamondAmount, receipt.BusinessAt)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := service.repository.AppendPaymentCredit(ctx, PaymentCredit{
		Receipt:   receipt,
		Reason:    paymentCreditReason,
		CreatedAt: receipt.BusinessAt,
	}); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: true, DiamondBalance: balance}, nil
}

// resolveVerifiedOrderSettlement 在唯一交易竞争后读取已提交的订单和回执，收敛为幂等重放。
func (service *Service) resolveVerifiedOrderSettlement(ctx context.Context, repository PaymentRepository, settlement VerifiedOrderSettlement) (ApplyResult, error) {
	order, err := repository.FindOrder(ctx, settlement.PaymentOrderID)
	if err != nil {
		return ApplyResult{}, err
	}
	if order == nil {
		return ApplyResult{}, ErrPaymentOrderNotFound
	}
	if err := validateSettlementOrder(*order); err != nil {
		return ApplyResult{}, err
	}
	if order.UserID != settlement.UserID || order.Provider != settlement.Provider || order.ProviderOrderID != settlement.ProviderOrderID {
		return ApplyResult{}, ErrPaymentOrderMismatch
	}
	receipt, err := (Receipt{
		Provider:              order.Provider,
		ExternalTransactionID: settlement.ExternalTransactionID,
		PaymentOrderID:        order.ID,
		UserID:                order.UserID,
		DiamondAmount:         order.DiamondAmount,
		BusinessAt:            settlement.BusinessAt,
	}).Normalize(service.now())
	if err != nil {
		return ApplyResult{}, err
	}
	existing, err := repository.FindReceipt(ctx, receipt.Provider, receipt.ExternalTransactionID)
	if err != nil {
		return ApplyResult{}, err
	}
	if existing == nil {
		return ApplyResult{}, ErrReceiptAlreadyExists
	}
	if order.Status != PaymentOrderStatusPaid {
		if !receipt.SameFact(*existing) {
			return ApplyResult{}, ErrReceiptConflict
		}
		return ApplyResult{}, ErrPaymentSettlementInconsistent
	}
	if !receipt.SameFact(*existing) {
		return ApplyResult{}, ErrPaymentSettlementInconsistent
	}
	balance, err := repository.FindDiamondBalance(ctx, receipt.UserID)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: false, DiamondBalance: balance}, nil
}

// validateSettlementOrder 校验订单冻结快照在 pending 或 paid 状态下仍保持完整。
func validateSettlementOrder(order PaymentOrder) error {
	if order.Status != PaymentOrderStatusPending && order.Status != PaymentOrderStatusPaid {
		return ErrInvalidPaymentOrder
	}
	order.Status = PaymentOrderStatusPending
	return order.ValidatePending()
}

// resolveExistingReceipt 在并发写入撞上唯一索引后重新读取已提交的支付事实。
// 重新读取必须在失败事务结束后执行，否则事务快照可能仍不可见对方刚提交的回执。
func (service *Service) resolveExistingReceipt(ctx context.Context, receipt Receipt) (ApplyResult, error) {
	existing, err := service.repository.FindReceipt(ctx, receipt.Provider, receipt.ExternalTransactionID)
	if err != nil {
		return ApplyResult{}, err
	}
	if existing == nil {
		// 这表示仓储错误地把非唯一键错误映射为了 ErrReceiptAlreadyExists，不能误报为成功。
		return ApplyResult{}, ErrReceiptAlreadyExists
	}
	if !receipt.SameFact(*existing) {
		return ApplyResult{}, ErrReceiptConflict
	}
	balance, err := service.repository.FindDiamondBalance(ctx, receipt.UserID)
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Applied: false, DiamondBalance: balance}, nil
}
