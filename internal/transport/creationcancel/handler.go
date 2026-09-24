// Package creationcancel exposes the one user-facing cancellation request
// route. It acknowledges durable cancellation intent; only provider callback
// or lookup may later determine a terminal outcome.
package creationcancel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"ai-business-service/internal/biz/generationcancel"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

const creationCancelPrefix = "/api/creations/"

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type canceller interface {
	Cancel(context.Context, generationcancel.Command) (generationcancel.Result, error)
}

type handler struct {
	authenticator authenticator
	canceller     canceller
}

// NewHandler constructs the HTTP adapter only. Gateway owns the separate
// feature and exact-route gates, and the usecase owns the transaction clock.
func NewHandler(authenticator authenticator, canceller canceller) http.Handler {
	return &handler{authenticator: authenticator, canceller: canceller}
}

func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	identity, err := handler.authenticate(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	creationID, ok := parseCreationCancelPath(request)
	if !ok {
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	result, err := handler.canceller.Cancel(request.Context(), generationcancel.Command{
		CreationID: creationID,
		UserID:     identity.UserID,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	// This is intentionally 202 and uses "accepted", not "cancelled": the
	// durable outbox request may still race a completed/failed provider terminal.
	writeJSON(writer, http.StatusAccepted, map[string]any{"success": true, "data": map[string]any{
		"creationId": result.CreationID, "accepted": result.Accepted,
		"cancelEventCount": result.CancelEventCount, "replayed": result.Replayed,
	}})
}

func (handler *handler) authenticate(request *http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	if handler == nil || handler.authenticator == nil || handler.canceller == nil {
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

// parseCreationCancelPath permits exactly one opaque creation ID component.
// The ID shape itself belongs to the domain command; this transport guard only
// prevents alternate encodings, query variants, or accidental sub-routes from
// bypassing Gateway's exact route contract.
func parseCreationCancelPath(request *http.Request) (string, bool) {
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.EscapedPath() != request.URL.Path || !strings.HasPrefix(request.URL.Path, creationCancelPrefix) || !strings.HasSuffix(request.URL.Path, "/cancel") {
		return "", false
	}
	creationID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, creationCancelPrefix), "/cancel")
	if creationID == "" || strings.Contains(creationID, "/") {
		return "", false
	}
	return creationID, true
}

func writeError(writer http.ResponseWriter, err error) {
	var apiError *shared.APIError
	if errors.As(err, &apiError) {
		if errors.Is(err, shared.ErrUnauthenticated) {
			writeClientError(writer, http.StatusUnauthorized, "UNAUTHENTICATED", "Authentication required")
			return
		}
		writeClientError(writer, apiError.StatusCode(), apiError.Code(), apiError.Message())
		return
	}
	switch {
	case errors.Is(err, generationcancel.ErrNotFound):
		writeClientError(writer, http.StatusNotFound, "CREATION_NOT_FOUND", "Creation not found")
	case errors.Is(err, generationcancel.ErrNotReady):
		writeClientError(writer, http.StatusConflict, "CREATION_CANCEL_NOT_READY", "Creation is not ready to cancel")
	case errors.Is(err, generationcancel.ErrConflict):
		writeClientError(writer, http.StatusConflict, "CREATION_CANCEL_CONFLICT", "Creation cancellation conflicts with its current state")
	case errors.Is(err, generationcancel.ErrInvalidCommand):
		writeClientError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
	default:
		writeClientError(writer, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Service unavailable")
	}
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeClientError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"success": false, "code": code, "message": message, "details": nil})
}

var _ http.Handler = (*handler)(nil)
