package data

import (
	"context"
	"errors"
	"testing"

	"ai-business-service/internal/biz/adminpricing"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func newAdminPricingTestRepository(t *testing.T) (adminpricing.Repository, context.Context, *mongo.Database) {
	t.Helper()
	client := newLocalMongoClient(t)
	database := client.Database("admin_pricing_" + uuid.NewString())
	ctx := context.Background()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}
	t.Cleanup(func() { _ = database.Drop(context.Background()) })
	return NewAdminPricingRepository(&Data{client: client, database: database}), ctx, database
}

func TestMongoAdminPricingConfigRoundTripAndAudit(t *testing.T) {
	repository, ctx, database := newAdminPricingTestRepository(t)
	actor := adminpricing.Actor{ID: "pricing-admin", Role: "super_admin"}
	input := adminpricing.SystemConfigInput{
		Key:      adminpricing.CoinPackagesKey,
		Value:    map[string]any{"coins_1000": map[string]any{"coins": int64(1800), "enabled": false}},
		Category: "wallet", Environment: "all", ChangeReason: "pricing test",
	}
	saved, err := repository.SaveSystemConfig(ctx, actor, input)
	if err != nil {
		t.Fatalf("SaveSystemConfig() error = %v", err)
	}
	if saved.Key != adminpricing.CoinPackagesKey || saved.UpdatedBy != actor.ID || !saved.Enabled {
		t.Fatalf("saved = %#v", saved)
	}
	loaded, err := repository.LoadSystemConfig(ctx, adminpricing.CoinPackagesKey, "all")
	if err != nil {
		t.Fatalf("LoadSystemConfig() error = %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded config is nil")
	}
	value, ok := loaded.Value.(map[string]any)
	if !ok {
		t.Fatalf("loaded value type = %T", loaded.Value)
	}
	entry, ok := value["coins_1000"].(map[string]any)
	if !ok || entry["enabled"] != false {
		t.Fatalf("loaded value = %#v", loaded.Value)
	}
	audits, err := database.Collection(schema.CollectionAdminAudit).CountDocuments(ctx, bson.M{"action": "admin_pricing_config_update", "target_id": adminpricing.CoinPackagesKey + "|all"})
	if err != nil || audits != 1 {
		t.Fatalf("audit count = %d, error = %v", audits, err)
	}
}

func TestMongoAdminPricingDeleteDisablesConfig(t *testing.T) {
	repository, ctx, _ := newAdminPricingTestRepository(t)
	actor := adminpricing.Actor{ID: "pricing-admin", Role: "super_admin"}
	_, err := repository.SaveSystemConfig(ctx, actor, adminpricing.SystemConfigInput{Key: "wallet.firstRechargeBonusCoins", Value: int64(20), Environment: "all"})
	if err != nil {
		t.Fatalf("SaveSystemConfig() error = %v", err)
	}
	deleted, err := repository.DeleteSystemConfig(ctx, actor, "wallet.firstRechargeBonusCoins", "all")
	if err != nil || deleted.Enabled {
		t.Fatalf("DeleteSystemConfig() = %#v, error = %v", deleted, err)
	}
	if _, err := repository.LoadSystemConfig(ctx, "wallet.firstRechargeBonusCoins", "all"); !errors.Is(err, adminpricing.ErrNotFound) {
		t.Fatalf("deleted config load error = %v, want ErrNotFound", err)
	}
}
