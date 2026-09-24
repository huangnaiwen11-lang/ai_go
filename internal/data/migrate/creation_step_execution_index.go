package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var (
	// ErrCreationStepExecutionIndexPreflight means historical job documents do
	// not yet have a valid, unambiguous frozen route. A caller must backfill and
	// re-run the read-only preflight rather than installing a partial index that
	// silently leaves those jobs outside its scope.
	ErrCreationStepExecutionIndexPreflight = errors.New("creation-step execution index preflight failed")
	// ErrLegacyCreationStepExecutionIndexRetirementUnconfirmed prevents a
	// destructive index drop until the operator has separately confirmed that
	// all writers understand the scoped index contract.
	ErrLegacyCreationStepExecutionIndexRetirementUnconfirmed = errors.New("legacy creation-step execution index retirement is not confirmed")
	// ErrCreationStepScopedExecutionIndexMissing means the old global index
	// cannot be retired because its replacement has not been verified.
	ErrCreationStepScopedExecutionIndexMissing = errors.New("scoped creation-step execution index is missing or incompatible")
)

// CreationStepExecutionIndexPreflight is deliberately aggregate-only: it
// reports counts and never includes provider job IDs or account values, so a
// dry run is safe to capture in deployment evidence.
type CreationStepExecutionIndexPreflight struct {
	DocumentsWithJob         int64
	InvalidRouteDocuments    int64
	DuplicateScopedJobGroups int64
}

// Ready reports whether it is safe to create the scoped unique index. It does
// not authorize retirement of the legacy global index; that requires a
// separate explicit confirmation after compatible writers are deployed.
func (report CreationStepExecutionIndexPreflight) Ready() bool {
	return report.InvalidRouteDocuments == 0 && report.DuplicateScopedJobGroups == 0
}

// CreationStepExecutionIndexMigration is an explicit deployment migration. It
// is never invoked by Initializer.Ensure, which may run in ordinary service
// processes and must remain non-destructive.
type CreationStepExecutionIndexMigration struct {
	steps *mongo.Collection
}

// NewCreationStepExecutionIndexMigration constructs the controlled migration
// against one already-selected database. Nil is retained as an invalid
// migration object so callers get a deterministic error instead of a panic.
func NewCreationStepExecutionIndexMigration(database *mongo.Database) *CreationStepExecutionIndexMigration {
	if database == nil {
		return &CreationStepExecutionIndexMigration{}
	}
	return &CreationStepExecutionIndexMigration{steps: database.Collection(schema.CollectionCreationSteps)}
}

// Preflight scans only job-bearing creation steps, validates their fully
// frozen routes, and groups potential scoped uniqueness collisions on Mongo.
// It does not write documents or create/drop any index.
func (migration *CreationStepExecutionIndexMigration) Preflight(ctx context.Context) (CreationStepExecutionIndexPreflight, error) {
	if migration == nil || migration.steps == nil {
		return CreationStepExecutionIndexPreflight{}, errors.New("creation-step execution index migration is not configured")
	}
	jobs, err := migration.steps.CountDocuments(ctx, bson.D{{Key: "external_execution_id", Value: bson.D{{Key: "$exists", Value: true}}}})
	if err != nil {
		return CreationStepExecutionIndexPreflight{}, fmt.Errorf("count creation-step jobs for index preflight: %w", err)
	}
	report := CreationStepExecutionIndexPreflight{DocumentsWithJob: jobs}
	cursor, err := migration.steps.Find(ctx, bson.D{{Key: "external_execution_id", Value: bson.D{{Key: "$exists", Value: true}}}})
	if err != nil {
		return CreationStepExecutionIndexPreflight{}, fmt.Errorf("scan creation-step jobs for index preflight: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var document bson.M
		if err := cursor.Decode(&document); err != nil {
			return CreationStepExecutionIndexPreflight{}, fmt.Errorf("decode creation-step job for index preflight: %w", err)
		}
		if !validCreationStepScopedJob(document) {
			report.InvalidRouteDocuments++
		}
	}
	if err := cursor.Err(); err != nil {
		return CreationStepExecutionIndexPreflight{}, fmt.Errorf("iterate creation-step jobs for index preflight: %w", err)
	}

	duplicates, err := migration.duplicateScopedJobGroups(ctx)
	if err != nil {
		return CreationStepExecutionIndexPreflight{}, err
	}
	report.DuplicateScopedJobGroups = duplicates
	return report, nil
}

// EnsureScopedUnique creates the scoped replacement only after preflight is
// clean. It intentionally retains the legacy global unique index when present
// so existing writers cannot be broadened by an ordinary rollout.
func (migration *CreationStepExecutionIndexMigration) EnsureScopedUnique(ctx context.Context) (CreationStepExecutionIndexPreflight, error) {
	report, err := migration.Preflight(ctx)
	if err != nil {
		return CreationStepExecutionIndexPreflight{}, err
	}
	if !report.Ready() {
		return report, ErrCreationStepExecutionIndexPreflight
	}
	spec := schema.CreationStepsScopedExternalExecutionIndex()
	indexOptions := options.Index().SetName(spec.Name).SetUnique(true).SetPartialFilterExpression(spec.PartialFilter)
	if _, err := migration.steps.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: spec.Keys, Options: indexOptions}); err != nil {
		return report, fmt.Errorf("create scoped creation-step execution index: %w", err)
	}
	return report, nil
}

// RetireLegacyGlobalUnique removes the old external_execution_id-only unique
// index only when all historic jobs pass preflight, the exact replacement is
// installed, and the caller explicitly confirms compatible writers. This is
// intentionally a separate, opt-in operation from EnsureScopedUnique.
func (migration *CreationStepExecutionIndexMigration) RetireLegacyGlobalUnique(ctx context.Context, writersConfirmed bool) (CreationStepExecutionIndexPreflight, error) {
	report, err := migration.Preflight(ctx)
	if err != nil {
		return CreationStepExecutionIndexPreflight{}, err
	}
	if !report.Ready() {
		return report, ErrCreationStepExecutionIndexPreflight
	}
	if !writersConfirmed {
		return report, ErrLegacyCreationStepExecutionIndexRetirementUnconfirmed
	}
	installed, err := migration.scopedIndexInstalled(ctx)
	if err != nil {
		return report, err
	}
	if !installed {
		return report, ErrCreationStepScopedExecutionIndexMissing
	}
	legacyExists, err := migration.indexNamed(ctx, schema.LegacyCreationStepsExternalExecutionUniqueIndex)
	if err != nil {
		return report, err
	}
	if !legacyExists {
		return report, nil
	}
	if err := migration.steps.Indexes().DropOne(ctx, schema.LegacyCreationStepsExternalExecutionUniqueIndex); err != nil {
		return report, fmt.Errorf("drop legacy global creation-step execution index: %w", err)
	}
	return report, nil
}

func (migration *CreationStepExecutionIndexMigration) duplicateScopedJobGroups(ctx context.Context) (int64, error) {
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{
			{Key: "provider", Value: bson.D{{Key: "$type", Value: "string"}}},
			{Key: "account_ref", Value: bson.D{{Key: "$type", Value: "string"}}},
			{Key: "external_execution_id", Value: bson.D{{Key: "$type", Value: "string"}}},
		}}},
		bson.D{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: bson.D{{Key: "provider", Value: "$provider"}, {Key: "account_ref", Value: "$account_ref"}, {Key: "external_execution_id", Value: "$external_execution_id"}}},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
		bson.D{{Key: "$match", Value: bson.D{{Key: "count", Value: bson.D{{Key: "$gt", Value: 1}}}}}},
		bson.D{{Key: "$count", Value: "groups"}},
	}
	cursor, err := migration.steps.Aggregate(ctx, pipeline, options.Aggregate().SetAllowDiskUse(true))
	if err != nil {
		return 0, fmt.Errorf("group duplicate scoped creation-step jobs: %w", err)
	}
	defer cursor.Close(ctx)
	if !cursor.Next(ctx) {
		if err := cursor.Err(); err != nil {
			return 0, fmt.Errorf("read duplicate scoped creation-step groups: %w", err)
		}
		return 0, nil
	}
	var result struct {
		Groups int64 `bson:"groups"`
	}
	if err := cursor.Decode(&result); err != nil {
		return 0, fmt.Errorf("decode duplicate scoped creation-step groups: %w", err)
	}
	if err := cursor.Err(); err != nil {
		return 0, fmt.Errorf("iterate duplicate scoped creation-step groups: %w", err)
	}
	return result.Groups, nil
}

func validCreationStepScopedJob(document bson.M) bool {
	job, jobOK := document["external_execution_id"].(string)
	provider, providerOK := document["provider"].(string)
	accountRef, accountRefOK := document["account_ref"].(string)
	contractVersion, contractVersionOK := document["contract_version"].(string)
	mappingVersion, mappingVersionOK := document["mapping_version"].(string)
	if !jobOK || !providerOK || !accountRefOK || !contractVersionOK || !mappingVersionOK ||
		!validExternalExecutionID(job) {
		return false
	}
	_, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{
		Provider: provider, AccountRef: accountRef, ContractVersion: contractVersion, MappingVersion: mappingVersion,
	})
	return err == nil
}

func validExternalExecutionID(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= 512
}

func (migration *CreationStepExecutionIndexMigration) scopedIndexInstalled(ctx context.Context) (bool, error) {
	spec := schema.CreationStepsScopedExternalExecutionIndex()
	cursor, err := migration.steps.Indexes().List(ctx)
	if err != nil {
		return false, fmt.Errorf("list creation-step indexes: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var index bson.M
		if err := cursor.Decode(&index); err != nil {
			return false, fmt.Errorf("decode creation-step index: %w", err)
		}
		if name, _ := index["name"].(string); name == spec.Name {
			return index["unique"] == true && scopedIndexKeysMatch(index["key"], spec.Keys) && scopedIndexPartialFilterMatches(index["partialFilterExpression"]), nil
		}
	}
	if err := cursor.Err(); err != nil {
		return false, fmt.Errorf("iterate creation-step indexes: %w", err)
	}
	return false, nil
}

func scopedIndexKeysMatch(value any, expected bson.D) bool {
	keys, ok := value.(bson.D)
	if !ok || len(keys) != len(expected) {
		return false
	}
	for index := range expected {
		if keys[index].Key != expected[index].Key || indexDirection(keys[index].Value) != indexDirection(expected[index].Value) {
			return false
		}
	}
	return true
}

func indexDirection(value any) int64 {
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

func scopedIndexPartialFilterMatches(value any) bool {
	filter, ok := value.(bson.D)
	if !ok || len(filter) != 3 {
		return false
	}
	for _, field := range []string{"provider", "account_ref", "external_execution_id"} {
		matched := false
		for _, element := range filter {
			if element.Key == field && typeStringConstraint(element.Value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func typeStringConstraint(value any) bool {
	switch constraint := value.(type) {
	case bson.D:
		return len(constraint) == 1 && constraint[0].Key == "$type" && constraint[0].Value == "string"
	case bson.M:
		return len(constraint) == 1 && constraint["$type"] == "string"
	default:
		return false
	}
}

func (migration *CreationStepExecutionIndexMigration) indexNamed(ctx context.Context, name string) (bool, error) {
	cursor, err := migration.steps.Indexes().List(ctx)
	if err != nil {
		return false, fmt.Errorf("list creation-step indexes: %w", err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var index struct {
			Name string `bson:"name"`
		}
		if err := cursor.Decode(&index); err != nil {
			return false, fmt.Errorf("decode creation-step index name: %w", err)
		}
		if index.Name == name {
			return true, nil
		}
	}
	if err := cursor.Err(); err != nil {
		return false, fmt.Errorf("iterate creation-step index names: %w", err)
	}
	return false, nil
}
