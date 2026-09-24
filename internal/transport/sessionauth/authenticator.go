// Package sessionauth 提供 Go 自有不透明会话的最小 HTTP 身份解析能力。
// 它不签发令牌、不兼容 Node 登录态，也不注册任何公开路由。
package sessionauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/biz/shared"
)

const maxSessionIDLength = 128

// sessionValidator 是会话认证器需要的最小领域边界。
// 具体实现由 identity.Usecase 提供，传输层不读取 MongoDB 或自行判断账号状态。
type sessionValidator interface {
	ValidateSession(context.Context, string, time.Time) (*identity.Session, error)
}

// AuthenticatedIdentity 是已经过 Go 自有会话验证的最小身份事实。
// 它故意只保存用户标识，不携带令牌、会话标识或账户状态等敏感信息。
type AuthenticatedIdentity struct {
	UserID        string
	ContentAccess string
	Role          string
}

// Authenticator 仅从 Authorization Bearer Header 中提取并验证 Go 会话 ID。
type Authenticator struct {
	validator sessionValidator
	now       func() time.Time
}

// NewAuthenticator 使用服务端当前时间构造会话认证器。
func NewAuthenticator(validator sessionValidator) *Authenticator {
	return NewAuthenticatorWithClock(validator, time.Now)
}

// NewAuthenticatorWithClock 允许本地测试固定服务端时钟。
// 传入 nil 时钟会在认证时安全失败，生产装配必须使用 NewAuthenticator。
func NewAuthenticatorWithClock(validator sessionValidator, now func() time.Time) *Authenticator {
	return &Authenticator{validator: validator, now: now}
}

// Authenticate 验证单个 Go 自有 Bearer 会话，并返回仅含 UserID 的身份事实。
// Cookie、query、X-User-Id 和其他客户端输入不参与身份判定。
func (authenticator *Authenticator) Authenticate(request *http.Request) (*AuthenticatedIdentity, error) {
	if request == nil {
		return nil, shared.ErrUnauthenticated
	}
	if authenticator == nil || authenticator.validator == nil || authenticator.now == nil {
		return nil, shared.ErrServiceUnavailable
	}

	sessionID, err := bearerSessionID(request.Header.Values("Authorization"))
	if err != nil {
		return nil, shared.ErrUnauthenticated
	}

	session, err := authenticator.validator.ValidateSession(request.Context(), sessionID, authenticator.now().UTC())
	if errors.Is(err, identity.ErrSessionInvalid) || errors.Is(err, identity.ErrSessionNotFound) {
		return nil, shared.ErrUnauthenticated
	}
	if err != nil {
		return nil, shared.ErrServiceUnavailable
	}
	if session == nil || strings.TrimSpace(session.UserID) == "" {
		return nil, shared.ErrUnauthenticated
	}
	return &AuthenticatedIdentity{UserID: session.UserID, ContentAccess: session.ContentAccess, Role: session.Role}, nil
}

// bearerSessionID 只接受一个、格式严格的 Bearer 值，避免重复 Header 或多种来源
// 被错误合并为同一身份。
func bearerSessionID(values []string) (string, error) {
	if len(values) != 1 {
		return "", shared.ErrUnauthenticated
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", shared.ErrUnauthenticated
	}
	sessionID := parts[1]
	if len(sessionID) == 0 || len(sessionID) > maxSessionIDLength {
		return "", shared.ErrUnauthenticated
	}
	for _, character := range sessionID {
		if unicode.IsControl(character) {
			return "", shared.ErrUnauthenticated
		}
	}
	return sessionID, nil
}
