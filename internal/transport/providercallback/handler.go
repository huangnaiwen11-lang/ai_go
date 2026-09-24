// Package providercallback 提供 PolarStar B2B 终态回调（b2b.callback.v2）的
// HTTP 适配器。它只做「验签 → 持久化 → ACK」，不做终态 CAS。
package providercallback

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	bizgeneration "ai-business-service/internal/biz/generation"
	platform "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

const maxCallbackBodyBytes = 1 << 20

type deliveryUsecase interface {
	Handle(context.Context, bizgeneration.ProviderDeliveryFact) (bizgeneration.ProviderInboxApplyResult, error)
}

type handler struct {
	verifier *polarstarb2b.CallbackVerifier
	usecase  deliveryUsecase
}

// NewHandler 创建仅供受控装配或 httptest 调用的 B2B 回调处理器。
// 它不注册任何路由，也不启动后台循环。
func NewHandler(verifier *polarstarb2b.CallbackVerifier, usecase deliveryUsecase) http.Handler {
	return &handler{verifier: verifier, usecase: usecase}
}

// ServeHTTP 只处理验签后的 b2b.callback.v2 终态投递。
//
// 返回码直接决定平台的重投行为，因此每一个都必须是有意的：
//   - 200：投递已被持久接受（含重复投递）；
//   - 401/400：确定性拒绝，平台会提前停止投递；
//   - 409：同一 deliveryId 携带了不同字节，重投不会改变结果；
//   - 500：瞬时存储故障，平台按退避重投。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	if request.URL == nil || request.URL.EscapedPath() != request.URL.Path || request.URL.Path != platform.B2BCallbackPath {
		writeError(writer, http.StatusNotFound, "invalid_request")
		return
	}
	if request.URL.ForceQuery || request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	// The signature covers the bytes received by this handler. Accepting any
	// Content-Encoding would let a proxy or middleware transparently decode a
	// body after the 1 MiB raw-byte bound was applied, so compressed callbacks
	// are rejected before reading or parsing the body.
	if hasContentEncoding(request.Header) {
		writeError(writer, http.StatusUnsupportedMediaType, "invalid_request")
		return
	}
	if handler == nil || handler.verifier == nil || handler.usecase == nil || request.Body == nil {
		writeError(writer, http.StatusInternalServerError, "internal_error")
		return
	}
	defer request.Body.Close()
	rawBody, err := io.ReadAll(io.LimitReader(request.Body, maxCallbackBodyBytes+1))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(rawBody) > maxCallbackBodyBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "invalid_request")
		return
	}
	event, err := handler.verifier.Verify(request.Header, rawBody)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid_callback")
		return
	}
	result, err := handler.usecase.Handle(request.Context(), bizgeneration.ProviderDeliveryFact{
		AccountRef: event.AccountRef,
		DeliveryID: event.DeliveryID,
		StepID:     event.StepID,
		JobID:      event.JobID,
		Attempt:    event.Attempt,
		Payload:    rawBody,
	})
	switch {
	case err == nil:
	case errors.Is(err, bizgeneration.ErrInvalidProviderDelivery):
		writeError(writer, http.StatusBadRequest, "invalid_callback")
		return
	default:
		// 存储失败是瞬时故障：同一 deliveryId 重投会命中同一条 inbox 记录，
		// 不会产生第二份事实，因此这里让平台按自己的退避重投是安全的。
		writeError(writer, http.StatusInternalServerError, "internal_error")
		return
	}
	if result == bizgeneration.InboxApplyQuarantined {
		// 冲突证据已经落库，重投不会改变结论。用确定性 4xx 让平台提前停止，
		// 而不是让它把同一份冲突再送 7 次。
		writeError(writer, http.StatusConflict, "callback_conflict")
		return
	}
	writeJSON(writer, http.StatusOK, `{"ok":true}`)
}

func hasContentEncoding(headers http.Header) bool {
	for key := range headers {
		if strings.EqualFold(key, "Content-Encoding") {
			return true
		}
	}
	return false
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, `{"error":"`+code+`"}`)
}

func writeJSON(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

var _ http.Handler = (*handler)(nil)
