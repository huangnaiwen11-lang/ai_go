package identity

import (
	"context"
	"errors"
	"time"

	"ai-business-service/internal/biz/authcredential"
)

var (
	// ErrUserNotFound 表示指定用户不存在。
	ErrUserNotFound = errors.New("identity: user not found")
	// ErrIdentityNotFound 表示指定外部身份不存在。
	ErrIdentityNotFound = errors.New("identity: external identity not found")
	// ErrSessionNotFound 表示指定会话不存在。
	ErrSessionNotFound = errors.New("identity: session not found")
	// ErrGuestBindingNotAllowed 表示用户不是可绑定的正常游客。
	ErrGuestBindingNotAllowed = errors.New("identity: guest binding is not allowed")
	// ErrIdentityAlreadyBound 表示外部身份已经归属某个用户。
	ErrIdentityAlreadyBound = errors.New("identity: external identity already bound")
	// ErrInvalidBindingInput 表示游客绑定缺少用户、渠道或外部主体。
	ErrInvalidBindingInput = errors.New("identity: invalid guest binding input")
	// ErrInvalidAccountStatus 表示不允许把账户改为指定状态。
	ErrInvalidAccountStatus = errors.New("identity: invalid target account status")
	// ErrSessionInvalid 表示会话已撤销、过期、版本不匹配或用户不可用。
	ErrSessionInvalid = errors.New("identity: session is invalid")
	// ErrEmailAlreadyRegistered 表示正常或封禁账号已占用该邮箱。
	ErrEmailAlreadyRegistered = errors.New("identity: email already registered")
	// ErrInvalidCredentials 同时表示未知邮箱和错误密码，避免账号枚举。
	ErrInvalidCredentials = errors.New("identity: invalid credentials")
	// ErrWebGuestLoginDisabled 表示 Web 端不能创建游客。
	ErrWebGuestLoginDisabled = errors.New("identity: web guest login is disabled")
	// ErrInvalidAuthEntryInput 表示注册、登录或游客入口参数无效。
	ErrInvalidAuthEntryInput = errors.New("identity: invalid auth entry input")
	// ErrAuthDependenciesUnavailable 表示仅装配了历史身份能力，不能安全执行账号入口。
	ErrAuthDependenciesUnavailable = errors.New("identity: auth entry dependencies are unavailable")
)

// UserRepository 是用户状态的反转依赖边界。
// 条件更新由仓储实现，业务层不接触数据库查询语法。
type UserRepository interface {
	Find(context.Context, string) (*User, error)
	MarkBound(context.Context, string, time.Time) (*User, error)
	ChangeAccountStatus(context.Context, string, AccountStatus, time.Time) (*User, error)
}

// AuthUserRepository 扩展历史用户仓储，为账号入口提供创建和设备游客查找能力。
type AuthUserRepository interface {
	UserRepository
	Create(context.Context, User) error
	FindGuestByDevice(context.Context, string, string) (*User, error)
}

// IdentityRepository 是外部身份全局唯一性的反转依赖边界。
type IdentityRepository interface {
	FindByProviderSubject(context.Context, string, string) (*ExternalIdentity, error)
	Create(context.Context, ExternalIdentity) error
}

// SessionRepository 是会话读取与批量撤销的反转依赖边界。
type SessionRepository interface {
	Find(context.Context, string) (*Session, error)
	RevokeActiveByUser(context.Context, string, time.Time) error
}

// AuthSessionRepository 扩展历史会话仓储，为注册、登录和游客登录签发会话。
type AuthSessionRepository interface {
	SessionRepository
	Create(context.Context, Session) error
}

// AccountRepository 仅负责为新用户原子创建零余额自有账户。
type AccountRepository interface {
	CreateZero(context.Context, string, time.Time) error
}

// CredentialRepository 是 authcredential.Repository 的身份模块别名，保持本模块依赖
// 指向明确的领域边界而不是具体 MongoDB 实现。
type CredentialRepository = authcredential.Repository
