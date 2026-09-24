package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestDecodeManifestRejectsUnknownFields(t *testing.T) {
	_, err := decodeManifest(strings.NewReader(`{
		"catalog": {
			"version": "polarstar.image.v1",
			"source_version": "catalog-2026-09-23",
			"published_at": "2026-09-23T00:00:00Z",
			"models": [],
			"unexpected": true
		},
		"recipes": []
	}`))
	if err == nil {
		t.Fatal("unknown manifest fields must be rejected")
	}
}

func TestValidateManifestRejectsDuplicateRecipeTuple(t *testing.T) {
	candidate := validManifest()
	candidate.Recipes = append(candidate.Recipes, candidate.Recipes[0])
	if _, err := validateManifest(candidate); err == nil {
		t.Fatal("duplicate template_id/template_version/atom must be rejected")
	}
}

func TestValidateManifestRejectsDuplicateProductKey(t *testing.T) {
	candidate := validManifest()
	candidate.Catalog.Models = append(candidate.Catalog.Models, candidate.Catalog.Models[0])
	if _, err := validateManifest(candidate); err == nil {
		t.Fatal("duplicate product_key must be rejected")
	}
}

func TestValidateManifestRejectsRecipeThatCannotBecomePublicRequest(t *testing.T) {
	candidate := validManifest()
	candidate.Recipes[0].Input = json.RawMessage(`{"prompt":"base","privateWorkflow":"never"}`)
	if _, err := validateManifest(candidate); err == nil {
		t.Fatal("recipe with a private input field must be rejected before publication")
	}
}

func TestValidateManifestBuildsImmutableRuntimeDocuments(t *testing.T) {
	plan, err := validateManifest(validManifest())
	if err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	if plan.ManifestSHA256 == "" || plan.Catalog.ID != "polarstar.image.v1" || len(plan.Recipes) != 1 {
		t.Fatalf("unexpected import plan: %#v", plan)
	}
	if plan.Catalog.Status != "" || plan.Recipes[0].Status != "" {
		t.Fatalf("status must be chosen by the explicit write operation: %#v", plan)
	}
}

func TestValidateManifestDigestIgnoresRecipeOrder(t *testing.T) {
	first := validManifest()
	secondRecipe := first.Recipes[0]
	secondRecipe.TemplateID = "template-image-second"
	first.Recipes = append(first.Recipes, secondRecipe)
	second := first
	second.Recipes[0], second.Recipes[1] = second.Recipes[1], second.Recipes[0]

	firstPlan, err := validateManifest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := validateManifest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstPlan.ManifestSHA256 != secondPlan.ManifestSHA256 {
		t.Fatalf("recipe ordering changed manifest digest: %s != %s", firstPlan.ManifestSHA256, secondPlan.ManifestSHA256)
	}
	if !sameRecipeDocument(recipeRecordFromDocument(firstPlan.Recipes[0], "draft", firstPlan.ManifestSHA256), secondPlan.Recipes[0]) ||
		!sameRecipeDocument(recipeRecordFromDocument(firstPlan.Recipes[1], "draft", firstPlan.ManifestSHA256), secondPlan.Recipes[1]) {
		t.Fatalf("recipes were not sorted to one stable order: %#v / %#v", firstPlan.Recipes, secondPlan.Recipes)
	}
}

func TestRunDefaultsToDryRunWithoutLoadingMongoConfiguration(t *testing.T) {
	payload, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--manifest", "-", "--conf", "/definitely/missing/config.yaml"},
		bytes.NewReader(payload), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dry-run 通过") || !strings.Contains(stdout.String(), "未连接或写入 MongoDB") {
		t.Fatalf("dry-run output = %q", stdout.String())
	}
}

func TestRunRejectsTwoWriteModesBeforeOpeningMongo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(
		[]string{"--manifest", "-", "--apply-draft", "--publish"},
		strings.NewReader("{}"), &stdout, &stderr,
	)
	if code != 2 || !strings.Contains(stderr.String(), "互斥") {
		t.Fatalf("conflicting write modes = code %d stderr %q", code, stderr.String())
	}
}

func TestControlPlaneRecordsDecodeAsRuntimeModels(t *testing.T) {
	plan, err := validateManifest(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	catalogRaw, err := bson.Marshal(catalogRecordFromDocument(plan.Catalog, "draft", plan.ManifestSHA256))
	if err != nil {
		t.Fatal(err)
	}
	var catalog model.MappingCatalogDocument
	if err := bson.Unmarshal(catalogRaw, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Status != "draft" || catalog.ID != plan.Catalog.ID || !sameCatalogDocument(catalogRecordFromDocument(catalog, catalog.Status, plan.ManifestSHA256), plan.Catalog) {
		t.Fatalf("catalog runtime decode = %#v", catalog)
	}

	recipeRaw, err := bson.Marshal(recipeRecordFromDocument(plan.Recipes[0], "draft", plan.ManifestSHA256))
	if err != nil {
		t.Fatal(err)
	}
	var recipe model.GenerationProductRecipeDocument
	if err := bson.Unmarshal(recipeRaw, &recipe); err != nil {
		t.Fatal(err)
	}
	if recipe.Status != "draft" || !sameRecipeDocument(recipeRecordFromDocument(recipe, recipe.Status, plan.ManifestSHA256), plan.Recipes[0]) {
		t.Fatalf("recipe runtime decode = %#v", recipe)
	}
}

func TestApplyManifestDraftThenPublishIsImmutableAndIdempotent(t *testing.T) {
	database := isolatedCatalogMongoDatabase(t)
	plan, err := validateManifest(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	draft, err := applyManifest(ctx, database, plan, writeDraft)
	if err != nil || draft.Created != 2 || draft.Published != 0 {
		t.Fatalf("apply draft = %#v, %v", draft, err)
	}
	if got := catalogStatus(t, ctx, database, plan.Catalog.ID); got != "draft" {
		t.Fatalf("catalog status after draft = %q", got)
	}
	if got := recipeStatus(t, ctx, database, plan.Recipes[0]); got != "draft" {
		t.Fatalf("recipe status after draft = %q", got)
	}

	replayedDraft, err := applyManifest(ctx, database, plan, writeDraft)
	if err != nil || replayedDraft.Skipped != 2 {
		t.Fatalf("replayed draft = %#v, %v", replayedDraft, err)
	}
	published, err := applyManifest(ctx, database, plan, writePublish)
	if err != nil || published.Published != 2 {
		t.Fatalf("publish draft = %#v, %v", published, err)
	}
	if got := catalogStatus(t, ctx, database, plan.Catalog.ID); got != "published" {
		t.Fatalf("catalog status after publish = %q", got)
	}
	if got := recipeStatus(t, ctx, database, plan.Recipes[0]); got != "published" {
		t.Fatalf("recipe status after publish = %q", got)
	}
	replayedPublish, err := applyManifest(ctx, database, plan, writePublish)
	if err != nil || replayedPublish.Skipped != 2 {
		t.Fatalf("replayed publish = %#v, %v", replayedPublish, err)
	}

	changed := validManifest()
	changed.Recipes[0].Input = json.RawMessage(`{"prompt":"changed","aspectRatio":"1:1"}`)
	changedPlan, err := validateManifest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyManifest(ctx, database, changedPlan, writeDraft); err == nil {
		t.Fatal("different content may not overwrite a published immutable recipe")
	}
	if _, err := applyManifest(ctx, database, changedPlan, writePublish); err == nil {
		t.Fatal("different content may not overwrite a published immutable recipe")
	}
}

func TestApplyManifestPublishRequiresExactDraftDigest(t *testing.T) {
	database := isolatedCatalogMongoDatabase(t)
	plan, err := validateManifest(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := applyManifest(ctx, database, plan, writeDraft); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Collection(schema.CollectionGenerationModelMappings).UpdateOne(ctx,
		bson.D{{Key: "_id", Value: plan.Catalog.ID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "manifest_sha256", Value: "different"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyManifest(ctx, database, plan, writePublish); err == nil {
		t.Fatal("publish must compare-and-set the exact draft digest")
	}
	if got := catalogStatus(t, ctx, database, plan.Catalog.ID); got != "draft" {
		t.Fatalf("mismatched draft must remain draft, got %q", got)
	}
}

func TestPublishedCatalogRejectsRecipeAppendDraftAndMissingReplay(t *testing.T) {
	database := isolatedCatalogMongoDatabase(t)
	plan, err := validateManifest(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := applyManifest(ctx, database, plan, writeDraft); err != nil {
		t.Fatal(err)
	}
	if _, err := applyManifest(ctx, database, plan, writePublish); err != nil {
		t.Fatal(err)
	}

	appended := validManifest()
	second := appended.Recipes[0]
	second.TemplateID = "template-image-appended"
	appended.Recipes = append(appended.Recipes, second)
	appendedPlan, err := validateManifest(appended)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyManifest(ctx, database, appendedPlan, writeDraft); err == nil {
		t.Fatal("published catalog must reject an append manifest")
	}
	count, err := database.Collection(schema.CollectionGenerationProductRecipes).CountDocuments(ctx, bson.D{})
	if err != nil || count != 1 {
		t.Fatalf("append attempt changed recipes: count=%d err=%v", count, err)
	}

	filter := bson.D{{Key: "template_id", Value: plan.Recipes[0].TemplateID}, {Key: "template_version", Value: plan.Recipes[0].TemplateVersion}, {Key: "atom", Value: plan.Recipes[0].Atom}}
	if _, err := database.Collection(schema.CollectionGenerationProductRecipes).UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "draft"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyManifest(ctx, database, plan, writePublish); err == nil {
		t.Fatal("published catalog must reject a draft recipe")
	}
	if _, err := database.Collection(schema.CollectionGenerationProductRecipes).DeleteOne(ctx, filter); err != nil {
		t.Fatal(err)
	}
	if _, err := applyManifest(ctx, database, plan, writePublish); err == nil {
		t.Fatal("published catalog must reject a missing recipe")
	}
}

func isolatedCatalogMongoDatabase(t *testing.T) *mongo.Database {
	t.Helper()
	uri := strings.TrimSpace(os.Getenv("CLING_TEST_MONGO_URI"))
	if uri == "" {
		t.Skip("set CLING_TEST_MONGO_URI to run the isolated catalog import Mongo tests")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect isolated MongoDB: %v", err)
	}
	database := client.Database("catalog_import_" + uuid.NewString())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		_ = client.Disconnect(context.Background())
		t.Fatalf("initialize isolated schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if err := database.Drop(cleanupContext); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		_ = client.Disconnect(cleanupContext)
	})
	return database
}

func catalogStatus(t *testing.T, ctx context.Context, database *mongo.Database, id string) string {
	t.Helper()
	var document model.MappingCatalogDocument
	if err := database.Collection(schema.CollectionGenerationModelMappings).FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&document); err != nil {
		t.Fatalf("read catalog %q: %v", id, err)
	}
	return document.Status
}

func recipeStatus(t *testing.T, ctx context.Context, database *mongo.Database, recipe model.GenerationProductRecipeDocument) string {
	t.Helper()
	var document model.GenerationProductRecipeDocument
	if err := database.Collection(schema.CollectionGenerationProductRecipes).FindOne(ctx, bson.D{
		{Key: "template_id", Value: recipe.TemplateID},
		{Key: "template_version", Value: recipe.TemplateVersion},
		{Key: "atom", Value: recipe.Atom},
	}).Decode(&document); err != nil {
		t.Fatalf("read recipe: %v", err)
	}
	return document.Status
}

func validManifest() manifest {
	return manifest{
		Catalog: catalogManifest{
			Version: "polarstar.image.v1", SourceVersion: "catalog-2026-09-23",
			PublishedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC),
			Models: []modelManifest{{
				Capability: "text_to_image", ProductKey: "image", PublicModel: "ps-image-v1",
				AllowedInputs: []string{"prompt", "aspectRatio", "seed"},
				Sizes:         []sizeManifest{{Width: 1024, Height: 1024}},
				AspectRatios:  []string{"1:1", "16:9"}, Enabled: true,
			}},
		},
		Recipes: []recipeManifest{{
			TemplateID: "template-image", TemplateVersion: 7, Atom: "text_to_image", ProductKey: "image",
			Input:             json.RawMessage(`{"prompt":"base","aspectRatio":"1:1"}`),
			AllowedUserInputs: []string{"prompt", "aspectRatio"}, PromptUserInputMode: "replace",
		}},
	}
}
