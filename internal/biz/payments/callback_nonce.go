package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	// ErrPaymentCallbackReplayed 表示同一份已验证的支付回调 nonce 已被消费。
	ErrPaymentCallbackReplayed = errors.New("payments: payment callback nonce replayed")
	// ErrInvalidPaymentCallbackNonce 表示 nonce 缺失或摘要值对象无效。
	ErrInvalidPaymentCallbackNonce = errors.New("payments: invalid payment callback nonce")
)

// PaymentCallbackNonceHash 是支付回调 nonce 的受控 SHA-256 摘要值对象。
// 原 nonce 只在构造时参与摘要，之后不能穿过支付领域接口。
type PaymentCallbackNonceHash struct {
	hex string
}

// NewPaymentCallbackNonceHash 将已验签 nonce 固定投影为 SHA-256 十六进制摘要。
func NewPaymentCallbackNonceHash(nonce string) (PaymentCallbackNonceHash, error) {
	if nonce == "" || strings.TrimSpace(nonce) != nonce {
		return PaymentCallbackNonceHash{}, ErrInvalidPaymentCallbackNonce
	}
	sum := sha256.Sum256([]byte(nonce))
	return PaymentCallbackNonceHash{hex: hex.EncodeToString(sum[:])}, nil
}

// Hex 返回可持久化的 SHA-256 摘要，不暴露原 nonce。
func (nonceHash PaymentCallbackNonceHash) Hex() string {
	return nonceHash.hex
}

// Valid 判断摘要是否为构造函数产生的规范 SHA-256 十六进制值。
func (nonceHash PaymentCallbackNonceHash) Valid() bool {
	if len(nonceHash.hex) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(nonceHash.hex)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == nonceHash.hex
}

// PaymentCallbackNonceStore 定义已验证支付回调的最小防重放持久化边界。
// 调用方只能提交 nonce 摘要与已认证的 HTTP 边界事实，不能传递原 nonce、签名、报文或金额。
type PaymentCallbackNonceStore interface {
	Consume(ctx context.Context, nonceHash PaymentCallbackNonceHash, method, path string, expiresAt time.Time) error
}
