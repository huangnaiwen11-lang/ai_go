package payments

import (
	"errors"
	"strings"
	"time"
)

// Provider 表示已经由上游完成可信校验的支付渠道。
// 领域层只依赖渠道标识，不接触签名、令牌或原始购买凭证。
type Provider string

const (
	// ProviderPayCores 表示来自 PayCores 的一次性支付事实。
	ProviderPayCores Provider = "paycores"
	// ProviderAppStore 保留早期合并应用商店渠道的历史本地回执兼容性。
	// 新增商店购买必须使用 ProviderApple 或 ProviderGoogle，以隔离交易号命名空间。
	ProviderAppStore Provider = "app_store"
	// ProviderApple 表示已由 Apple 验证器确认的应用内购买事实。
	ProviderApple Provider = "apple"
	// ProviderGoogle 表示已由 Google Play 验证器确认的应用内购买事实。
	ProviderGoogle Provider = "google"
)

var (
	// ErrInvalidReceipt 表示回执缺少幂等入账所必需的规范化业务事实。
	ErrInvalidReceipt = errors.New("payments: invalid receipt")
	// ErrReceiptConflict 表示同一渠道交易被携带不一致的入账事实重放。
	ErrReceiptConflict = errors.New("payments: receipt conflicts with existing transaction")
	// ErrReceiptAlreadyExists 表示并发事务已先一步写入相同的渠道交易。
	// 仓储必须把唯一索引重复键映射为该错误，服务随后会重新读取并收敛为幂等结果。
	ErrReceiptAlreadyExists = errors.New("payments: receipt already exists")
	// ErrDependenciesUnavailable 表示支付服务尚未配置完整的事务仓储。
	ErrDependenciesUnavailable = errors.New("payments: dependencies unavailable")
)

// Receipt 是已由上游验证的规范化支付回执。
// 它故意不保存原始签名、购买凭证、支付金额、余额或 VIP 信息，避免支付渠道细节
// 和主站权益规则泄露到入账领域。
type Receipt struct {
	Provider              Provider
	ExternalTransactionID string
	PaymentOrderID        string
	UserID                string
	DiamondAmount         int64
	BusinessAt            time.Time
}

// Normalize 校验回执的最小业务事实，并固定为 UTC 时刻。
// 调用方应在进入可能重试的事务之前执行它，保证同一次入账重试使用完全相同的业务时间。
func (receipt Receipt) Normalize(fallbackTime time.Time) (Receipt, error) {
	if !receipt.Provider.Valid() || blank(receipt.ExternalTransactionID) || blank(receipt.PaymentOrderID) || blank(receipt.UserID) || receipt.DiamondAmount <= 0 {
		return Receipt{}, ErrInvalidReceipt
	}
	if receipt.BusinessAt.IsZero() {
		receipt.BusinessAt = fallbackTime
	}
	if receipt.BusinessAt.IsZero() {
		return Receipt{}, ErrInvalidReceipt
	}
	receipt.BusinessAt = receipt.BusinessAt.UTC()
	return receipt, nil
}

// blank 判断标识是否缺失或仅由空白字符组成。
// 非空标识保留原始值；上游若有渠道特定的格式化规则，应在校验完成前统一规范化。
func blank(value string) bool {
	return strings.TrimSpace(value) == ""
}

// Valid 判断渠道是否为支付领域当前允许接收的可信回执来源。
func (provider Provider) Valid() bool {
	return provider == ProviderPayCores || provider == ProviderAppStore || provider == ProviderApple || provider == ProviderGoogle
}

// SameFact 判断两份回执是否描述同一笔已经结算的业务事实。
// 渠道和外部交易号由仓储唯一索引保证；其余字段必须一致，不能静默覆盖。
func (receipt Receipt) SameFact(other Receipt) bool {
	return receipt.Provider == other.Provider &&
		receipt.ExternalTransactionID == other.ExternalTransactionID &&
		receipt.PaymentOrderID == other.PaymentOrderID &&
		receipt.UserID == other.UserID &&
		receipt.DiamondAmount == other.DiamondAmount
}
