// Command b2b-catalog-import imports reviewed PolarStar B2B recipes and model
// mappings. It is deliberately a control-plane command: dry-run is the
// default and publishing never rewrites an already published version.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"ai-business-service/internal/biz/creations"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"
	generationintegration "ai-business-service/internal/integrations/generation"
	"ai-business-service/internal/integrations/polarstarb2b"

	kratosconfig "github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	defaultConfigPath    = "/data/conf/config.yaml"
	catalogImportTimeout = 30 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("b2b-catalog-import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "JSON manifest path（必填；- 表示 stdin）")
	configPath := flags.String("conf", defaultConfigPath, "运行库配置路径（仅写入时使用）")
	applyDraft := flags.Bool("apply-draft", false, "将经校验的 manifest 作为不可变 draft 写入运行库")
	publish := flags.Bool("publish", false, "仅把同一 manifest 的原样 draft 条件发布为 published")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if strings.TrimSpace(*manifestPath) == "" {
		fmt.Fprintln(stderr, "必须显式提供 --manifest。")
		return 2
	}
	if *applyDraft && *publish {
		fmt.Fprintln(stderr, "--apply-draft 与 --publish 互斥。")
		return 2
	}

	reader, closeManifest, err := openManifest(*manifestPath, stdin)
	if err != nil {
		fmt.Fprintln(stderr, "打开 manifest 失败：", err)
		return 1
	}
	defer closeManifest()
	decoded, err := decodeManifest(reader)
	if err != nil {
		fmt.Fprintln(stderr, "解析 manifest 失败：", err)
		return 1
	}
	plan, err := validateManifest(decoded)
	if err != nil {
		fmt.Fprintln(stderr, "拒绝 manifest：", err)
		return 1
	}

	if !*applyDraft && !*publish {
		fmt.Fprintf(stdout, "dry-run 通过：catalog=%s recipes=%d sha256=%s；未连接或写入 MongoDB。\n", plan.Catalog.ID, len(plan.Recipes), plan.ManifestSHA256)
		return 0
	}

	bootstrap, closeConfig, err := loadBootstrap(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "读取运行库配置失败：", err)
		return 1
	}
	defer closeConfig()
	database, disconnect, err := openConfiguredDatabase(bootstrap)
	if err != nil {
		fmt.Fprintln(stderr, "连接运行库失败：", err)
		return 1
	}
	defer disconnect()

	ctx, cancel := context.WithTimeout(context.Background(), catalogImportTimeout)
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		fmt.Fprintln(stderr, "初始化运行库 schema 失败：", err)
		return 1
	}
	mode := writeDraft
	if *publish {
		mode = writePublish
	}
	report, err := applyManifest(ctx, database, plan, mode)
	if err != nil {
		fmt.Fprintln(stderr, "导入失败：", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s 完成：catalog=%s recipes=%d created=%d published=%d skipped=%d sha256=%s\n", mode, plan.Catalog.ID, len(plan.Recipes), report.Created, report.Published, report.Skipped, plan.ManifestSHA256)
	return 0
}

func openManifest(path string, stdin io.Reader) (io.Reader, func(), error) {
	if path == "-" {
		if stdin == nil {
			return nil, nil, errors.New("stdin is not configured")
		}
		return stdin, func() {}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return file, func() { _ = file.Close() }, nil
}

func loadBootstrap(path string) (*conf.Bootstrap, func(), error) {
	configuration := kratosconfig.New(kratosconfig.WithSource(file.NewSource(path), env.NewSource("KRATOS")))
	if err := configuration.Load(); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("加载本地配置: %w", err)
	}
	bootstrap := &conf.Bootstrap{}
	if err := configuration.Scan(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("解析本地配置: %w", err)
	}
	if err := conf.ApplyMongoEnvironmentOverrides(bootstrap, os.Getenv); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("应用 Mongo 环境覆盖: %w", err)
	}
	if err := conf.Validate(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, fmt.Errorf("校验本地配置: %w", err)
	}
	return bootstrap, func() { _ = configuration.Close() }, nil
}

func openConfiguredDatabase(bootstrap *conf.Bootstrap) (*mongo.Database, func(), error) {
	if bootstrap == nil || bootstrap.GetData() == nil || bootstrap.GetData().GetMongo() == nil {
		return nil, nil, errors.New("MongoDB configuration is required")
	}
	mongoConfig := bootstrap.GetData().GetMongo()
	client, err := mongo.Connect(options.Client().ApplyURI(mongoConfig.GetUri()))
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), catalogImportTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, err
	}
	return client.Database(mongoConfig.GetDatabase()), func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), catalogImportTimeout)
		defer closeCancel()
		_ = client.Disconnect(closeContext)
	}, nil
}

type manifest struct {
	Catalog catalogManifest  `json:"catalog"`
	Recipes []recipeManifest `json:"recipes"`
}

type catalogManifest struct {
	Version       string          `json:"version"`
	SourceVersion string          `json:"source_version"`
	PublishedAt   time.Time       `json:"published_at"`
	Models        []modelManifest `json:"models"`
}

type modelManifest struct {
	Capability    string             `json:"capability"`
	ProductKey    string             `json:"product_key"`
	PublicModel   string             `json:"public_model"`
	AllowedInputs []string           `json:"allowed_inputs"`
	Sizes         []sizeManifest     `json:"sizes"`
	AspectRatios  []string           `json:"aspect_ratios"`
	Durations     []int32            `json:"durations"`
	Templates     []templateManifest `json:"templates"`
	Enabled       bool               `json:"enabled"`
}

type sizeManifest struct {
	Width  int32 `json:"width"`
	Height int32 `json:"height"`
}

type templateManifest struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

type recipeManifest struct {
	TemplateID          string          `json:"template_id"`
	TemplateVersion     int64           `json:"template_version"`
	Atom                string          `json:"atom"`
	ProductKey          string          `json:"product_key"`
	TemplateKey         string          `json:"template_key"`
	Input               json.RawMessage `json:"input"`
	Assets              []assetManifest `json:"assets"`
	AllowedUserInputs   []string        `json:"allowed_user_inputs"`
	PromptUserInputMode string          `json:"prompt_user_input_mode"`
}

type assetManifest struct {
	Role string `json:"role"`
	URL  string `json:"url"`
}

func decodeManifest(reader io.Reader) (manifest, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var decoded manifest
	if err := decoder.Decode(&decoded); err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return manifest{}, fmt.Errorf("decode manifest: trailing JSON value")
		}
		return manifest{}, fmt.Errorf("decode manifest trailing value: %w", err)
	}
	return decoded, nil
}

// importPlan deliberately has no caller-controlled status. apply-draft and
// publish are the only code paths permitted to choose one.
type importPlan struct {
	ManifestSHA256 string
	Catalog        model.MappingCatalogDocument
	Recipes        []model.GenerationProductRecipeDocument
}

func validateManifest(source manifest) (importPlan, error) {
	catalog, normalized, err := catalogFromManifest(source.Catalog)
	if err != nil {
		return importPlan{}, err
	}
	// A catalog version becomes execution_route.mapping_version at admission, so
	// its validity must be checked before it can be published.
	if _, err := creations.NormalizeExecutionRoute(creations.ExecutionRoute{
		Provider:        creations.PolarStarB2BProvider,
		AccountRef:      "catalog-import",
		ContractVersion: creations.B2BContractVersion,
		MappingVersion:  normalized.Version,
	}); err != nil {
		return importPlan{}, fmt.Errorf("catalog version cannot be used as a B2B route identity: %w", err)
	}
	snapshot, err := generationintegration.MappingSnapshotFromCatalog(normalized, normalized.Version)
	if err != nil {
		return importPlan{}, fmt.Errorf("validate catalog public mapping: %w", err)
	}

	seenRecipes := make(map[string]struct{}, len(source.Recipes))
	recipes := make([]model.GenerationProductRecipeDocument, 0, len(source.Recipes))
	for index, raw := range source.Recipes {
		key := recipeTupleKey(raw.TemplateID, raw.TemplateVersion, raw.Atom)
		if _, exists := seenRecipes[key]; exists {
			return importPlan{}, fmt.Errorf("recipes[%d]: duplicate template_id/template_version/atom", index)
		}
		seenRecipes[key] = struct{}{}

		document, published, err := recipeFromManifest(raw)
		if err != nil {
			return importPlan{}, fmt.Errorf("recipes[%d]: %w", index, err)
		}
		entry, exists := normalized.Entry(document.ProductKey)
		if !exists || !entry.Enabled || entry.Capability != document.Atom {
			return importPlan{}, fmt.Errorf("recipes[%d]: product_key does not resolve to an enabled matching capability", index)
		}
		if document.TemplateKey != "" {
			if _, exists := entry.Template(document.TemplateKey); !exists {
				return importPlan{}, fmt.Errorf("recipes[%d]: template_key is absent from the product mapping", index)
			}
		}
		if !recipeAllowedInputsAreMapped(document.AllowedUserInputs, entry.AllowedInputs) {
			return importPlan{}, fmt.Errorf("recipes[%d]: allowed_user_inputs exceeds public mapping allowlist", index)
		}

		product, err := generationintegration.ProductInputFromRecipe(creations.B2BProductRecipe{
			ProductKey: document.ProductKey, TemplateKey: document.TemplateKey,
			Input: published.Input, Assets: published.Assets,
		}, document.Atom)
		if err != nil {
			return importPlan{}, fmt.Errorf("recipes[%d]: decode public product input: %w", index, err)
		}
		if document.Atom == string(creations.AtomImageToVideo) {
			if polarstarb2b.ValidateProduct(snapshot, product) != nil && polarstarb2b.ValidateDeferredImageToVideoProduct(snapshot, product) != nil {
				return importPlan{}, fmt.Errorf("recipes[%d]: invalid image_to_video public product", index)
			}
		} else if err := polarstarb2b.ValidateProduct(snapshot, product); err != nil {
			return importPlan{}, fmt.Errorf("recipes[%d]: invalid public product: %w", index, err)
		}
		recipes = append(recipes, document)
	}
	if len(recipes) == 0 {
		return importPlan{}, fmt.Errorf("manifest must contain at least one recipe")
	}
	// Recipe order has no runtime meaning. Sort before hashing and applying so
	// a review manifest can be reformatted without creating a false conflict.
	sort.Slice(recipes, func(left, right int) bool {
		return recipeTupleKey(recipes[left].TemplateID, recipes[left].TemplateVersion, recipes[left].Atom) <
			recipeTupleKey(recipes[right].TemplateID, recipes[right].TemplateVersion, recipes[right].Atom)
	})

	plan := importPlan{Catalog: catalog, Recipes: recipes}
	digest, err := planDigest(plan)
	if err != nil {
		return importPlan{}, fmt.Errorf("hash manifest: %w", err)
	}
	plan.ManifestSHA256 = digest
	return plan, nil
}

func catalogFromManifest(raw catalogManifest) (model.MappingCatalogDocument, bizgeneration.PublishedMappingCatalog, error) {
	entries := make([]bizgeneration.ModelMappingEntry, 0, len(raw.Models))
	for _, item := range raw.Models {
		sizes := make([]bizgeneration.ModelMappingSize, 0, len(item.Sizes))
		for _, size := range item.Sizes {
			sizes = append(sizes, bizgeneration.ModelMappingSize{Width: int(size.Width), Height: int(size.Height)})
		}
		durations := make([]int, 0, len(item.Durations))
		for _, duration := range item.Durations {
			durations = append(durations, int(duration))
		}
		templates := make([]bizgeneration.ModelMappingTemplate, 0, len(item.Templates))
		for _, template := range item.Templates {
			templates = append(templates, bizgeneration.ModelMappingTemplate{Key: template.Key, ID: template.ID})
		}
		entries = append(entries, bizgeneration.ModelMappingEntry{
			Capability: item.Capability, ProductKey: item.ProductKey, PublicModel: item.PublicModel,
			AllowedInputs: append([]string(nil), item.AllowedInputs...), Sizes: sizes,
			AspectRatios: append([]string(nil), item.AspectRatios...), Durations: durations,
			Templates: templates, Enabled: item.Enabled,
		})
	}
	normalized, err := (bizgeneration.PublishedMappingCatalog{
		Version: raw.Version, SourceVersion: raw.SourceVersion, PublishedAt: raw.PublishedAt, Entries: entries,
	}).Normalize()
	if err != nil {
		return model.MappingCatalogDocument{}, bizgeneration.PublishedMappingCatalog{}, fmt.Errorf("invalid model catalog: %w", err)
	}
	document := model.MappingCatalogDocument{
		ID: normalized.Version, SourceVersion: normalized.SourceVersion, PublishedAt: normalized.PublishedAt,
		Models: make([]model.MappingModelDocument, 0, len(normalized.Entries)),
	}
	for _, item := range normalized.Entries {
		entry := model.MappingModelDocument{
			Capability: item.Capability, ProductKey: item.ProductKey, PublicModel: item.PublicModel,
			AllowedInputs: append([]string(nil), item.AllowedInputs...), AspectRatios: append([]string(nil), item.AspectRatios...),
			Enabled: item.Enabled,
		}
		for _, size := range item.Sizes {
			entry.Sizes = append(entry.Sizes, model.MappingSizeDocument{Width: int32(size.Width), Height: int32(size.Height)})
		}
		for _, duration := range item.Durations {
			entry.Durations = append(entry.Durations, int32(duration))
		}
		for _, template := range item.Templates {
			entry.Templates = append(entry.Templates, model.MappingTemplateDocument{Key: template.Key, ID: template.ID})
		}
		document.Models = append(document.Models, entry)
	}
	return document, normalized, nil
}

func recipeFromManifest(raw recipeManifest) (model.GenerationProductRecipeDocument, creations.PublishedB2BProductRecipe, error) {
	input, err := decodeJSONObject(raw.Input)
	if err != nil {
		return model.GenerationProductRecipeDocument{}, creations.PublishedB2BProductRecipe{}, fmt.Errorf("input must be a JSON object: %w", err)
	}
	var assets []creations.B2BAsset
	var documentAssets []model.GenerationProductAssetDocument
	for _, item := range raw.Assets {
		assets = append(assets, creations.B2BAsset{Role: item.Role, URL: item.URL})
		documentAssets = append(documentAssets, model.GenerationProductAssetDocument{Role: item.Role, URL: item.URL})
	}
	published, err := (creations.PublishedB2BProductRecipe{
		TemplateID: raw.TemplateID, TemplateVersion: raw.TemplateVersion, Atom: creations.StepAtom(raw.Atom),
		ProductKey: raw.ProductKey, TemplateKey: raw.TemplateKey, Input: append(json.RawMessage(nil), raw.Input...),
		Assets: assets, AllowedUserInputs: append([]string(nil), raw.AllowedUserInputs...),
		PromptUserInputMode: creations.PromptUserInputMode(raw.PromptUserInputMode),
	}).Normalize()
	if err != nil {
		return model.GenerationProductRecipeDocument{}, creations.PublishedB2BProductRecipe{}, fmt.Errorf("invalid published recipe: %w", err)
	}
	document := model.GenerationProductRecipeDocument{
		ID:         recipeDocumentID(published.TemplateID, published.TemplateVersion, string(published.Atom)),
		TemplateID: published.TemplateID, TemplateVersion: published.TemplateVersion, Atom: string(published.Atom),
		ProductKey: published.ProductKey, TemplateKey: published.TemplateKey, Input: input, Assets: documentAssets,
		AllowedUserInputs: append([]string(nil), published.AllowedUserInputs...), PromptUserInputMode: string(published.PromptUserInputMode),
	}
	return document, published, nil
}

func decodeJSONObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("JSON value is not an object")
		}
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	return object, nil
}

func recipeAllowedInputsAreMapped(recipeInputs, mappingInputs []string) bool {
	allowed := make(map[string]struct{}, len(mappingInputs))
	for _, input := range mappingInputs {
		allowed[input] = struct{}{}
	}
	for _, input := range recipeInputs {
		if _, exists := allowed[input]; !exists {
			return false
		}
	}
	return true
}

func recipeTupleKey(templateID string, version int64, atom string) string {
	encoded, _ := json.Marshal(struct {
		TemplateID string `json:"template_id"`
		Version    int64  `json:"template_version"`
		Atom       string `json:"atom"`
	}{templateID, version, atom})
	return string(encoded)
}

func recipeDocumentID(templateID string, version int64, atom string) string {
	sum := sha256.Sum256([]byte(recipeTupleKey(templateID, version, atom)))
	return "b2b_recipe:" + hex.EncodeToString(sum[:])
}

func planDigest(plan importPlan) (string, error) {
	encoded, err := json.Marshal(struct {
		Catalog model.MappingCatalogDocument            `json:"catalog"`
		Recipes []model.GenerationProductRecipeDocument `json:"recipes"`
	}{Catalog: plan.Catalog, Recipes: plan.Recipes})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

type writeMode string

const (
	writeDraft   writeMode = "draft"
	writePublish writeMode = "publish"
)

type applyReport struct {
	Created   int
	Published int
	Skipped   int
}

// The runtime decoders intentionally ignore control-plane metadata, while all
// fields they do read retain exactly the model document shapes. The digest is
// a CAS witness for the reviewed manifest; it prevents a draft from being
// promoted after somebody has changed it out of band.
type catalogRecord struct {
	ID             string                       `bson:"_id"`
	Status         string                       `bson:"status"`
	SourceVersion  string                       `bson:"source_version"`
	PublishedAt    time.Time                    `bson:"published_at"`
	Models         []model.MappingModelDocument `bson:"models"`
	ManifestSHA256 string                       `bson:"manifest_sha256"`
}

type recipeRecord struct {
	ID                  string                                 `bson:"_id"`
	Status              string                                 `bson:"status"`
	TemplateID          string                                 `bson:"template_id"`
	TemplateVersion     int64                                  `bson:"template_version"`
	Atom                string                                 `bson:"atom"`
	ProductKey          string                                 `bson:"product_key"`
	TemplateKey         string                                 `bson:"template_key,omitempty"`
	Input               map[string]any                         `bson:"input"`
	Assets              []model.GenerationProductAssetDocument `bson:"assets,omitempty"`
	AllowedUserInputs   []string                               `bson:"allowed_user_inputs,omitempty"`
	PromptUserInputMode string                                 `bson:"prompt_user_input_mode,omitempty"`
	ManifestSHA256      string                                 `bson:"manifest_sha256"`
}

func applyManifest(ctx context.Context, database *mongo.Database, plan importPlan, mode writeMode) (applyReport, error) {
	if database == nil || database.Client() == nil {
		return applyReport{}, errors.New("MongoDB database is required")
	}
	if mode != writeDraft && mode != writePublish {
		return applyReport{}, fmt.Errorf("invalid write mode %q", mode)
	}
	if plan.ManifestSHA256 == "" || len(plan.Recipes) == 0 || plan.Catalog.ID == "" {
		return applyReport{}, errors.New("validated import plan is required")
	}
	session, err := database.Client().StartSession()
	if err != nil {
		return applyReport{}, fmt.Errorf("start catalog import transaction: %w", err)
	}
	defer session.EndSession(context.Background())

	result, err := session.WithTransaction(ctx, func(tx context.Context) (any, error) {
		report := applyReport{}
		catalogAlreadyPublished, err := applyCatalog(tx, database, plan, mode, &report)
		if err != nil {
			return nil, err
		}
		if catalogAlreadyPublished {
			if err := verifyPublishedReplay(tx, database, plan, &report); err != nil {
				return nil, err
			}
			return report, nil
		}
		for _, recipe := range plan.Recipes {
			if err := applyRecipe(tx, database, recipe, plan.ManifestSHA256, mode, &report); err != nil {
				return nil, err
			}
		}
		return report, nil
	})
	if err != nil {
		return applyReport{}, err
	}
	report, ok := result.(applyReport)
	if !ok {
		return applyReport{}, errors.New("catalog import transaction returned an unexpected result")
	}
	return report, nil
}

// applyCatalog returns true only when this exact manifest is replaying an
// already published catalog. Callers must then verify every recipe is already
// published too; a published catalog is never a container that later runs can
// append recipes to.
func applyCatalog(ctx context.Context, database *mongo.Database, plan importPlan, mode writeMode, report *applyReport) (bool, error) {
	collection := database.Collection(schema.CollectionGenerationModelMappings)
	var existing catalogRecord
	err := collection.FindOne(ctx, bson.D{{Key: "_id", Value: plan.Catalog.ID}}).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		if mode != writeDraft {
			return false, fmt.Errorf("catalog %q has no imported draft to publish", plan.Catalog.ID)
		}
		if _, err := collection.InsertOne(ctx, catalogRecordFromDocument(plan.Catalog, string(writeDraft), plan.ManifestSHA256)); err != nil {
			return false, fmt.Errorf("insert catalog draft %q: %w", plan.Catalog.ID, err)
		}
		report.Created++
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read catalog %q: %w", plan.Catalog.ID, err)
	}
	if !sameCatalogDocument(existing, plan.Catalog) {
		return false, fmt.Errorf("catalog version %q already exists with different immutable content", plan.Catalog.ID)
	}
	switch existing.Status {
	case "published":
		if existing.ManifestSHA256 != plan.ManifestSHA256 {
			return false, fmt.Errorf("published catalog %q belongs to a different immutable manifest", plan.Catalog.ID)
		}
		report.Skipped++
		return true, nil
	case "draft":
		if mode == writeDraft {
			if existing.ManifestSHA256 != plan.ManifestSHA256 {
				return false, fmt.Errorf("catalog draft %q belongs to a different manifest", plan.Catalog.ID)
			}
			report.Skipped++
			return false, nil
		}
		if existing.ManifestSHA256 != plan.ManifestSHA256 {
			return false, fmt.Errorf("catalog draft %q does not match this manifest", plan.Catalog.ID)
		}
		result, err := collection.UpdateOne(ctx, bson.D{
			{Key: "_id", Value: plan.Catalog.ID}, {Key: "status", Value: "draft"},
			{Key: "manifest_sha256", Value: plan.ManifestSHA256},
		}, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "published"}}}})
		if err != nil {
			return false, fmt.Errorf("publish catalog %q: %w", plan.Catalog.ID, err)
		}
		if result.MatchedCount != 1 {
			return false, fmt.Errorf("catalog draft %q changed before publish", plan.Catalog.ID)
		}
		report.Published++
		return false, nil
	default:
		return false, fmt.Errorf("catalog version %q has unsupported status %q", plan.Catalog.ID, existing.Status)
	}
}

func verifyPublishedReplay(ctx context.Context, database *mongo.Database, plan importPlan, report *applyReport) error {
	collection := database.Collection(schema.CollectionGenerationProductRecipes)
	count, err := collection.CountDocuments(ctx, bson.D{{Key: "manifest_sha256", Value: plan.ManifestSHA256}})
	if err != nil {
		return fmt.Errorf("count published manifest recipes: %w", err)
	}
	if count != int64(len(plan.Recipes)) {
		return fmt.Errorf("published catalog %q does not have exactly this manifest's recipes", plan.Catalog.ID)
	}
	for _, document := range plan.Recipes {
		filter := bson.D{
			{Key: "template_id", Value: document.TemplateID},
			{Key: "template_version", Value: document.TemplateVersion},
			{Key: "atom", Value: document.Atom},
		}
		var existing recipeRecord
		if err := collection.FindOne(ctx, filter).Decode(&existing); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return fmt.Errorf("published catalog %q is missing recipe %s", plan.Catalog.ID, recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
			}
			return fmt.Errorf("read published recipe: %w", err)
		}
		if existing.Status != "published" || existing.ManifestSHA256 != plan.ManifestSHA256 || !sameRecipeDocument(existing, document) {
			return fmt.Errorf("published catalog %q recipe %s is not an exact published replay", plan.Catalog.ID, recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
		}
		report.Skipped++
	}
	return nil
}

func applyRecipe(ctx context.Context, database *mongo.Database, document model.GenerationProductRecipeDocument, manifestSHA256 string, mode writeMode, report *applyReport) error {
	collection := database.Collection(schema.CollectionGenerationProductRecipes)
	filter := bson.D{
		{Key: "template_id", Value: document.TemplateID},
		{Key: "template_version", Value: document.TemplateVersion},
		{Key: "atom", Value: document.Atom},
	}
	var existing recipeRecord
	err := collection.FindOne(ctx, filter).Decode(&existing)
	if errors.Is(err, mongo.ErrNoDocuments) {
		if mode != writeDraft {
			return fmt.Errorf("recipe %s has no imported draft to publish", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
		}
		if _, err := collection.InsertOne(ctx, recipeRecordFromDocument(document, string(writeDraft), manifestSHA256)); err != nil {
			return fmt.Errorf("insert recipe draft: %w", err)
		}
		report.Created++
		return nil
	}
	if err != nil {
		return fmt.Errorf("read recipe: %w", err)
	}
	if !sameRecipeDocument(existing, document) {
		return fmt.Errorf("recipe %s already exists with different immutable content", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
	}
	switch existing.Status {
	case "published":
		if existing.ManifestSHA256 != manifestSHA256 {
			return fmt.Errorf("published recipe %s belongs to a different immutable manifest", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
		}
		if mode == writeDraft {
			return fmt.Errorf("published recipe %s cannot be added to a draft catalog", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
		}
		report.Skipped++
		return nil
	case "draft":
		if mode == writeDraft {
			if existing.ManifestSHA256 != manifestSHA256 {
				return fmt.Errorf("recipe draft %s belongs to a different manifest", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
			}
			report.Skipped++
			return nil
		}
		if existing.ManifestSHA256 != manifestSHA256 {
			return fmt.Errorf("recipe draft %s does not match this manifest", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
		}
		publishFilter := append(filter,
			bson.E{Key: "status", Value: "draft"},
			bson.E{Key: "manifest_sha256", Value: manifestSHA256},
		)
		result, err := collection.UpdateOne(ctx, publishFilter, bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: "published"}}}})
		if err != nil {
			return fmt.Errorf("publish recipe: %w", err)
		}
		if result.MatchedCount != 1 {
			return fmt.Errorf("recipe draft %s changed before publish", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom))
		}
		report.Published++
		return nil
	default:
		return fmt.Errorf("recipe %s has unsupported status %q", recipeTupleKey(document.TemplateID, document.TemplateVersion, document.Atom), existing.Status)
	}
}

func catalogRecordFromDocument(document model.MappingCatalogDocument, status, manifestSHA256 string) catalogRecord {
	return catalogRecord{
		ID: document.ID, Status: status, SourceVersion: document.SourceVersion, PublishedAt: document.PublishedAt,
		Models: document.Models, ManifestSHA256: manifestSHA256,
	}
}

func recipeRecordFromDocument(document model.GenerationProductRecipeDocument, status, manifestSHA256 string) recipeRecord {
	return recipeRecord{
		ID: document.ID, Status: status, TemplateID: document.TemplateID, TemplateVersion: document.TemplateVersion,
		Atom: document.Atom, ProductKey: document.ProductKey, TemplateKey: document.TemplateKey, Input: document.Input,
		Assets: document.Assets, AllowedUserInputs: document.AllowedUserInputs, PromptUserInputMode: document.PromptUserInputMode,
		ManifestSHA256: manifestSHA256,
	}
}

func sameCatalogDocument(record catalogRecord, document model.MappingCatalogDocument) bool {
	return record.ID == document.ID && record.SourceVersion == document.SourceVersion &&
		record.PublishedAt.Equal(document.PublishedAt) && reflect.DeepEqual(record.Models, document.Models)
}

func sameRecipeDocument(record recipeRecord, document model.GenerationProductRecipeDocument) bool {
	return record.ID == document.ID && record.TemplateID == document.TemplateID &&
		record.TemplateVersion == document.TemplateVersion && record.Atom == document.Atom &&
		record.ProductKey == document.ProductKey && record.TemplateKey == document.TemplateKey &&
		reflect.DeepEqual(record.Input, document.Input) && reflect.DeepEqual(record.Assets, document.Assets) &&
		reflect.DeepEqual(record.AllowedUserInputs, document.AllowedUserInputs) && record.PromptUserInputMode == document.PromptUserInputMode
}
