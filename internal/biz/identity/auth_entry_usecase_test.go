package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/authcredential"
)

// TestRegisterCreatesBoundUserZeroBalanceAndThirtyDaySession 固化本地账号入口注册语义。
func TestRegisterCreatesBoundUserZeroBalanceAndThirtyDaySession(t *testing.T) {
	fixture := newAuthEntryFixture(t)
	result, err := fixture.usecase.Register(context.Background(), RegisterInput{
		Email:       "  USER@example.test ",
		Password:    "correct-horse",
		Timezone:    "Asia/Shanghai",
		DisplayName: "测试用户",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if result.User.BindingState != BindingStateBound || result.User.AccountStatus != AccountStatusNormal {
		t.Fatalf("registered user = %#v", result.User)
	}
	if result.User.DisplayName != "测试用户" || result.AccountBalance != 0 {
		t.Fatalf("registration projection = %#v", result)
	}
	if got, want := result.Session.ExpiresAt, fixture.now.Add(30*24*time.Hour); !got.Equal(want) {
		t.Fatalf("session expiration = %s, want %s", got, want)
	}
	if _, err := fixture.credentials.FindActiveByEmail(context.Background(), "user@example.test"); err != nil {
		t.Fatalf("registered credential missing: %v", err)
	}
}

// TestRegisterDeletedEmailCreatesNewUserButBannedEmailConflicts 防止删除账号占用邮箱，也
// 防止通过重新注册绕过封禁。
func TestRegisterDeletedEmailCreatesNewUserButBannedEmailConflicts(t *testing.T) {
	fixture := newAuthEntryFixture(t)
	fixture.seedCredential("deleted@example.test", AccountStatusDeleted, false)
	created, err := fixture.usecase.Register(context.Background(), RegisterInput{Email: "deleted@example.test", Password: "correct-horse", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("Register(deleted email) error = %v", err)
	}
	fixture.seedCredential("banned@example.test", AccountStatusBanned, true)
	_, err = fixture.usecase.Register(context.Background(), RegisterInput{Email: "banned@example.test", Password: "correct-horse", Timezone: "UTC"})
	if !errors.Is(err, ErrEmailAlreadyRegistered) {
		t.Fatalf("Register(banned email) error = %v, want ErrEmailAlreadyRegistered", err)
	}
	if created.User.ID == "" {
		t.Fatal("deleted email registration must create a new user")
	}
}

// TestPasswordLoginHidesWhetherEmailExists 验证未知邮箱和错误密码使用同一领域错误。
func TestPasswordLoginHidesWhetherEmailExists(t *testing.T) {
	fixture := newAuthEntryFixture(t)
	if _, err := fixture.usecase.Register(context.Background(), RegisterInput{Email: "known@example.test", Password: "correct-horse", Timezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	_, wrongPasswordErr := fixture.usecase.LoginWithPassword(context.Background(), PasswordLoginInput{Email: "known@example.test", Password: "wrong-horse"})
	_, unknownEmailErr := fixture.usecase.LoginWithPassword(context.Background(), PasswordLoginInput{Email: "unknown@example.test", Password: "wrong-horse"})
	if !errors.Is(wrongPasswordErr, ErrInvalidCredentials) || !errors.Is(unknownEmailErr, ErrInvalidCredentials) {
		t.Fatalf("login errors = %v, %v, both must be ErrInvalidCredentials", wrongPasswordErr, unknownEmailErr)
	}
}

// TestGuestLoginRejectsWebAndReusesSameMobileDeviceUser 固化游客只能在移动端创建和设备复用规则。
func TestGuestLoginRejectsWebAndReusesSameMobileDeviceUser(t *testing.T) {
	fixture := newAuthEntryFixture(t)
	if _, err := fixture.usecase.LoginGuest(context.Background(), GuestLoginInput{Platform: "web", DeviceID: "device-web-1", Timezone: "UTC"}); !errors.Is(err, ErrWebGuestLoginDisabled) {
		t.Fatalf("LoginGuest(web) error = %v, want ErrWebGuestLoginDisabled", err)
	}
	first, err := fixture.usecase.LoginGuest(context.Background(), GuestLoginInput{Platform: "ios", DeviceID: "device-ios-1", Timezone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("first LoginGuest() error = %v", err)
	}
	second, err := fixture.usecase.LoginGuest(context.Background(), GuestLoginInput{Platform: "ios", DeviceID: "device-ios-1", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("second LoginGuest() error = %v", err)
	}
	if first.User.ID != second.User.ID || second.User.Timezone != "Asia/Shanghai" {
		t.Fatalf("guest reuse result = %#v, %#v", first.User, second.User)
	}
}

// TestDeleteAccountDisablesCredential 验证删除账户后邮箱可被后续新用户注册，旧凭据不再
// 参与活动邮箱的唯一性约束。
func TestDeleteAccountDisablesCredential(t *testing.T) {
	fixture := newAuthEntryFixture(t)
	registered, err := fixture.usecase.Register(context.Background(), RegisterInput{Email: "deleted-later@example.test", Password: "correct-horse", Timezone: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.usecase.ChangeAccountStatus(context.Background(), registered.User.ID, AccountStatusDeleted); err != nil {
		t.Fatalf("ChangeAccountStatus() error = %v", err)
	}
	if _, err := fixture.credentials.FindActiveByEmail(context.Background(), "deleted-later@example.test"); !errors.Is(err, authcredential.ErrCredentialNotFound) {
		t.Fatalf("deleted user credential lookup error = %v, want ErrCredentialNotFound", err)
	}
}

type authEntryFixture struct {
	usecase     *Usecase
	users       *authMemoryUsers
	credentials *authMemoryCredentials
	now         time.Time
}

func newAuthEntryFixture(t *testing.T) *authEntryFixture {
	t.Helper()
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	users := newAuthMemoryUsers()
	credentials := newAuthMemoryCredentials()
	policy := authcredential.MustNewPolicy(authcredential.Params{MemoryKiB: 19 * 1024, TimeCost: 2, Parallelism: 1, SaltBytes: 16, KeyBytes: 32})
	usecase := NewAuthUsecase(users, authMemoryIdentities{}, newAuthMemorySessions(), newAuthMemoryAccounts(), credentials, policy, authMemoryTx{})
	usecase.clock = func() time.Time { return now }
	return &authEntryFixture{usecase: usecase, users: users, credentials: credentials, now: now}
}

func (fixture *authEntryFixture) seedCredential(email string, status AccountStatus, active bool) {
	userID := "seed-" + email
	fixture.users.users[userID] = User{ID: userID, AccountStatus: status, BindingState: BindingStateBound, Timezone: "UTC", SessionVersion: 1}
	fixture.credentials.byEmail[email] = authcredential.Credential{ID: "credential-" + email, UserID: userID, EmailNormalized: email, PasswordHash: "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Active: active}
}

type authMemoryTx struct{}

func (authMemoryTx) WithinTx(ctx context.Context, operation func(context.Context) error) error {
	return operation(ctx)
}

type authMemoryUsers struct {
	mu    sync.Mutex
	users map[string]User
}

func newAuthMemoryUsers() *authMemoryUsers { return &authMemoryUsers{users: map[string]User{}} }
func (s *authMemoryUsers) Find(_ context.Context, id string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	return &user, nil
}
func (s *authMemoryUsers) Create(_ context.Context, user User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.users[user.ID]; exists {
		return errors.New("duplicate user")
	}
	s.users[user.ID] = user
	return nil
}
func (s *authMemoryUsers) FindGuestByDevice(_ context.Context, platform, deviceID string) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range s.users {
		if user.GuestPlatform == platform && user.GuestDeviceID == deviceID {
			copy := user
			return &copy, nil
		}
	}
	return nil, ErrUserNotFound
}
func (s *authMemoryUsers) MarkBound(context.Context, string, time.Time) (*User, error) {
	return nil, errors.New("not used")
}
func (s *authMemoryUsers) ChangeAccountStatus(_ context.Context, userID string, status AccountStatus, updatedAt time.Time) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[userID]
	if !ok {
		return nil, ErrUserNotFound
	}
	user.AccountStatus = status
	user.SessionVersion++
	user.UpdatedAt = updatedAt
	s.users[userID] = user
	return &user, nil
}

type authMemoryIdentities struct{}

func (authMemoryIdentities) FindByProviderSubject(context.Context, string, string) (*ExternalIdentity, error) {
	return nil, ErrIdentityNotFound
}
func (authMemoryIdentities) Create(context.Context, ExternalIdentity) error { return nil }

type authMemorySessions struct {
	mu       sync.Mutex
	sessions map[string]Session
}

func newAuthMemorySessions() *authMemorySessions {
	return &authMemorySessions{sessions: map[string]Session{}}
}
func (s *authMemorySessions) Find(_ context.Context, id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return &session, nil
}
func (s *authMemorySessions) Create(_ context.Context, session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
	return nil
}
func (s *authMemorySessions) RevokeActiveByUser(_ context.Context, userID string, revokedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, session := range s.sessions {
		if session.UserID == userID && session.RevokedAt == nil {
			session.RevokedAt = &revokedAt
			s.sessions[id] = session
		}
	}
	return nil
}

type authMemoryAccounts struct {
	mu       sync.Mutex
	accounts map[string]int64
}

func newAuthMemoryAccounts() *authMemoryAccounts {
	return &authMemoryAccounts{accounts: map[string]int64{}}
}
func (s *authMemoryAccounts) CreateZero(_ context.Context, id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.accounts[id]; exists {
		return errors.New("duplicate account")
	}
	s.accounts[id] = 0
	return nil
}

type authMemoryCredentials struct {
	mu      sync.Mutex
	byEmail map[string]authcredential.Credential
}

func newAuthMemoryCredentials() *authMemoryCredentials {
	return &authMemoryCredentials{byEmail: map[string]authcredential.Credential{}}
}
func (s *authMemoryCredentials) FindActiveByEmail(_ context.Context, email string) (*authcredential.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, ok := s.byEmail[email]
	if !ok || !credential.Active {
		return nil, authcredential.ErrCredentialNotFound
	}
	return &credential, nil
}
func (s *authMemoryCredentials) Create(_ context.Context, credential authcredential.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byEmail[credential.EmailNormalized]; ok && existing.Active {
		return authcredential.ErrEmailAlreadyRegistered
	}
	s.byEmail[credential.EmailNormalized] = credential
	return nil
}
func (s *authMemoryCredentials) UpdatePasswordHash(context.Context, string, string, time.Time) error {
	return nil
}
func (s *authMemoryCredentials) DeactivateByUser(_ context.Context, userID string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for email, credential := range s.byEmail {
		if credential.UserID == userID {
			credential.Active = false
			credential.UpdatedAt = updatedAt
			s.byEmail[email] = credential
		}
	}
	return nil
}
