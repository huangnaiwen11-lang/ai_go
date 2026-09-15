package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"
	"time"

	"ai-business-service/internal/biz/authcredential"
	"ai-business-service/internal/biz/shared"

	"github.com/google/uuid"
)

// Usecase 协调身份模块的领域规则。
// 它不依赖 HTTP、JWT、MongoDB 或其他业务模块。
type Usecase struct {
	users             UserRepository
	identities        IdentityRepository
	sessions          SessionRepository
	tx                shared.TxRunner
	clock             func() time.Time
	authUsers         AuthUserRepository
	authSessions      AuthSessionRepository
	accounts          AccountRepository
	credentials       CredentialRepository
	passwords         authcredential.PasswordPolicy
	dummyPasswordHash string
}

// NewAuthUsecase 在既有身份能力之上装配本地账号入口。
// 该构造器不替换 NewUsecase，避免未接管账号路由的现有调用方意外取得密码登录能力。
func NewAuthUsecase(users AuthUserRepository, identities IdentityRepository, sessions AuthSessionRepository, accounts AccountRepository, credentials CredentialRepository, passwords authcredential.PasswordPolicy, tx shared.TxRunner) *Usecase {
	usecase := NewUsecase(users, identities, sessions, tx)
	usecase.authUsers = users
	usecase.authSessions = sessions
	usecase.accounts = accounts
	usecase.credentials = credentials
	usecase.passwords = passwords
	if passwords != nil {
		// 未知邮箱也运行一次 Argon2id，缩小其与错误密码的可观测时间差。
		usecase.dummyPasswordHash, _ = passwords.Hash("not-a-real-login-password")
	}
	return usecase
}

// Register 创建同一个 Go 身份域中的已绑定用户、密码凭据、零余额账户和 30 天会话。
func (usecase *Usecase) Register(ctx context.Context, input RegisterInput) (*LoginResult, error) {
	email, timezone, err := normalizeRegisterInput(input)
	if err != nil {
		return nil, err
	}
	if err := usecase.requireAuthDependencies(); err != nil {
		return nil, err
	}
	passwordHash, err := usecase.passwords.Hash(input.Password)
	if errors.Is(err, authcredential.ErrWeakPassword) {
		return nil, ErrInvalidAuthEntryInput
	}
	if err != nil {
		return nil, err
	}

	now := usecase.clock()
	user := User{ID: newOpaqueID(), DisplayName: strings.TrimSpace(input.DisplayName), AccountStatus: AccountStatusNormal, BindingState: BindingStateBound, Timezone: timezone, SessionVersion: 1, ContentAccess: ContentAccessStandard, CreatedAt: now, UpdatedAt: now}
	session := Session{ID: newOpaqueID(), UserID: user.ID, SessionVersion: user.SessionVersion, ExpiresAt: now.Add(30 * 24 * time.Hour)}
	credential := authcredential.Credential{ID: newOpaqueID(), UserID: user.ID, EmailNormalized: email, PasswordHash: passwordHash, Active: true, CreatedAt: now, UpdatedAt: now}
	if err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		existing, findErr := usecase.credentials.FindActiveByEmail(txCtx, email)
		switch {
		case findErr == nil:
			existingUser, userErr := usecase.users.Find(txCtx, existing.UserID)
			if userErr != nil || existingUser.AccountStatus != AccountStatusDeleted {
				return ErrEmailAlreadyRegistered
			}
			if err := usecase.credentials.DeactivateByUser(txCtx, existing.UserID, now); err != nil {
				return err
			}
		case !errors.Is(findErr, authcredential.ErrCredentialNotFound):
			return findErr
		}
		if err := usecase.authUsers.Create(txCtx, user); err != nil {
			return err
		}
		if err := usecase.credentials.Create(txCtx, credential); err != nil {
			if errors.Is(err, authcredential.ErrEmailAlreadyRegistered) {
				return ErrEmailAlreadyRegistered
			}
			return err
		}
		if err := usecase.accounts.CreateZero(txCtx, user.ID, now); err != nil {
			return err
		}
		return usecase.authSessions.Create(txCtx, session)
	}); err != nil {
		return nil, err
	}
	return &LoginResult{User: &user, Session: &session, AccountBalance: 0}, nil
}

// LoginWithPassword 校验凭据后签发新会话；未知邮箱和错误密码始终返回同一个错误。
func (usecase *Usecase) LoginWithPassword(ctx context.Context, input PasswordLoginInput) (*LoginResult, error) {
	email, password, err := normalizeLoginInput(input)
	if err != nil {
		return nil, err
	}
	if err := usecase.requireAuthDependencies(); err != nil {
		return nil, err
	}
	credential, err := usecase.credentials.FindActiveByEmail(ctx, email)
	if errors.Is(err, authcredential.ErrCredentialNotFound) {
		_, _, _ = usecase.passwords.Verify(usecase.dummyPasswordHash, password)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	matched, needsRehash, err := usecase.passwords.Verify(credential.PasswordHash, password)
	if err != nil || !matched {
		return nil, ErrInvalidCredentials
	}
	user, err := usecase.users.Find(ctx, credential.UserID)
	if err != nil || user.AccountStatus != AccountStatusNormal {
		return nil, ErrInvalidCredentials
	}
	now := usecase.clock()
	if needsRehash {
		newHash, hashErr := usecase.passwords.Hash(password)
		if hashErr != nil {
			return nil, hashErr
		}
		if err := usecase.credentials.UpdatePasswordHash(ctx, credential.UserID, newHash, now); err != nil {
			return nil, err
		}
	}
	session := Session{ID: newOpaqueID(), UserID: user.ID, SessionVersion: user.SessionVersion, ExpiresAt: now.Add(30 * 24 * time.Hour)}
	if err := usecase.authSessions.Create(ctx, session); err != nil {
		return nil, err
	}
	return &LoginResult{User: user, Session: &session}, nil
}

// LoginGuest 仅为 iOS、Android 创建或复用设备游客；第一次写入的时区永不覆盖。
func (usecase *Usecase) LoginGuest(ctx context.Context, input GuestLoginInput) (*LoginResult, error) {
	platform := strings.ToLower(strings.TrimSpace(input.Platform))
	if platform == "web" {
		return nil, ErrWebGuestLoginDisabled
	}
	if (platform != "ios" && platform != "android") || len(strings.TrimSpace(input.DeviceID)) < 8 || !validTimezone(input.Timezone) {
		return nil, ErrInvalidAuthEntryInput
	}
	if err := usecase.requireAuthDependencies(); err != nil {
		return nil, err
	}
	now := usecase.clock()
	var result *LoginResult
	err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		user, findErr := usecase.authUsers.FindGuestByDevice(txCtx, platform, strings.TrimSpace(input.DeviceID))
		if errors.Is(findErr, ErrUserNotFound) {
			user = &User{ID: newOpaqueID(), AccountStatus: AccountStatusNormal, BindingState: BindingStateGuest, Timezone: strings.TrimSpace(input.Timezone), GuestPlatform: platform, GuestDeviceID: strings.TrimSpace(input.DeviceID), SessionVersion: 1, ContentAccess: ContentAccessStandard, CreatedAt: now, UpdatedAt: now}
			if err := usecase.authUsers.Create(txCtx, *user); err != nil {
				return err
			}
			if err := usecase.accounts.CreateZero(txCtx, user.ID, now); err != nil {
				return err
			}
		} else if findErr != nil {
			return findErr
		} else if user.AccountStatus != AccountStatusNormal {
			return ErrInvalidCredentials
		}
		session := Session{ID: newOpaqueID(), UserID: user.ID, SessionVersion: user.SessionVersion, ExpiresAt: now.Add(30 * 24 * time.Hour)}
		if err := usecase.authSessions.Create(txCtx, session); err != nil {
			return err
		}
		result = &LoginResult{User: user, Session: &session}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CurrentUser 返回已完成会话认证后的安全用户投影来源。
func (usecase *Usecase) CurrentUser(ctx context.Context, userID string) (*User, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrUserNotFound
	}
	return usecase.users.Find(ctx, userID)
}

func (usecase *Usecase) requireAuthDependencies() error {
	if usecase == nil || usecase.authUsers == nil || usecase.authSessions == nil || usecase.accounts == nil || usecase.credentials == nil || usecase.passwords == nil || usecase.tx == nil {
		return ErrAuthDependenciesUnavailable
	}
	return nil
}

func normalizeRegisterInput(input RegisterInput) (string, string, error) {
	email, err := normalizeEmail(input.Email)
	if err != nil || !validTimezone(input.Timezone) {
		return "", "", ErrInvalidAuthEntryInput
	}
	return email, strings.TrimSpace(input.Timezone), nil
}

func normalizeLoginInput(input PasswordLoginInput) (string, string, error) {
	email, err := normalizeEmail(input.Email)
	if err != nil || input.Password == "" {
		return "", "", ErrInvalidCredentials
	}
	return email, input.Password, nil
}

func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || !strings.Contains(email, "@") {
		return "", ErrInvalidAuthEntryInput
	}
	return email, nil
}

func validTimezone(raw string) bool {
	timezone := strings.TrimSpace(raw)
	if timezone == "" || timezone == "Local" {
		return false
	}
	_, err := time.LoadLocation(timezone)
	return err == nil
}

func newOpaqueID() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		panic("read opaque identifier randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

// NewUsecase 创建身份用例。
// 依赖均为领域接口，便于通过内存仓储进行单元测试。
func NewUsecase(users UserRepository, identities IdentityRepository, sessions SessionRepository, tx shared.TxRunner) *Usecase {
	return &Usecase{
		users:      users,
		identities: identities,
		sessions:   sessions,
		tx:         tx,
		clock: func() time.Time {
			return time.Now().UTC()
		},
	}
}

// BindGuest 将游客升级为同一个用户上的已绑定账户。
// 该流程不会创建用户，也不会触及奖励、余额或账本。重复绑定同一身份时返回同一用户。
func (usecase *Usecase) BindGuest(ctx context.Context, input BindGuestInput) (*User, error) {
	if input.UserID == "" || input.Provider == "" || input.Subject == "" {
		return nil, ErrInvalidBindingInput
	}

	var boundUser *User
	err := usecase.tx.WithinTx(ctx, func(transactionContext context.Context) error {
		user, err := usecase.users.Find(transactionContext, input.UserID)
		if err != nil {
			return err
		}
		if user.AccountStatus != AccountStatusNormal {
			return ErrGuestBindingNotAllowed
		}

		existingIdentity, err := usecase.identities.FindByProviderSubject(transactionContext, input.Provider, input.Subject)
		switch {
		case err == nil:
			if existingIdentity.UserID != user.ID {
				return ErrIdentityAlreadyBound
			}
			boundUser, err = usecase.ensureBound(transactionContext, user)
			return err
		case !errors.Is(err, ErrIdentityNotFound):
			return err
		}

		if user.BindingState != BindingStateGuest {
			return ErrGuestBindingNotAllowed
		}

		if err := usecase.identities.Create(transactionContext, ExternalIdentity{
			ID:        uuid.NewString(),
			Provider:  input.Provider,
			Subject:   input.Subject,
			UserID:    user.ID,
			CreatedAt: usecase.clock(),
		}); err != nil {
			return err
		}

		boundUser, err = usecase.users.MarkBound(transactionContext, user.ID, usecase.clock())
		return err
	})
	if err == nil {
		return boundUser, nil
	}

	// MongoDB 唯一索引在并发提交时可能把第二个请求报告为重复键。
	// 若身份已由同一用户写入，则把该请求收敛为幂等成功。
	if errors.Is(err, ErrIdentityAlreadyBound) {
		return usecase.resolveExistingBinding(ctx, input)
	}
	return nil, err
}

// ChangeAccountStatus 将用户改为封禁或删除，并使既有登录态立即失效。
func (usecase *Usecase) ChangeAccountStatus(ctx context.Context, userID string, targetStatus AccountStatus) (*User, error) {
	if userID == "" || (targetStatus != AccountStatusBanned && targetStatus != AccountStatusDeleted) {
		return nil, ErrInvalidAccountStatus
	}

	var changedUser *User
	err := usecase.tx.WithinTx(ctx, func(transactionContext context.Context) error {
		user, err := usecase.users.Find(transactionContext, userID)
		if err != nil {
			return err
		}
		if user.AccountStatus == targetStatus {
			changedUser = user
			return nil
		}

		now := usecase.clock()
		changedUser, err = usecase.users.ChangeAccountStatus(transactionContext, userID, targetStatus, now)
		if err != nil {
			return err
		}
		if err := usecase.sessions.RevokeActiveByUser(transactionContext, userID, now); err != nil {
			return err
		}
		// 已删除账号不再占用邮箱。仅在账号入口完整装配时写入凭据状态，保留
		// 既有纯身份用例对历史仓储实现的兼容性。
		if targetStatus == AccountStatusDeleted && usecase.credentials != nil {
			return usecase.credentials.DeactivateByUser(transactionContext, userID, now)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changedUser, nil
}

// ValidateSession 验证会话本身、用户状态与签发版本。
// 只要封禁或删除递增用户版本，旧会话就会在下一次校验立即失效。
func (usecase *Usecase) ValidateSession(ctx context.Context, sessionID string, now time.Time) (*Session, error) {
	session, err := usecase.sessions.Find(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}
	if session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return nil, ErrSessionInvalid
	}

	user, err := usecase.users.Find(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}
	if user.AccountStatus != AccountStatusNormal || user.SessionVersion != session.SessionVersion {
		return nil, ErrSessionInvalid
	}
	// 内容访问级别来自同一次已验证的用户读取，供模板选择使用，不能信任 HTTP 输入。
	session.ContentAccess = user.ContentAccess
	return session, nil
}

func (usecase *Usecase) resolveExistingBinding(ctx context.Context, input BindGuestInput) (*User, error) {
	existingIdentity, err := usecase.identities.FindByProviderSubject(ctx, input.Provider, input.Subject)
	if err != nil {
		return nil, err
	}
	if existingIdentity.UserID != input.UserID {
		return nil, ErrIdentityAlreadyBound
	}

	user, err := usecase.users.Find(ctx, input.UserID)
	if err != nil {
		return nil, err
	}
	if user.AccountStatus != AccountStatusNormal {
		return nil, ErrGuestBindingNotAllowed
	}
	return usecase.ensureBound(ctx, user)
}

func (usecase *Usecase) ensureBound(ctx context.Context, user *User) (*User, error) {
	if user.BindingState == BindingStateBound {
		return user, nil
	}
	if user.BindingState != BindingStateGuest {
		return nil, ErrGuestBindingNotAllowed
	}
	return usecase.users.MarkBound(ctx, user.ID, usecase.clock())
}
