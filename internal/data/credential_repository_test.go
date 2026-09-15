package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/biz/authcredential"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestMongoCredentialRepositoryOnlyReservesActiveEmail 验证删除账号停用凭据后，邮箱
// 可以由新的 Go 用户重新注册；正常和封禁账号的活动凭据仍受唯一约束保护。
func TestMongoCredentialRepositoryOnlyReservesActiveEmail(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}

	repository := NewCredentialRepository(&Data{database: database})
	email := "credential-" + uuid.NewString() + "@example.test"
	first := authcredential.Credential{ID: uuid.NewString(), UserID: uuid.NewString(), EmailNormalized: email, PasswordHash: "$argon2id$test", Active: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	second := authcredential.Credential{ID: uuid.NewString(), UserID: uuid.NewString(), EmailNormalized: email, PasswordHash: "$argon2id$test", Active: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	t.Cleanup(func() {
		cleanupCredentialDocuments(t, database, first.ID, second.ID)
	})

	if err := repository.Create(ctx, first); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	if err := repository.Create(ctx, second); !errors.Is(err, authcredential.ErrEmailAlreadyRegistered) {
		t.Fatalf("Create(second active email) error = %v, want ErrEmailAlreadyRegistered", err)
	}
	if err := repository.DeactivateByUser(ctx, first.UserID, time.Now().UTC()); err != nil {
		t.Fatalf("DeactivateByUser() error = %v", err)
	}
	if err := repository.Create(ctx, second); err != nil {
		t.Fatalf("Create(second after deactivate) error = %v", err)
	}
}

func cleanupCredentialDocuments(t *testing.T, database *mongo.Database, ids ...string) {
	t.Helper()
	if _, err := database.Collection(schema.CollectionCredentials).DeleteMany(context.Background(), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}); err != nil {
		t.Errorf("cleanup credential documents: %v", err)
	}
}
