// Package localpayment 提供由 Gateway 显式开关接管的本地支付入口。
package localpayment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/integrations/appstore"
	"ai-business-service/internal/transport/sessionauth"
)

const (
	checkoutPath       = "/api/wallet/create-external-checkout"
	verifyPurchasePath = "/api/wallet/verify-purchase"
	maxRequestBytes    = 64 << 10
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type checkoutUsecase interface {
	CreatePendingPayCoresOrder(context.Context, string, string) (*payments.PaymentOrder, error)
	SettleVerifiedStorePurchase(context.Context, payments.VerifiedStorePurchase) (payments.ApplyResult, error)
}

// paycoresCheckoutUsecase 是显式启用本地 PayCores mock 时的真实建单边界。
// 与 checkoutUsecase 分离，保证默认本地 IAP fixture 不会意外向外发起收银台请求。
type paycoresCheckoutUsecase interface {
	CreatePayCoresCheckout(context.Context, string, string) (payments.PayCoresCheckout, error)
}

type handler struct {
	authenticator authenticator
	checkout      checkoutUsecase
	verifier      appstore.Verifier
	paycores      paycoresCheckoutUsecase
	queries       queryUsecase
}

// NewHandler 创建本地支付入口处理器。路由接管权仍由 Gateway 的精确开关控制。
func NewHandler(authenticator authenticator, checkout checkoutUsecase, verifier appstore.Verifier, paycores ...paycoresCheckoutUsecase) http.Handler {
	instance := &handler{authenticator: authenticator, checkout: checkout, verifier: verifier}
	if len(paycores) == 1 {
		instance.paycores = paycores[0]
	}
	return instance
}

// ServeHTTP 分发受控支付读写 URL。接管后错误不能回退 Node，避免混用订单归属或重复入账。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler.serveQuery(writer, request) {
		return
	}
	// 查询依赖可独立装配，但不能因此放行缺少建单用例的旧 POST 路径。
	if handler == nil || handler.checkout == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if request == nil || request.URL == nil {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == checkoutPath:
		handler.createCheckout(writer, request, identity.UserID)
	case request.Method == http.MethodPost && request.URL.Path == verifyPurchasePath:
		handler.verifyPurchase(writer, request, identity.UserID)
	default:
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	}
}

func (handler *handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler == nil || handler.authenticator == nil {
		return nil, shared.ErrServiceUnavailable
	}
	identity, err := handler.authenticator.Authenticate(request)
	if err != nil {
		return nil, err
	}
	if identity == nil || strings.TrimSpace(identity.UserID) == "" {
		return nil, shared.ErrUnauthenticated
	}
	return identity, nil
}

func (handler *handler) createCheckout(writer http.ResponseWriter, request *http.Request, userID string) {
	var body struct {
		ProductID string `json:"productId"`
	}
	if err := decodeStrictJSON(request, &body); err != nil || strings.TrimSpace(body.ProductID) == "" {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	if handler.paycores != nil {
		checkout, err := handler.paycores.CreatePayCoresCheckout(request.Context(), userID, body.ProductID)
		if err != nil {
			writePaymentError(writer, err)
			return
		}
		writeSuccess(writer, http.StatusOK, map[string]any{"orderId": checkout.Order.ID, "checkoutUrl": checkout.CheckoutURL, "credits": checkout.Order.DiamondAmount, "integrationMode": "paycores"})
		return
	}
	order, err := handler.checkout.CreatePendingPayCoresOrder(request.Context(), userID, body.ProductID)
	if err != nil {
		writePaymentError(writer, err)
		return
	}
	// 本地阶段没有真实收银台 URL。明确标记 local_only，避免前端或后续集成误认为已向 PayCores 发起扣款。
	writeSuccess(writer, http.StatusOK, map[string]any{"orderId": order.ID, "checkoutUrl": "", "credits": order.DiamondAmount, "integrationMode": "local_only"})
}

func (handler *handler) verifyPurchase(writer http.ResponseWriter, request *http.Request, userID string) {
	if handler.verifier == nil {
		writeError(writer, shared.ErrServiceUnavailable)
		return
	}
	var body struct {
		Provider       payments.StoreProvider `json:"provider"`
		Receipt        string                 `json:"receipt"`
		ProductID      string                 `json:"productId"`
		StoreProductID string                 `json:"storeProductId"`
	}
	if err := decodeStrictJSON(request, &body); err != nil || !body.Provider.Valid() || strings.TrimSpace(body.Receipt) == "" || strings.TrimSpace(body.ProductID) == "" {
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
		return
	}
	if strings.TrimSpace(body.StoreProductID) == "" {
		body.StoreProductID = body.ProductID
	}
	verified, err := handler.verifier.Verify(request.Context(), appstore.VerificationRequest{Provider: body.Provider, Receipt: body.Receipt, ProductID: body.ProductID, StoreProductID: body.StoreProductID})
	if err != nil || verified.Provider != body.Provider || verified.StoreProductID != body.StoreProductID || strings.TrimSpace(verified.ExternalTransactionID) == "" {
		writeClientError(writer, http.StatusBadRequest, "INVALID_PURCHASE_RECEIPT", "Receipt verification failed")
		return
	}
	result, err := handler.checkout.SettleVerifiedStorePurchase(request.Context(), payments.VerifiedStorePurchase{Provider: verified.Provider, ExternalTransactionID: verified.ExternalTransactionID, StoreProductID: verified.StoreProductID, ProductID: body.ProductID, UserID: userID})
	if err != nil {
		writePaymentError(writer, err)
		return
	}
	writeSuccess(writer, http.StatusOK, map[string]any{"balance": result.DiamondBalance, "coins": 0, "duplicate": !result.Applied})
}

func decodeStrictJSON(request *http.Request, target any) error {
	if request == nil || request.Body == nil {
		return errors.New("request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

func writePaymentError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, payments.ErrProductNotPublished), errors.Is(err, payments.ErrInvalidPaymentOrder), errors.Is(err, payments.ErrInvalidStorePurchase):
		writeClientError(writer, http.StatusBadRequest, "INVALID_REQUEST", "Invalid request")
	case errors.Is(err, payments.ErrPaymentOrderMismatch), errors.Is(err, payments.ErrReceiptConflict):
		writeClientError(writer, http.StatusConflict, "PAYMENT_CONFLICT", "Payment transaction conflict")
	default:
		writeError(writer, shared.ErrServiceUnavailable)
	}
}

func writeError(writer http.ResponseWriter, err error) {
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		writeClientError(writer, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	writeClientError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
}

func writeSuccess(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
}

func writeClientError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*handler)(nil)
