package sessionauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/shared"
)

func TestAuthenticatorAuthenticate验证单个Go会话并仅返回用户标识(t *testing.T) {
	fixedNow := time.Date(2026, time.September, 8, 9, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	validator := &fakeSessionValidator{session: &identity.Session{UserID: "go-user-1", ContentAccess: identity.ContentAccessReviewRestricted, Role: "admin"}}
	authenticator := NewAuthenticatorWithClock(validator, func() time.Time { return fixedNow })
	request := httptest.NewRequest(http.MethodPost, "/api/chat/image/async", nil)
	request.Header.Set("Authorization", "bEaReR session-1")

	got, err := authenticator.Authenticate(request)

	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got == nil || got.UserID != "go-user-1" || got.ContentAccess != identity.ContentAccessReviewRestricted || got.Role != "admin" {
		t.Fatalf("Authenticate() identity = %#v, want user go-user-1", got)
	}
	if validator.calls != 1 || validator.gotSessionID != "session-1" {
		t.Fatalf("validator calls/session = %d/%q, want 1/session-1", validator.calls, validator.gotSessionID)
	}
	if want := fixedNow.UTC(); !validator.gotNow.Equal(want) || validator.gotNow.Location() != time.UTC {
		t.Fatalf("validator now = %s (%s), want %s (UTC)", validator.gotNow, validator.gotNow.Location(), want)
	}
}

func TestAuthenticatorAuthenticate在调用验证器前拒绝不可信身份来源(t *testing.T) {
	testCases := []struct {
		name      string
		authorize []string
		prepare   func(*http.Request)
	}{
		{name: "缺失授权头"},
		{name: "重复授权头", authorize: []string{"Bearer first", "Bearer second"}},
		{name: "非Bearer方案", authorize: []string{"Basic session-1"}},
		{name: "Bearer缺少令牌", authorize: []string{"Bearer"}},
		{name: "多余字段", authorize: []string{"Bearer session-1 extra"}},
		{name: "控制字符", authorize: []string{"Bearer session\x01"}},
		{name: "令牌过长", authorize: []string{"Bearer " + strings.Repeat("a", 129)}},
		{
			name: "不信任Cookie查询或用户头",
			prepare: func(request *http.Request) {
				request.Header.Set("Cookie", "session=node-session")
				request.Header.Set("X-User-Id", "forged-user")
				query := request.URL.Query()
				query.Set("token", "query-session")
				request.URL.RawQuery = query.Encode()
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			validator := &fakeSessionValidator{session: &identity.Session{UserID: "must-not-be-read"}}
			authenticator := NewAuthenticator(validator)
			request := httptest.NewRequest(http.MethodPost, "/api/chat/image/async", nil)
			for _, value := range testCase.authorize {
				request.Header.Add("Authorization", value)
			}
			if testCase.prepare != nil {
				testCase.prepare(request)
			}

			got, err := authenticator.Authenticate(request)

			if !errors.Is(err, shared.ErrUnauthenticated) {
				t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
			}
			if got != nil {
				t.Fatalf("Authenticate() identity = %#v, want nil", got)
			}
			if validator.calls != 0 {
				t.Fatalf("validator calls = %d, want 0", validator.calls)
			}
		})
	}
}

func TestAuthenticatorAuthenticate将失效会话统一映射为未认证(t *testing.T) {
	testCases := []struct {
		name    string
		session *identity.Session
		err     error
	}{
		{name: "领域无效会话", err: identity.ErrSessionInvalid},
		{name: "领域会话不存在", err: identity.ErrSessionNotFound},
		{name: "空会话", session: nil},
		{name: "空用户标识", session: &identity.Session{}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			validator := &fakeSessionValidator{session: testCase.session, err: testCase.err}
			authenticator := NewAuthenticator(validator)
			request := httptest.NewRequest(http.MethodGet, "/candidate", nil)
			request.Header.Set("Authorization", "Bearer session-1")

			got, err := authenticator.Authenticate(request)

			if !errors.Is(err, shared.ErrUnauthenticated) {
				t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
			}
			if got != nil {
				t.Fatalf("Authenticate() identity = %#v, want nil", got)
			}
		})
	}
}

func TestAuthenticatorAuthenticate将未知验证故障映射为服务不可用(t *testing.T) {
	validator := &fakeSessionValidator{err: errors.New("mongo credential must not leak")}
	authenticator := NewAuthenticator(validator)
	request := httptest.NewRequest(http.MethodGet, "/candidate", nil)
	request.Header.Set("Authorization", "Bearer session-1")

	got, err := authenticator.Authenticate(request)

	if !errors.Is(err, shared.ErrServiceUnavailable) {
		t.Fatalf("Authenticate() error = %v, want ErrServiceUnavailable", err)
	}
	if got != nil {
		t.Fatalf("Authenticate() identity = %#v, want nil", got)
	}
	if strings.Contains(err.Error(), "credential") {
		t.Fatalf("Authenticate() leaked dependency failure: %q", err)
	}
}

type fakeSessionValidator struct {
	gotSessionID string
	gotNow       time.Time
	calls        int
	session      *identity.Session
	err          error
}

func (fake *fakeSessionValidator) ValidateSession(_ context.Context, sessionID string, now time.Time) (*identity.Session, error) {
	fake.calls++
	fake.gotSessionID = sessionID
	fake.gotNow = now
	return fake.session, fake.err
}
