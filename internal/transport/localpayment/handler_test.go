package localpayment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/integrations/appstore"
	"ai-business-service/internal/transport/sessionauth"
)

// 购买请求中的 diamonds 必须作为未知字段被拒绝；HTTP 层只能提交商品标识，数量由领域层查询。
func TestHandler验证购买拒绝客户端钻石数量(t *testing.T) {
	checkout := &recordingCheckout{}
	handler := NewHandler(staticAuthenticator{userID: "user-1"}, checkout, &recordingVerifier{})
	request := httptest.NewRequest(http.MethodPost, verifyPurchasePath, strings.NewReader(`{"provider":"apple","receipt":"any","productId":"coins_100","storeProductId":"coins100","diamonds":999999}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", recorder.Code)
	}
	if checkout.settleCalls != 0 {
		t.Fatalf("非法客户端数量不应进入结算，用例调用 = %d", checkout.settleCalls)
	}
}

// 已验证 IAP 交易的产品与用户来自 HTTP 会话和验证器，处理器只把它们交给领域结算。
func TestHandler验证购买只交给可信验证器与领域用例(t *testing.T) {
	checkout := &recordingCheckout{result: payments.ApplyResult{Applied: true, DiamondBalance: 120}}
	verifier := &recordingVerifier{verified: appstore.VerifiedPurchase{Provider: payments.StoreProviderGoogle, ExternalTransactionID: "google-transaction-1", StoreProductID: "coins100"}}
	handler := NewHandler(staticAuthenticator{userID: "user-from-session"}, checkout, verifier)
	request := httptest.NewRequest(http.MethodPost, verifyPurchasePath, strings.NewReader(`{"provider":"google","receipt":"local:v1:google:google-transaction-1:coins100","productId":"coins_100","storeProductId":"coins100"}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d，body = %s", recorder.Code, recorder.Body.String())
	}
	if verifier.requests != 1 || checkout.settleCalls != 1 {
		t.Fatalf("验证器调用 %d 次，结算调用 %d 次，期望各一次", verifier.requests, checkout.settleCalls)
	}
	if checkout.purchase.UserID != "user-from-session" || checkout.purchase.ExternalTransactionID != "google-transaction-1" || checkout.purchase.ProductID != "coins_100" {
		t.Fatalf("领域输入 = %#v，期望只使用会话、验证器和商品标识", checkout.purchase)
	}
	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			Balance int64 `json:"balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Success || envelope.Data.Balance != 120 {
		t.Fatalf("响应 = %s，期望成功余额 120，error = %v", recorder.Body.String(), err)
	}
}

type staticAuthenticator struct{ userID string }

func (auth staticAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return &sessionauth.AuthenticatedIdentity{UserID: auth.userID}, nil
}

type recordingVerifier struct {
	verified appstore.VerifiedPurchase
	requests int
}

func (verifier *recordingVerifier) Verify(context.Context, appstore.VerificationRequest) (appstore.VerifiedPurchase, error) {
	verifier.requests++
	return verifier.verified, nil
}

type recordingCheckout struct {
	result      payments.ApplyResult
	purchase    payments.VerifiedStorePurchase
	settleCalls int
}

func (checkout *recordingCheckout) CreatePendingPayCoresOrder(context.Context, string, string) (*payments.PaymentOrder, error) {
	return &payments.PaymentOrder{ID: "order-1", DiamondAmount: 100}, nil
}

func (checkout *recordingCheckout) SettleVerifiedStorePurchase(_ context.Context, purchase payments.VerifiedStorePurchase) (payments.ApplyResult, error) {
	checkout.settleCalls++
	checkout.purchase = purchase
	return checkout.result, nil
}

var _ authenticator = staticAuthenticator{}
var _ checkoutUsecase = (*recordingCheckout)(nil)

// 编译期固定测试替身对验证器边界的实现，避免测试意外绕过接口。
var _ appstore.Verifier = (*recordingVerifier)(nil)

func TestHandler本地收银台只创建待确认订单(t *testing.T) {
	checkout := &recordingCheckout{}
	handler := NewHandler(staticAuthenticator{userID: "user-1"}, checkout, &recordingVerifier{})
	request := httptest.NewRequest(http.MethodPost, checkoutPath, strings.NewReader(`{"productId":"coins_100"}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"integrationMode":"local_only"`) {
		t.Fatalf("本地收银台响应 = %d %s，期望显式 local_only", recorder.Code, recorder.Body.String())
	}
}
