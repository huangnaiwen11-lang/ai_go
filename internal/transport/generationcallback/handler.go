// Package generationcallback 提供未注册的生成回调 HTTP 适配器。
package generationcallback

import (
	"context"
	"errors"
	"io"
	"net/http"

	bizgeneration "ai-business-service/internal/biz/generation"
	platform "ai-business-service/internal/integrations/generation"
)

const (
	maxCallbackBodyBytes = 1024 * 1024
	callbackPathV1       = "/api/v1/internal/generation-callback"
	callbackPathLegacy   = "/api/internal/generation-callback"
)

type callbackUsecase interface {
	Handle(context.Context, bizgeneration.VerifiedCallbackEvent) error
}

type handler struct {
	verifier *platform.CallbackVerifier
	usecase  callbackUsecase
}

// NewHandler 创建仅供受控装配或 httptest 调用的回调处理器。
func NewHandler(verifier *platform.CallbackVerifier, usecase callbackUsecase) http.Handler {
	return &handler{verifier: verifier, usecase: usecase}
}

// ServeHTTP 只处理验签后的 V2 终态回调，不注册任何实际路由。
func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "invalid_request")
		return
	}
	if request.URL == nil || request.URL.EscapedPath() != request.URL.Path || !isAllowedPath(request.URL.Path) {
		writeError(writer, http.StatusNotFound, "invalid_request")
		return
	}
	if request.URL.ForceQuery || request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_request")
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
	event, err := handler.verifier.Verify(request.Method, request.URL.Path, request.Header, rawBody)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid_callback")
		return
	}
	if err := handler.usecase.Handle(request.Context(), verifiedEvent(event)); err != nil {
		if errors.Is(err, bizgeneration.ErrCallbackAlreadyConfirmed) {
			writeJSON(writer, http.StatusOK, `{"ok":true}`)
			return
		}
		if errors.Is(err, bizgeneration.ErrInvalidCallbackEvent) {
			writeError(writer, http.StatusConflict, "callback_conflict")
			return
		}
		writeError(writer, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(writer, http.StatusOK, `{"ok":true}`)
}

func isAllowedPath(path string) bool {
	return path == callbackPathV1 || path == callbackPathLegacy
}

func verifiedEvent(event platform.CallbackEvent) bizgeneration.VerifiedCallbackEvent {
	terminal := bizgeneration.CallbackTerminal("")
	if event.Failed {
		terminal = bizgeneration.CallbackTerminalFailed
	}
	if event.Cancelled {
		terminal = bizgeneration.CallbackTerminalCancelled
	}
	return bizgeneration.VerifiedCallbackEvent{
		StepID: event.ExternalRef, ExternalRef: event.ExternalRef, JobID: event.JobID,
		Capability: event.Capability, MediaType: event.Result.MediaType, ResultURL: event.Result.URL,
		Terminal: terminal, NonceHash: event.NonceHash, PayloadDigest: event.PayloadDigest,
		CallbackVersion: event.CallbackVersion,
	}
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
