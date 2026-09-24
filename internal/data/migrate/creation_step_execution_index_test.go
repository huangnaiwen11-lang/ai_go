package migrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestCreationStepExecutionIndexMigrationRejectsInvalidOrDuplicatePreflight(t *testing.T) {
	database := newLocalTestDatabase(t)
	ensureLocalSchema(t, database)
	collection := database.Collection(schema.CollectionCreationSteps)
	// Register restoration before document cleanup so cleanup executes in the
	// safe order (documents first, then recreate the scoped index) even when a
	// later assertion fails.
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := NewInitializer(database).Ensure(cleanupContext); err != nil {
			t.Errorf("restore scoped creation-step index after fixture: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := collection.Indexes().DropOne(ctx, schema.CreationStepsScopedExternalExecutionUniqueIndex); err != nil {
		t.Fatalf("drop scoped index for preflight fixture: %v", err)
	}

	documents := []bson.D{
		{{Key: "_id", Value: uuid.NewString()}, {Key: "creation_id", Value: uuid.NewString()}, {Key: "sequence", Value: 1}, {Key: "external_execution_id", Value: "missing-route"}},
		{{Key: "_id", Value: uuid.NewString()}, {Key: "creation_id", Value: uuid.NewString()}, {Key: "sequence", Value: 1}, {Key: "provider", Value: "polarstar_b2b_v2"}, {Key: "account_ref", Value: "account-a"}, {Key: "contract_version", Value: "b2b.job.v2"}, {Key: "mapping_version", Value: "mapping-1"}, {Key: "external_execution_id", Value: "duplicate-job"}},
		{{Key: "_id", Value: uuid.NewString()}, {Key: "creation_id", Value: uuid.NewString()}, {Key: "sequence", Value: 1}, {Key: "provider", Value: "polarstar_b2b_v2"}, {Key: "account_ref", Value: "account-a"}, {Key: "contract_version", Value: "b2b.job.v2"}, {Key: "mapping_version", Value: "mapping-1"}, {Key: "external_execution_id", Value: "duplicate-job"}},
	}
	ids := make(bson.A, 0, len(documents))
	for _, document := range documents {
		ids = append(ids, document[0].Value)
		if _, err := collection.InsertOne(ctx, document); err != nil {
			t.Fatalf("insert preflight fixture: %v", err)
		}
	}
	cleanupDocumentsByID(t, collection, ids)

	report, err := NewCreationStepExecutionIndexMigration(database).Preflight(ctx)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if report.DocumentsWithJob != 3 || report.InvalidRouteDocuments != 1 || report.DuplicateScopedJobGroups != 1 || report.Ready() {
		t.Fatalf("preflight report = %#v", report)
	}
	if _, err := NewCreationStepExecutionIndexMigration(database).EnsureScopedUnique(ctx); !errors.Is(err, ErrCreationStepExecutionIndexPreflight) {
		t.Fatalf("unsafe scoped index creation error = %v, want preflight error", err)
	}
}

func TestCreationStepExecutionIndexMigrationRetiresLegacyOnlyAfterExplicitConfirmation(t *testing.T) {
	database := newLocalTestDatabase(t)
	ensureLocalSchema(t, database)
	collection := database.Collection(schema.CollectionCreationSteps)
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := collection.Indexes().DropOne(cleanupContext, schema.LegacyCreationStepsExternalExecutionUniqueIndex); err != nil {
			var commandError mongo.CommandError
			if !errors.As(err, &commandError) || commandError.Code != 27 {
				t.Errorf("remove legacy global index after fixture: %v", err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "external_execution_id", Value: 1}},
		Options: options.Index().SetName(schema.LegacyCreationStepsExternalExecutionUniqueIndex).SetUnique(true).SetSparse(true),
	}); err != nil {
		t.Fatalf("create legacy global index: %v", err)
	}
	firstID, secondID, duplicateID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cleanupDocumentsByID(t, collection, bson.A{firstID, secondID, duplicateID})
	if _, err := collection.InsertOne(ctx, bson.D{{Key: "_id", Value: firstID}, {Key: "creation_id", Value: uuid.NewString()}, {Key: "sequence", Value: 1}, {Key: "provider", Value: "polarstar_b2b_v2"}, {Key: "account_ref", Value: "account-a"}, {Key: "contract_version", Value: "b2b.job.v2"}, {Key: "mapping_version", Value: "mapping-1"}, {Key: "external_execution_id", Value: "job-a"}}); err != nil {
		t.Fatalf("insert scoped job: %v", err)
	}

	migration := NewCreationStepExecutionIndexMigration(database)
	report, err := migration.EnsureScopedUnique(ctx)
	if err != nil || !report.Ready() {
		t.Fatalf("ensure scoped index = %#v / %v", report, err)
	}
	if !creationStepIndexExists(t, ctx, collection, schema.LegacyCreationStepsExternalExecutionUniqueIndex) || !creationStepIndexExists(t, ctx, collection, schema.CreationStepsScopedExternalExecutionUniqueIndex) {
		t.Fatal("ensure scoped index changed legacy index or failed to retain scoped index")
	}
	if _, err := migration.RetireLegacyGlobalUnique(ctx, false); !errors.Is(err, ErrLegacyCreationStepExecutionIndexRetirementUnconfirmed) {
		t.Fatalf("unconfirmed legacy retirement error = %v", err)
	}
	if !creationStepIndexExists(t, ctx, collection, schema.LegacyCreationStepsExternalExecutionUniqueIndex) {
		t.Fatal("unconfirmed retirement dropped legacy global index")
	}

	report, err = migration.RetireLegacyGlobalUnique(ctx, true)
	if err != nil || !report.Ready() {
		t.Fatalf("retire legacy index = %#v / %v", report, err)
	}
	if creationStepIndexExists(t, ctx, collection, schema.LegacyCreationStepsExternalExecutionUniqueIndex) || !creationStepIndexExists(t, ctx, collection, schema.CreationStepsScopedExternalExecutionUniqueIndex) {
		t.Fatal("legacy/scoped index state after explicit retirement is incorrect")
	}
	if _, err := collection.InsertOne(ctx, bson.D{{Key: "_id", Value: secondID}, {Key: "creation_id", Value: uuid.NewString()}, {Key: "sequence", Value: 1}, {Key: "provider", Value: "polarstar_b2b_v2"}, {Key: "account_ref", Value: "account-b"}, {Key: "contract_version", Value: "b2b.job.v2"}, {Key: "mapping_version", Value: "mapping-1"}, {Key: "external_execution_id", Value: "job-a"}}); err != nil {
		t.Fatalf("same job in another account after retirement: %v", err)
	}
	if _, err := collection.InsertOne(ctx, bson.D{{Key: "_id", Value: duplicateID}, {Key: "creation_id", Value: uuid.NewString()}, {Key: "sequence", Value: 1}, {Key: "provider", Value: "polarstar_b2b_v2"}, {Key: "account_ref", Value: "account-a"}, {Key: "contract_version", Value: "b2b.job.v2"}, {Key: "mapping_version", Value: "mapping-1"}, {Key: "external_execution_id", Value: "job-a"}}); !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("exact scoped duplicate error = %v, want duplicate key", err)
	}
}

func creationStepIndexExists(t *testing.T, ctx context.Context, collection *mongo.Collection, name string) bool {
	t.Helper()
	specifications, err := collection.Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("list creation-step indexes: %v", err)
	}
	for _, specification := range specifications {
		if specification.Name == name {
			return true
		}
	}
	return false
}
