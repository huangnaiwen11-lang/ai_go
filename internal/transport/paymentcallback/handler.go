// Package paymentcallback 提供未注册的 PayCores 支付确认回调 HTTP 适配器。
package paymentcallback

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"ai-business-service/internal/biz/payments"
	paycores "ai-business-service/internal/integrations/paycores"
)

const (
	maxCallbackBodyBytes = 1024 * 1024
	callbackPathLegacy   = "/api/internal/payment-confirmed"
	callbackPathV1       = "/api/v1/internal/payment-confirmed"
)

type confirmedCallbackUsecase interface {
	Handle(context.Context, payments.VerifiedPaymentConfirmation) (payments.ApplyResult, error)
}

type handler struct {
	verifier *paycores.CallbackVerifier
	usecase  confirmedCallbackUsecase
}

// NewHandler 创建仅供受控装配或 httptest 调用的支付确认回调处理器。
func NewHandler(verifier *paycores.CallbackVerifier, usecase confirmedCallbackUsecase) http.Handler {
	return &handler{verifier: verifier, usecase: usecase}
}

// ServeHTTP 只处理验签后的 PayCores 支付确认回调，不注册任何实际路由。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeFailure(writer, http.StatusMethodNotAllowed, "Invalid signature")
		return
	}
	if request.URL == nil || request.URL.EscapedPath() != request.URL.Path || !isAllowedPath(request.URL.Path) {
		writeFailure(writer, http.StatusNotFound, "Invalid signature")
		return
	}
	if request.URL.ForceQuery || request.URL.RawQuery != "" {
		writeFailure(writer, http.StatusBadRequest, "Invalid signature")
		return
	}
	if handler == nil || handler.verifier == nil || handler.usecase == nil || request.Body == nil {
		writeFailure(writer, http.StatusInternalServerError, "Failed to process payment")
		return
	}
	defer request.Body.Close()

	rawBody, err := io.ReadAll(io.LimitReader(request.Body, maxCallbackBodyBytes+1))
	if err != nil {
		writeFailure(writer, http.StatusBadRequest, "Invalid signature")
		return
	}
	if len(rawBody) > maxCallbackBodyBytes {
		writeFailure(writer, http.StatusRequestEntityTooLarge, "Invalid signature")
		return
	}

	verified, err := handler.verifier.Verify(request.Method, request.URL.Path, request.Header, rawBody)
	if err != nil {
		writeFailure(writer, http.StatusUnauthorized, "Invalid signature")
		return
	}
	nonce, err := handler.verifier.VerifiedNonce(request.Header)
	if err != nil {
		writeFailure(writer, http.StatusUnauthorized, "Invalid signature")
		return
	}
	nonceHash, err := payments.NewPaymentCallbackNonceHash(nonce)
	if err != nil {
		writeFailure(writer, http.StatusUnauthorized, "Invalid signature")
		return
	}
	confirmation, err := payments.NewVerifiedPaymentConfirmation(
		verified.UserID, verified.OrderID, verified.ProviderTxnID, nonceHash,
	)
	if err != nil {
		writeFailure(writer, http.StatusInternalServerError, "Failed to process payment")
		return
	}

	result, err := handler.usecase.Handle(request.Context(), confirmation)
	if err != nil {
		switch {
		case errors.Is(err, payments.ErrPaymentCallbackReplayed):
			writeFailure(writer, http.StatusConflict, "Request replayed")
		case errors.Is(err, payments.ErrConfirmedCallbackDependenciesUnavailable):
			writeFailure(writer, http.StatusServiceUnavailable, "Authentication unavailable")
		default:
			writeFailure(writer, http.StatusInternalServerError, "Failed to process payment")
		}
		return
	}

	writeJSON(writer, http.StatusOK, `{"success":true,"data":{"newBalance":`+strconv.FormatInt(result.DiamondBalance, 10)+`,"duplicate":`+strconv.FormatBool(!result.Applied)+`}}`)
}

func isAllowedPath(path string) bool {
	return path == callbackPathLegacy || path == callbackPathV1
}

func writeFailure(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, `{"success":false,"error":`+strconv.Quote(message)+`}`)
}

func writeJSON(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

var _ http.Handler = (*handler)(nil)
