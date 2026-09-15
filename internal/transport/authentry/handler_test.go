package authentry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/shared"
	"ai-business-service/internal/transport/sessionauth"
)

// TestRegisterReturnsCreatedEnvelopeWithoutPasswordOrHash 固定公开注册响应的安全边界。
func TestRegisterReturnsCreatedEnvelopeWithoutPasswordOrHash(t *testing.T) {
	usecase := &recordingUsecase{registerResult: testLoginResult()}
	handler := NewHandler(staticAuthenticator{}, usecase)
	request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"email":"u@example.test","password":"correct-horse","timezone":"UTC"}`))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated || usecase.registerInput.Email != "u@example.test" {
		t.Fatalf("register response = status %d, input %#v", recorder.Code, usecase.registerInput)
	}
	if body := recorder.Body.String(); strings.Contains(body, "correct-horse") || strings.Contains(body, "password_hash") || strings.Contains(body, "diamond_balance") {
		t.Fatalf("registration leaked sensitive fields: %s", body)
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Token string `json:"token"`
			User  struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || !response.Success || response.Data.Token != "session-token" || response.Data.User.ID != "user-1" {
		t.Fatalf("response = %s, decode err = %v", recorder.Body.String(), err)
	}
}

// TestAuthEntryRejectsUnknownFieldsAndInvalidMethods 防止前端绕过冻结请求合同注入字段。
func TestAuthEntryRejectsUnknownFieldsAndInvalidMethods(t *testing.T) {
	usecase := &recordingUsecase{registerResult: testLoginResult()}
	handler := NewHandler(staticAuthenticator{}, usecase)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"email":"u@example.test","password":"correct-horse","timezone":"UTC","isAdmin":true}`)))
	if recorder.Code != http.StatusBadRequest || usecase.registerCalled {
		t.Fatalf("unknown field = status %d, usecase called %t", recorder.Code, usecase.registerCalled)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/api/auth/login", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("invalid method status = %d, want 404", recorder.Code)
	}
}

// TestMeUsesOnlyBearerSessionAndReturnsSafeProjection 确保 /me 不能使用请求体或客户端声明身份。
func TestMeUsesOnlyBearerSessionAndReturnsSafeProjection(t *testing.T) {
	usecase := &recordingUsecase{currentUser: &identity.User{ID: "user-from-session", DisplayName: "用户", AccountStatus: identity.AccountStatusNormal, BindingState: identity.BindingStateBound, Timezone: "Asia/Shanghai", ContentAccess: identity.ContentAccessStandard}}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-from-session"}}, usecase)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))

	if recorder.Code != http.StatusOK || usecase.currentUserID != "user-from-session" {
		t.Fatalf("me response = status %d, userID %q", recorder.Code, usecase.currentUserID)
	}
	if body := recorder.Body.String(); strings.Contains(body, "session_version") || strings.Contains(body, "diamond") {
		t.Fatalf("me leaked internal fields: %s", body)
	}
}

// TestBindRequiresVerifiedCredentialAndKeepsOriginalUser 验证绑定必须依赖服务端 verifier，且只升级原游客。
func TestBindRequiresVerifiedCredentialAndKeepsOriginalUser(t *testing.T) {
	usecase := &recordingUsecase{currentUser: &identity.User{ID: "user-guest", BindingState: identity.BindingStateBound, AccountStatus: identity.AccountStatusNormal}}
	verifier := &recordingVerifier{result: identity.ExternalIdentity{Provider: "email", Subject: "u@example.test"}}
	handler := NewHandlerWithBinding(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-guest"}}, usecase, verifier)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/bind", strings.NewReader(`{"provider":"email","credential":"signed-proof"}`))
	request.Header.Set("Authorization", "Bearer session-token")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || usecase.bindInput.UserID != "user-guest" || usecase.bindInput.Subject != "u@example.test" {
		t.Fatalf("bind response = %d, input = %#v", recorder.Code, usecase.bindInput)
	}
	if verifier.credential != "signed-proof" {
		t.Fatalf("verifier credential = %q", verifier.credential)
	}
}

func TestBindRejectsRawSubjectAndUnknownFields(t *testing.T) {
	usecase := &recordingUsecase{}
	handler := NewHandlerWithBinding(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "user-guest"}}, usecase, &recordingVerifier{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/auth/bind", strings.NewReader(`{"provider":"email","subject":"raw@example.test"}`)))
	if recorder.Code != http.StatusBadRequest || usecase.bindCalled {
		t.Fatalf("raw subject status = %d, bind called = %t", recorder.Code, usecase.bindCalled)
	}
}

// 用户自助注销只允许删除当前会话所属用户，不能由请求体指定其他用户。
func TestDeleteAccountUsesCurrentSessionAndRevokesIt(t *testing.T) {
	usecase := &recordingUsecase{}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, usecase)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/api/auth/me", nil))
	if recorder.Code != http.StatusOK || usecase.deletedUserID != "session-user" || usecase.deletedStatus != identity.AccountStatusDeleted {
		t.Fatalf("delete status=%d user=%q target=%q", recorder.Code, usecase.deletedUserID, usecase.deletedStatus)
	}
}

// TestDeleteAccountRejectsChunkedBody 防止 HTTP/1.1 分块请求绕过 Content-Length 校验。
// 注销目标永远只能来自已认证会话，接口不接受任何客户端数据。
func TestDeleteAccountRejectsChunkedBody(t *testing.T) {
	usecase := &recordingUsecase{}
	handler := NewHandler(staticAuthenticator{identity: &sessionauth.AuthenticatedIdentity{UserID: "session-user"}}, usecase)
	request := httptest.NewRequest(http.MethodDelete, "/api/auth/me", strings.NewReader(`{"userId":"other-user"}`))
	request.ContentLength = -1
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest || usecase.deletedUserID != "" {
		t.Fatalf("delete status=%d deleted user=%q", recorder.Code, usecase.deletedUserID)
	}
}

type authUsecase interface {
	Register(context.Context, identity.RegisterInput) (*identity.LoginResult, error)
	LoginWithPassword(context.Context, identity.PasswordLoginInput) (*identity.LoginResult, error)
	LoginGuest(context.Context, identity.GuestLoginInput) (*identity.LoginResult, error)
	CurrentUser(context.Context, string) (*identity.User, error)
}

type authenticator interface {
	Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error)
}

type recordingUsecase struct {
	registerInput  identity.RegisterInput
	registerResult *identity.LoginResult
	registerCalled bool
	currentUserID  string
	currentUser    *identity.User
	bindInput      identity.BindGuestInput
	bindCalled     bool
	deletedUserID  string
	deletedStatus  identity.AccountStatus
}

func (usecase *recordingUsecase) Register(_ context.Context, input identity.RegisterInput) (*identity.LoginResult, error) {
	usecase.registerCalled = true
	usecase.registerInput = input
	return usecase.registerResult, nil
}
func (usecase *recordingUsecase) LoginWithPassword(context.Context, identity.PasswordLoginInput) (*identity.LoginResult, error) {
	return nil, shared.ErrInvalidCredentials
}
func (usecase *recordingUsecase) LoginGuest(context.Context, identity.GuestLoginInput) (*identity.LoginResult, error) {
	return nil, shared.ErrInvalidRequest
}
func (usecase *recordingUsecase) CurrentUser(_ context.Context, userID string) (*identity.User, error) {
	usecase.currentUserID = userID
	return usecase.currentUser, nil
}
func (usecase *recordingUsecase) BindGuest(_ context.Context, input identity.BindGuestInput) (*identity.User, error) {
	usecase.bindCalled = true
	usecase.bindInput = input
	return usecase.currentUser, nil
}
func (usecase *recordingUsecase) ChangeAccountStatus(_ context.Context, userID string, status identity.AccountStatus) (*identity.User, error) {
	usecase.deletedUserID = userID
	usecase.deletedStatus = status
	return &identity.User{ID: userID, AccountStatus: status}, nil
}

type recordingVerifier struct {
	result     identity.ExternalIdentity
	credential string
}

func (verifier *recordingVerifier) Verify(_ context.Context, _ string, credential string) (identity.ExternalIdentity, error) {
	verifier.credential = credential
	return verifier.result, nil
}

type staticAuthenticator struct {
	identity *sessionauth.AuthenticatedIdentity
	err      error
}

func (auth staticAuthenticator) Authenticate(*http.Request) (*sessionauth.AuthenticatedIdentity, error) {
	return auth.identity, auth.err
}

func testLoginResult() *identity.LoginResult {
	return &identity.LoginResult{User: &identity.User{ID: "user-1", DisplayName: "用户", AccountStatus: identity.AccountStatusNormal, BindingState: identity.BindingStateBound, Timezone: "UTC", ContentAccess: identity.ContentAccessStandard}, Session: &identity.Session{ID: "session-token", ExpiresAt: time.Now().Add(time.Hour)}}
}
