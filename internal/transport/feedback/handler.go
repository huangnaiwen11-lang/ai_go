// Package feedback 提供用户反馈写入 HTTP 入口。
package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	bizfeedback "ai-business-service/internal/biz/feedback"
	"ai-business-service/internal/transport/sessionauth"
)

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}
type submitter interface {
	Submit(context.Context, bizfeedback.SubmitInput) (*bizfeedback.Submission, error)
}
type handler struct {
	authenticator authenticator
	submitter     submitter
}

func NewHandler(authenticator authenticator, submitter submitter) http.Handler {
	return &handler{authenticator: authenticator, submitter: submitter}
}

func (handler *handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.Method != http.MethodPost || request.URL == nil || request.URL.Path != "/api/feedback" {
		writeError(writer, http.StatusNotFound, "NOT_FOUND", "Not found")
		return
	}
	if handler.authenticator == nil || handler.submitter == nil {
		writeError(writer, 503, "SERVICE_UNAVAILABLE", "Service unavailable")
		return
	}
	identity, err := handler.authenticator.Authenticate(request)
	if err != nil {
		writeError(writer, 401, "UNAUTHENTICATED", "Authentication required")
		return
	}
	var body struct {
		Type        string                   `json:"type"`
		Message     string                   `json:"message"`
		Email       string                   `json:"email"`
		Attachments []bizfeedback.Attachment `json:"attachments"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, 400, "INVALID_REQUEST", "Invalid request")
		return
	}
	submission, err := handler.submitter.Submit(request.Context(), bizfeedback.SubmitInput{UserID: identity.UserID, Type: body.Type, Message: body.Message, Email: body.Email, Attachments: body.Attachments})
	if err != nil {
		if errors.Is(err, bizfeedback.ErrInvalidInput) {
			writeError(writer, 400, "INVALID_REQUEST", "Invalid request")
		} else {
			writeError(writer, 503, "SERVICE_UNAVAILABLE", "Service unavailable")
		}
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"feedbackId": submission.ID, "message": "Feedback submitted successfully. Thank you!"})
}

func writeJSON(writer http.ResponseWriter, status int, data any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": true, "data": data})
}
func writeError(writer http.ResponseWriter, status int, code, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{"success": false, "code": code, "message": message})
}

var _ http.Handler = (*handler)(nil)
