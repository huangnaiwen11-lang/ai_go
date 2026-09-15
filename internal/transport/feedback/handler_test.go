package feedback

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bizfeedback "ai-business-service/internal/biz/feedback"
	"ai-business-service/internal/transport/sessionauth"
)

type staticAuth struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (auth staticAuth) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return auth.identity, auth.err
}

type staticSubmitter struct{ input bizfeedback.SubmitInput }

func (submitter *staticSubmitter) Submit(_ context.Context, input bizfeedback.SubmitInput) (*bizfeedback.Submission, error) {
	submitter.input = input
	return &bizfeedback.Submission{ID: "feedback-1"}, nil
}

func TestHandlerRequiresGoSessionAndUsesAuthenticatedUser(t *testing.T) {
	submitter := &staticSubmitter{}
	handler := NewHandler(staticAuth{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1"}}, submitter)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/feedback", strings.NewReader(`{"type":"bug","message":"这是一条足够长的反馈内容"}`)))
	if recorder.Code != http.StatusCreated || submitter.input.UserID != "user-1" {
		t.Fatalf("status = %d, input = %#v", recorder.Code, submitter.input)
	}
}
