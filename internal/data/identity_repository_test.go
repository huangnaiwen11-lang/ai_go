package data

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestNewUserDocumentPersistsAssignedRole(t *testing.T) {
	document := newUserDocument(identity.User{ID: "admin-1", Role: "admin"})
	encoded, err := bson.Marshal(document)
	if err != nil {
		t.Fatalf("marshal user document: %v", err)
	}
	var fields bson.M
	if err := bson.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode user document: %v", err)
	}
	if role, ok := fields["role"]; !ok || role != "admin" {
		t.Fatalf("persisted role = %#v, present = %t", role, ok)
	}
}

func TestToBizUserReturnsAssignedRole(t *testing.T) {
	user := toBizUser(model.UserDocument{ID: "admin-1", Role: "admin"})
	if user.Role != "admin" {
		t.Fatalf("user role = %q", user.Role)
	}
}

func TestMongoIdentityUsecaseConcurrentBindingKeepsOneUserAndOneIdentity(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}

	userID := uuid.NewString()
	provider := "email"
	subject := "concurrent-" + uuid.NewString() + "@example.test"
	insertIdentityID := ""
	insertedAt := time.Now().UTC()
	usersCollection := database.Collection(schema.CollectionUsers)
	identitiesCollection := database.Collection(schema.CollectionIdentities)
	if _, err := usersCollection.InsertOne(ctx, model.UserDocument{
		ID:             userID,
		AccountStatus:  string(identity.AccountStatusNormal),
		BindingState:   string(identity.BindingStateGuest),
		Timezone:       "Asia/Shanghai",
		SessionVersion: 1,
		CreatedAt:      insertedAt,
		UpdatedAt:      insertedAt,
	}); err != nil {
		t.Fatalf("insert guest user: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if insertIdentityID != "" {
			if _, err := identitiesCollection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: insertIdentityID}}); err != nil {
				t.Errorf("delete test identity %q: %v", insertIdentityID, err)
			}
		}
		if _, err := usersCollection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: userID}}); err != nil {
			t.Errorf("delete test user %q: %v", userID, err)
		}
	})

	data := &Data{database: database}
	usecase := identity.NewUsecase(
		NewUserRepository(data),
		NewIdentityRepository(data),
		NewSessionRepository(data),
		NewMongoTxRunner(client),
	)

	start := make(chan struct{})
	results := make(chan bindGuestResult, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			user, err := usecase.BindGuest(context.Background(), identity.BindGuestInput{
				UserID:   userID,
				Provider: provider,
				Subject:  subject,
			})
			results <- bindGuestResult{user: user, err: err}
		}()
	}
	close(start)
	waitGroup.Wait()
	close(results)

	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent BindGuest() error = %v", result.err)
		}
		if result.user.ID != userID {
			t.Fatalf("concurrent BindGuest() user id = %q, want %q", result.user.ID, userID)
		}
	}

	storedIdentity, err := NewIdentityRepository(data).FindByProviderSubject(ctx, provider, subject)
	if err != nil {
		t.Fatalf("read bound identity: %v", err)
	}
	insertIdentityID = storedIdentity.ID
	if storedIdentity.UserID != userID {
		t.Fatalf("stored identity user id = %q, want %q", storedIdentity.UserID, userID)
	}

	count, err := identitiesCollection.CountDocuments(ctx, bson.D{{Key: "provider", Value: provider}, {Key: "subject", Value: subject}})
	if err != nil {
		t.Fatalf("count identities: %v", err)
	}
	if count != 1 {
		t.Fatalf("identity count = %d, want 1", count)
	}
}

func TestMongoIdentityUsecaseBanRevokesSessionAndIncrementsVersion(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}

	userID := uuid.NewString()
	sessionID := uuid.NewString()
	insertedAt := time.Now().UTC()
	usersCollection := database.Collection(schema.CollectionUsers)
	sessionsCollection := database.Collection(schema.CollectionSessions)
	if _, err := usersCollection.InsertOne(ctx, model.UserDocument{
		ID:             userID,
		AccountStatus:  string(identity.AccountStatusNormal),
		BindingState:   string(identity.BindingStateBound),
		Timezone:       "Asia/Shanghai",
		SessionVersion: 3,
		CreatedAt:      insertedAt,
		UpdatedAt:      insertedAt,
	}); err != nil {
		t.Fatalf("insert normal user: %v", err)
	}
	if _, err := sessionsCollection.InsertOne(ctx, model.SessionDocument{
		ID:             sessionID,
		UserID:         userID,
		SessionVersion: 3,
		ExpiresAt:      insertedAt.Add(time.Hour),
	}); err != nil {
		t.Fatalf("insert active session: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		if _, err := sessionsCollection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: sessionID}}); err != nil {
			t.Errorf("delete test session %q: %v", sessionID, err)
		}
		if _, err := usersCollection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: userID}}); err != nil {
			t.Errorf("delete test user %q: %v", userID, err)
		}
	})

	data := &Data{database: database}
	usecase := identity.NewUsecase(
		NewUserRepository(data),
		NewIdentityRepository(data),
		NewSessionRepository(data),
		NewMongoTxRunner(client),
	)

	changedUser, err := usecase.ChangeAccountStatus(ctx, userID, identity.AccountStatusBanned)
	if err != nil {
		t.Fatalf("ChangeAccountStatus() error = %v", err)
	}
	if changedUser.SessionVersion != 4 {
		t.Fatalf("session version = %d, want 4", changedUser.SessionVersion)
	}
	if _, err := usecase.ValidateSession(ctx, sessionID, time.Now()); !errors.Is(err, identity.ErrSessionInvalid) {
		t.Fatalf("ValidateSession() error = %v, want ErrSessionInvalid", err)
	}

	var storedSession model.SessionDocument
	if err := sessionsCollection.FindOne(ctx, bson.D{{Key: "_id", Value: sessionID}}).Decode(&storedSession); err != nil {
		t.Fatalf("read revoked session: %v", err)
	}
	if storedSession.RevokedAt == nil {
		t.Fatal("stored session should be revoked")
	}
}

type bindGuestResult struct {
	user *identity.User
	err  error
}
