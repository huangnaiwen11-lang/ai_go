package data

import (
	"context"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/data/schema"
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestRuntimeAppSchemaInitializerRejectsUnconfiguredData(t *testing.T) {
	err := NewRuntimeAppSchemaInitializer(nil).Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runtime App schema initializer is not configured") {
		t.Fatalf("Ensure() error = %v, want unconfigured initializer error", err)
	}
}

func TestMongoRuntimeAppSchemaInitializerCreatesOnlyRequiredAppsIndexes(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("runtime_app_schema_" + uuid.NewString())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := database.Drop(ctx); err != nil {
			t.Errorf("drop runtime App schema test database: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initializer := NewRuntimeAppSchemaInitializer(&Data{client: client, database: database})
	if err := initializer.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if err := initializer.Ensure(ctx); err != nil {
		t.Fatalf("second Ensure() error = %v", err)
	}

	collections, err := database.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		t.Fatalf("list collections: %v", err)
	}
	if len(collections) != 1 || collections[0] != schema.CollectionApps {
		t.Fatalf("collections = %#v, want only %q", collections, schema.CollectionApps)
	}

	actualIndexes, err := database.Collection(schema.CollectionApps).Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("list App indexes: %v", err)
	}
	if len(actualIndexes) != len(schema.RuntimeAppIndexes())+1 {
		t.Fatalf("App index count = %d, want %d resolver indexes plus _id", len(actualIndexes), len(schema.RuntimeAppIndexes())+1)
	}
	for _, expected := range schema.RuntimeAppIndexes() {
		actual := runtimeAppIndexByName(actualIndexes, expected.Name)
		if actual == nil {
			t.Errorf("missing resolver index %q", expected.Name)
			continue
		}
		var keys bson.D
		if err := bson.Unmarshal(actual.KeysDocument, &keys); err != nil {
			t.Errorf("decode index %q keys: %v", expected.Name, err)
			continue
		}
		if !equalRuntimeAppIndexKeys(keys, expected.Keys) {
			t.Errorf("index %q keys = %#v, want %#v", expected.Name, keys, expected.Keys)
		}
		collation := runtimeAppIndexCollation(t, ctx, database.Collection(schema.CollectionApps), expected.Name)
		if collation == nil || collation.Locale != expected.Collation.Locale || collation.Strength != expected.Collation.Strength {
			t.Errorf("index %q collation = %#v, want locale=%q strength=%d", expected.Name, collation, expected.Collation.Locale, expected.Collation.Strength)
		}
	}
}

func TestMongoRuntimeAppSchemaInitializerRejectsWrongExistingIndexCollation(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("runtime_app_schema_conflict_" + uuid.NewString())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := database.Drop(ctx); err != nil {
			t.Errorf("drop runtime App schema conflict test database: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := database.CreateCollection(ctx, schema.CollectionApps); err != nil {
		t.Fatalf("create App collection: %v", err)
	}
	wrong := schema.RuntimeAppIndexes()[0]
	if _, err := database.Collection(schema.CollectionApps).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    wrong.Keys,
		Options: options.Index().SetName(wrong.Name),
	}); err != nil {
		t.Fatalf("create wrong-collation App index: %v", err)
	}

	err := NewRuntimeAppSchemaInitializer(&Data{client: client, database: database}).Ensure(ctx)
	if err == nil || !strings.Contains(err.Error(), "create runtime App indexes") {
		t.Fatalf("Ensure() error = %v, want wrong collation rejection", err)
	}
}

func runtimeAppIndexByName(indexes []mongo.IndexSpecification, name string) *mongo.IndexSpecification {
	for index := range indexes {
		if indexes[index].Name == name {
			return &indexes[index]
		}
	}
	return nil
}

func runtimeAppIndexCollation(t *testing.T, ctx context.Context, collection *mongo.Collection, name string) *options.Collation {
	t.Helper()
	cursor, err := collection.Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list App index definitions: %v", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var index struct {
			Name      string             `bson:"name"`
			Collation *options.Collation `bson:"collation"`
		}
		if err := cursor.Decode(&index); err != nil {
			t.Fatalf("decode App index definition: %v", err)
		}
		if index.Name == name {
			return index.Collation
		}
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("iterate App index definitions: %v", err)
	}
	return nil
}

func equalRuntimeAppIndexKeys(left, right bson.D) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Key != right[index].Key || runtimeAppIndexDirection(left[index].Value) != runtimeAppIndexDirection(right[index].Value) {
			return false
		}
	}
	return true
}

func runtimeAppIndexDirection(value any) int64 {
	switch direction := value.(type) {
	case int:
		return int64(direction)
	case int32:
		return int64(direction)
	case int64:
		return direction
	default:
		return 0
	}
}
