package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/identity"
)

func TestBindGuestUpgradesOriginalUserWithoutChangingTimezone(t *testing.T) {
	ctx := context.Background()
	users := newMemoryUsers(identity.User{
		ID:             "user-guest-1",
		AccountStatus:  identity.AccountStatusNormal,
		BindingState:   identity.BindingStateGuest,
		Timezone:       "Asia/Shanghai",
		SessionVersion: 1,
	})
	identities := newMemoryIdentities()
	sessions := newMemorySessions()
	usecase := identity.NewUsecase(users, identities, sessions, memoryTxRunner{})

	user, err := usecase.BindGuest(ctx, identity.BindGuestInput{
		UserID:   "user-guest-1",
		Provider: "email",
		Subject:  "guest@example.test",
	})
	if err != nil {
		t.Fatalf("BindGuest() error = %v", err)
	}
	if user.ID != "user-guest-1" {
		t.Fatalf("BindGuest() user id = %q, want original user id", user.ID)
	}
	if user.BindingState != identity.BindingStateBound {
		t.Fatalf("BindGuest() binding state = %q, want %q", user.BindingState, identity.BindingStateBound)
	}
	if user.Timezone != "Asia/Shanghai" {
		t.Fatalf("BindGuest() timezone = %q, want first written timezone unchanged", user.Timezone)
	}

	storedIdentity, err := identities.FindByProviderSubject(ctx, "email", "guest@example.test")
	if err != nil {
		t.Fatalf("FindByProviderSubject() error = %v", err)
	}
	if storedIdentity.UserID != "user-guest-1" {
		t.Fatalf("identity user id = %q, want original user id", storedIdentity.UserID)
	}
}

func TestBindGuestReturnsSameUserForRepeatedRequest(t *testing.T) {
	ctx := context.Background()
	users := newMemoryUsers(identity.User{
		ID:             "user-guest-2",
		AccountStatus:  identity.AccountStatusNormal,
		BindingState:   identity.BindingStateGuest,
		Timezone:       "Asia/Shanghai",
		SessionVersion: 1,
	})
	identities := newMemoryIdentities()
	usecase := identity.NewUsecase(users, identities, newMemorySessions(), memoryTxRunner{})
	input := identity.BindGuestInput{
		UserID:   "user-guest-2",
		Provider: "email",
		Subject:  "idempotent@example.test",
	}

	first, err := usecase.BindGuest(ctx, input)
	if err != nil {
		t.Fatalf("first BindGuest() error = %v", err)
	}
	second, err := usecase.BindGuest(ctx, input)
	if err != nil {
		t.Fatalf("second BindGuest() error = %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("repeated BindGuest() user id = %q, want %q", second.ID, first.ID)
	}
}

func TestBindGuestRejectsIdentityThatBelongsToAnotherUser(t *testing.T) {
	ctx := context.Background()
	users := newMemoryUsers(
		identity.User{
			ID:             "user-bound",
			AccountStatus:  identity.AccountStatusNormal,
			BindingState:   identity.BindingStateBound,
			Timezone:       "Asia/Shanghai",
			SessionVersion: 1,
		},
		identity.User{
			ID:             "user-guest-3",
			AccountStatus:  identity.AccountStatusNormal,
			BindingState:   identity.BindingStateGuest,
			Timezone:       "Asia/Shanghai",
			SessionVersion: 1,
		},
	)
	identities := newMemoryIdentities(identity.ExternalIdentity{
		ID:       "identity-1",
		Provider: "email",
		Subject:  "taken@example.test",
		UserID:   "user-bound",
	})
	usecase := identity.NewUsecase(users, identities, newMemorySessions(), memoryTxRunner{})

	_, err := usecase.BindGuest(ctx, identity.BindGuestInput{
		UserID:   "user-guest-3",
		Provider: "email",
		Subject:  "taken@example.test",
	})
	if !errors.Is(err, identity.ErrIdentityAlreadyBound) {
		t.Fatalf("BindGuest() error = %v, want ErrIdentityAlreadyBound", err)
	}
}

func TestChangeAccountStatusImmediatelyInvalidatesExistingSession(t *testing.T) {
	ctx := context.Background()
	for _, targetStatus := range []identity.AccountStatus{identity.AccountStatusBanned, identity.AccountStatusDeleted} {
		t.Run(string(targetStatus), func(t *testing.T) {
			users := newMemoryUsers(identity.User{
				ID:             "user-session-1",
				AccountStatus:  identity.AccountStatusNormal,
				BindingState:   identity.BindingStateBound,
				Timezone:       "Asia/Shanghai",
				SessionVersion: 3,
			})
			sessions := newMemorySessions(identity.Session{
				ID:             "session-before-status-change",
				UserID:         "user-session-1",
				SessionVersion: 3,
				ExpiresAt:      time.Now().Add(time.Hour),
			})
			usecase := identity.NewUsecase(users, newMemoryIdentities(), sessions, memoryTxRunner{})

			changed, err := usecase.ChangeAccountStatus(ctx, "user-session-1", targetStatus)
			if err != nil {
				t.Fatalf("ChangeAccountStatus() error = %v", err)
			}
			if changed.SessionVersion != 4 {
				t.Fatalf("session version = %d, want 4", changed.SessionVersion)
			}
			if _, err := usecase.ValidateSession(ctx, "session-before-status-change", time.Now()); !errors.Is(err, identity.ErrSessionInvalid) {
				t.Fatalf("ValidateSession() error = %v, want ErrSessionInvalid", err)
			}

			storedSession, err := sessions.Find(ctx, "session-before-status-change")
			if err != nil {
				t.Fatalf("Find() session error = %v", err)
			}
			if storedSession.RevokedAt == nil {
				t.Fatal("session should be explicitly revoked")
			}
		})
	}
}

type memoryTxRunner struct{}

func (memoryTxRunner) WithinTx(ctx context.Context, operation func(context.Context) error) error {
	return operation(ctx)
}

type memoryUsers struct {
	mu    sync.Mutex
	users map[string]identity.User
}

func newMemoryUsers(users ...identity.User) *memoryUsers {
	storage := &memoryUsers{users: make(map[string]identity.User, len(users))}
	for _, user := range users {
		storage.users[user.ID] = user
	}
	return storage
}

func (storage *memoryUsers) Find(ctx context.Context, userID string) (*identity.User, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	user, ok := storage.users[userID]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	return &user, nil
}

func (storage *memoryUsers) MarkBound(ctx context.Context, userID string, updatedAt time.Time) (*identity.User, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	user, ok := storage.users[userID]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	if user.AccountStatus != identity.AccountStatusNormal || user.BindingState != identity.BindingStateGuest {
		return nil, identity.ErrGuestBindingNotAllowed
	}
	user.BindingState = identity.BindingStateBound
	user.UpdatedAt = updatedAt
	storage.users[userID] = user
	return &user, nil
}

func (storage *memoryUsers) ChangeAccountStatus(ctx context.Context, userID string, targetStatus identity.AccountStatus, updatedAt time.Time) (*identity.User, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	user, ok := storage.users[userID]
	if !ok {
		return nil, identity.ErrUserNotFound
	}
	user.AccountStatus = targetStatus
	user.SessionVersion++
	user.UpdatedAt = updatedAt
	storage.users[userID] = user
	return &user, nil
}

type memoryIdentities struct {
	mu         sync.Mutex
	identities map[string]identity.ExternalIdentity
}

func newMemoryIdentities(entries ...identity.ExternalIdentity) *memoryIdentities {
	storage := &memoryIdentities{identities: make(map[string]identity.ExternalIdentity, len(entries))}
	for _, entry := range entries {
		storage.identities[identityKey(entry.Provider, entry.Subject)] = entry
	}
	return storage
}

func (storage *memoryIdentities) FindByProviderSubject(ctx context.Context, provider, subject string) (*identity.ExternalIdentity, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	entry, ok := storage.identities[identityKey(provider, subject)]
	if !ok {
		return nil, identity.ErrIdentityNotFound
	}
	return &entry, nil
}

func (storage *memoryIdentities) Create(ctx context.Context, entry identity.ExternalIdentity) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	key := identityKey(entry.Provider, entry.Subject)
	if _, exists := storage.identities[key]; exists {
		return identity.ErrIdentityAlreadyBound
	}
	storage.identities[key] = entry
	return nil
}

type memorySessions struct {
	mu       sync.Mutex
	sessions map[string]identity.Session
}

func newMemorySessions(entries ...identity.Session) *memorySessions {
	storage := &memorySessions{sessions: make(map[string]identity.Session, len(entries))}
	for _, entry := range entries {
		storage.sessions[entry.ID] = entry
	}
	return storage
}

func (storage *memorySessions) Find(ctx context.Context, sessionID string) (*identity.Session, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	session, ok := storage.sessions[sessionID]
	if !ok {
		return nil, identity.ErrSessionNotFound
	}
	return &session, nil
}

func (storage *memorySessions) RevokeActiveByUser(ctx context.Context, userID string, revokedAt time.Time) error {
	storage.mu.Lock()
	defer storage.mu.Unlock()

	for sessionID, session := range storage.sessions {
		if session.UserID == userID && session.RevokedAt == nil {
			session.RevokedAt = &revokedAt
			storage.sessions[sessionID] = session
		}
	}
	return nil
}

func identityKey(provider, subject string) string {
	return provider + "\x00" + subject
}
