package adminauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

func denialCode(t *testing.T, err error) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	WriteDenial(recorder, err)
	var body struct {
		Code string `json:"code"`
	}
	if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &body); decodeErr != nil {
		t.Fatalf("解析响应失败: %v", decodeErr)
	}
	return recorder.Code, body.Code
}

func TestWriteDenial区分未认证与无权限(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		// 前端只在 401 时清理令牌并跳转登录，因此匿名必须与无权限区分开。
		{"未认证", shared.ErrUnauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED"},
		{"会话服务不可用", shared.ErrServiceUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE"},
		{"已认证但非管理员", ErrForbidden, http.StatusForbidden, "FORBIDDEN"},
		{"未知错误按无权限处理", errors.New("boom"), http.StatusForbidden, "FORBIDDEN"},
	}
	for _, testCase := range cases {
		code, body := denialCode(t, testCase.err)
		if code != testCase.wantCode || body != testCase.wantBody {
			t.Errorf("%s: 得到 %d/%s，期望 %d/%s", testCase.name, code, body, testCase.wantCode, testCase.wantBody)
		}
	}
}

func TestAuthorizer缺少认证器时不可用(t *testing.T) {
	if err := Authorizer(nil)(httptest.NewRequest(http.MethodGet, "/api/admin/apps", nil)); err == nil {
		t.Fatal("缺少认证器时必须返回错误")
	}
}

func TestAuthorizerWithActor只返回会话身份并复用管理员门禁(t *testing.T) {
	validator := &authorizerSessionValidator{session: &identity.Session{UserID: "stored-admin", Role: "admin"}}
	authenticator := sessionauth.NewAuthenticatorWithClock(validator, time.Now)
	request := httptest.NewRequest(http.MethodPost, "/api/admin/video-review/video-one/review", nil)
	request.Header.Set("Authorization", "Bearer server-session")
	request.Header.Set("X-User-Id", "forged-admin")

	actorID, err := AuthorizerWithActor(authenticator)(request)
	if err != nil {
		t.Fatalf("AuthorizerWithActor() error = %v", err)
	}
	if actorID != "stored-admin" {
		t.Fatalf("actorID = %q, want stored-admin", actorID)
	}
	if validator.sessionID != "server-session" || validator.calls != 1 {
		t.Fatalf("validator session/calls = %q/%d, want server-session/1", validator.sessionID, validator.calls)
	}
}

func TestAuthorizerWithActor拒绝非管理员(t *testing.T) {
	validator := &authorizerSessionValidator{session: &identity.Session{UserID: "regular-user", Role: "user"}}
	authenticator := sessionauth.NewAuthenticatorWithClock(validator, time.Now)
	request := httptest.NewRequest(http.MethodGet, "/api/admin/video-review", nil)
	request.Header.Set("Authorization", "Bearer server-session")

	actorID, err := AuthorizerWithActor(authenticator)(request)
	if !errors.Is(err, ErrForbidden) || actorID != "" {
		t.Fatalf("actorID/error = %q/%v, want empty/ErrForbidden", actorID, err)
	}
}

type authorizerSessionValidator struct {
	calls     int
	sessionID string
	session   *identity.Session
	err       error
}

func (validator *authorizerSessionValidator) ValidateSession(_ context.Context, sessionID string, _ time.Time) (*identity.Session, error) {
	validator.calls++
	validator.sessionID = sessionID
	return validator.session, validator.err
}
