package payments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestNewPaymentCallbackNonceHash只暴露原Nonce的SHA256摘要(t *testing.T) {
	const rawNonce = "12345678-1234-4123-8123-123456789abc"

	nonceHash, err := NewPaymentCallbackNonceHash(rawNonce)
	if err != nil {
		t.Fatalf("NewPaymentCallbackNonceHash() error = %v", err)
	}
	wantSum := sha256.Sum256([]byte(rawNonce))
	want := hex.EncodeToString(wantSum[:])
	if got := nonceHash.Hex(); got != want {
		t.Fatalf("nonceHash.Hex() = %q, want SHA-256 %q", got, want)
	}
	if nonceHash.Hex() == rawNonce {
		t.Fatal("nonceHash.Hex() 意外保留原 nonce")
	}
}

func TestNewPaymentCallbackNonceHash拒绝空Nonce且零值无效(t *testing.T) {
	if _, err := NewPaymentCallbackNonceHash(" "); !errors.Is(err, ErrInvalidPaymentCallbackNonce) {
		t.Fatalf("空 nonce error = %v，期望 ErrInvalidPaymentCallbackNonce", err)
	}
	var zero PaymentCallbackNonceHash
	if zero.Hex() != "" {
		t.Fatalf("零值摘要 = %q，期望空值", zero.Hex())
	}
}

func TestPaymentCallbackNonceStore只接收受控摘要类型(t *testing.T) {
	storeType := reflect.TypeFor[PaymentCallbackNonceStore]()
	consume, ok := storeType.MethodByName("Consume")
	if !ok {
		t.Fatal("PaymentCallbackNonceStore 缺少 Consume")
	}
	if got, want := consume.Type.In(1), reflect.TypeFor[PaymentCallbackNonceHash](); got != want {
		t.Fatalf("Consume() nonce 参数类型 = %v，期望 %v；仓储接口不得接收裸 string", got, want)
	}

	var store PaymentCallbackNonceStore
	consumeWithHash(store, PaymentCallbackNonceHash{})
}

func consumeWithHash(store PaymentCallbackNonceStore, nonceHash PaymentCallbackNonceHash) {
	if store != nil {
		_ = store.Consume(context.Background(), nonceHash, "POST", "/api/internal/payment-confirmed", time.Now())
	}
}
