package appstore

import (
	"context"
	"errors"
	"testing"

	"ai-business-service/internal/biz/payments"
)

// 本地验证器只能识别明确的测试凭据，不能把任意字符串当成已验证的真实商店回执。
func TestLocalVerifier只接受受控测试回执(t *testing.T) {
	verifier := LocalVerifier{}
	request := VerificationRequest{
		Provider:       payments.StoreProviderApple,
		Receipt:        "local:v1:apple:transaction-1:com.example.coins100",
		ProductID:      "coins_100",
		StoreProductID: "com.example.coins100",
	}
	verified, err := verifier.Verify(context.Background(), request)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if verified.ExternalTransactionID != "transaction-1" || verified.Provider != payments.StoreProviderApple {
		t.Fatalf("已验证交易 = %#v，期望保留受控交易事实", verified)
	}

	request.Receipt = "real-store-receipt-must-not-pass-local-mode"
	if _, err := verifier.Verify(context.Background(), request); !errors.Is(err, ErrVerificationRejected) {
		t.Fatalf("真实或未知回执 error = %v，期望 ErrVerificationRejected", err)
	}
}

func TestIssueLocalReceiptCanBeVerified(t *testing.T) {
	receipt, err := IssueLocalReceipt(payments.StoreProviderGoogle, "transaction-2", "com.example.coins500")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := (LocalVerifier{}).Verify(context.Background(), VerificationRequest{Provider: payments.StoreProviderGoogle, Receipt: receipt, ProductID: "coins_500", StoreProductID: "com.example.coins500"})
	if err != nil || verified.ExternalTransactionID != "transaction-2" {
		t.Fatalf("receipt verify = %#v, %v", verified, err)
	}
}
