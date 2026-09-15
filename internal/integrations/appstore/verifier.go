// Package appstore 定义商店回执验证的可替换边界。
// 本阶段仅实现严格的本地测试回执，不包含真实 Apple、Google URL、密钥或网络调用。
package appstore

import (
	"context"
	"errors"
	"strings"

	"ai-business-service/internal/biz/payments"
)

var (
	// ErrVerificationRejected 表示回执不满足当前验证器的可信合同。
	ErrVerificationRejected = errors.New("appstore: receipt verification rejected")
)

// VerificationRequest 是 HTTP 入口传给验证器的最小购买声明。
// 产品和商店产品标识会与已验证结果再次逐项比对，避免客户端替换商品。
type VerificationRequest struct {
	Provider       payments.StoreProvider
	Receipt        string
	ProductID      string
	StoreProductID string
}

// VerifiedPurchase 是验证器确认的不可伪造业务事实投影。
type VerifiedPurchase struct {
	Provider              payments.StoreProvider
	ExternalTransactionID string
	StoreProductID        string
}

// Verifier 隔离 Apple/Google 的验证协议。领域层永远不依赖原始回执或第三方 SDK。
type Verifier interface {
	Verify(context.Context, VerificationRequest) (VerifiedPurchase, error)
}

// LocalVerifier 只接受 `local:v1:<provider>:<transaction>:<storeProduct>` 测试回执。
// 它必须由独立显式开关装配，任何真实或未知回执都会失败关闭。
type LocalVerifier struct{}

// IssueLocalReceipt 生成本地商店回执，供离线联调使用。
// 它不是 Apple/Google 签名回执，生产代码不得把该结果视为真实验签。
func IssueLocalReceipt(provider payments.StoreProvider, transactionID, storeProductID string) (string, error) {
	if !provider.Valid() || strings.TrimSpace(transactionID) == "" || strings.Contains(transactionID, ":") || strings.TrimSpace(storeProductID) == "" || strings.Contains(storeProductID, ":") {
		return "", ErrVerificationRejected
	}
	return "local:v1:" + string(provider) + ":" + strings.TrimSpace(transactionID) + ":" + strings.TrimSpace(storeProductID), nil
}

// Verify 校验本地测试回执与 HTTP 声明的商店来源、商店商品标识完全一致。
func (LocalVerifier) Verify(_ context.Context, request VerificationRequest) (VerifiedPurchase, error) {
	if !request.Provider.Valid() || strings.TrimSpace(request.ProductID) == "" || strings.TrimSpace(request.StoreProductID) == "" {
		return VerifiedPurchase{}, ErrVerificationRejected
	}
	parts := strings.Split(request.Receipt, ":")
	if len(parts) != 5 || parts[0] != "local" || parts[1] != "v1" || payments.StoreProvider(parts[2]) != request.Provider || strings.TrimSpace(parts[3]) == "" || parts[4] != request.StoreProductID {
		return VerifiedPurchase{}, ErrVerificationRejected
	}
	return VerifiedPurchase{Provider: request.Provider, ExternalTransactionID: parts[3], StoreProductID: parts[4]}, nil
}

var _ Verifier = LocalVerifier{}
