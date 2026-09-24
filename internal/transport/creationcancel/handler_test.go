package creationcancel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/generationcancel"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

func TestHandler只使用会话身份接受取消请求(t *testing.T) {
	canceller := &recordingCanceller{result: generationcancel.Result{CreationID: "creation-1", Accepted: true, CancelEventCount: 1}}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, canceller)
	request := httptest.NewRequest(http.MethodPost, "/api/creations/creation-1/cancel", strings.NewReader(`{"userId":"attacker"}`))
	request.Header.Set("X-User-Id", "attacker")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if canceller.calls != 1 || canceller.command.CreationID != "creation-1" || canceller.command.UserID != "session-user" {
		t.Fatalf("cancel command = %#v, calls=%d", canceller.command, canceller.calls)
	}
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			CreationID       string `json:"creationId"`
			Accepted         bool   `json:"accepted"`
			CancelEventCount int    `json:"cancelEventCount"`
			Replayed         bool   `json:"replayed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Success || body.Data.CreationID != "creation-1" || !body.Data.Accepted || body.Data.CancelEventCount != 1 || body.Data.Replayed {
		t.Fatalf("response = %s", recorder.Body.String())
	}
	if strings.Contains(strings.ToLower(recorder.Body.String()), "cancelled") {
		t.Fatalf("request acknowledgement must not claim a terminal cancellation: %s", recorder.Body.String())
	}
}

func TestHandler认证与领域错误映射(t *testing.T) {
	tests := []struct {
		name       string
		auth       staticAuthenticator
		cancelErr  error
		wantStatus int
		wantCode   string
		wantCalls  int
	}{
		{name: "anonymous", auth: staticAuthenticator{err: shared.ErrUnauthenticated}, wantStatus: http.StatusUnauthorized, wantCode: "UNAUTHENTICATED"},
		{name: "missing owner", auth: staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{}}, wantStatus: http.StatusUnauthorized, wantCode: "UNAUTHENTICATED"},
		{name: "not found", auth: staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1"}}, cancelErr: generationcancel.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "CREATION_NOT_FOUND", wantCalls: 1},
		{name: "not ready", auth: staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1"}}, cancelErr: generationcancel.ErrNotReady, wantStatus: http.StatusConflict, wantCode: "CREATION_CANCEL_NOT_READY", wantCalls: 1},
		{name: "state conflict", auth: staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1"}}, cancelErr: generationcancel.ErrConflict, wantStatus: http.StatusConflict, wantCode: "CREATION_CANCEL_CONFLICT", wantCalls: 1},
		{name: "dependency unavailable", auth: staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1"}}, cancelErr: generationcancel.ErrDependenciesUnavailable, wantStatus: http.StatusServiceUnavailable, wantCode: "SERVICE_UNAVAILABLE", wantCalls: 1},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			canceller := &recordingCanceller{err: testCase.cancelErr}
			handler := NewHandler(testCase.auth, canceller)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/creations/creation-1/cancel", nil))
			if recorder.Code != testCase.wantStatus || canceller.calls != testCase.wantCalls {
				t.Fatalf("status/calls = %d/%d, want %d/%d; body=%s", recorder.Code, canceller.calls, testCase.wantStatus, testCase.wantCalls, recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["code"] != testCase.wantCode {
				t.Fatalf("error body = %s / %v, want code %q", recorder.Body.String(), err, testCase.wantCode)
			}
		})
	}
}

func TestHandler拒绝非规范路径且不调用领域层(t *testing.T) {
	canceller := &recordingCanceller{}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1"}}, canceller)
	for _, target := range []string{
		"/api/creations/creation-1/cancel?ignored=true",
		"/api/creations/creation-1/cancel/extra",
		"/api/creations//cancel",
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("target=%q status=%d, body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/creations/creation-1/cancel", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("wrong method status=%d", recorder.Code)
	}
	if canceller.calls != 0 {
		t.Fatalf("invalid path called canceller %d times", canceller.calls)
	}
}

type staticAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (auth staticAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return auth.identity, auth.err
}

type recordingCanceller struct {
	command generationcancel.Command
	result  generationcancel.Result
	err     error
	calls   int
}

func (canceller *recordingCanceller) Cancel(_ context.Context, command generationcancel.Command) (generationcancel.Result, error) {
	canceller.calls++
	canceller.command = command
	if canceller.err != nil {
		return generationcancel.Result{}, canceller.err
	}
	return canceller.result, nil
}

func TestHandler领域调用时间来自服务端而不是客户端(t *testing.T) {
	canceller := &recordingCanceller{result: generationcancel.Result{CreationID: "creation-1", Accepted: true}}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-1"}}, canceller)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/creations/creation-1/cancel", nil))
	if recorder.Code != http.StatusAccepted || canceller.command.At != (time.Time{}) {
		t.Fatalf("status/command = %d/%#v; handler must not accept a client timestamp", recorder.Code, canceller.command)
	}
}

var _ interface {
	Cancel(context.Context, generationcancel.Command) (generationcancel.Result, error)
} = (*recordingCanceller)(nil)
