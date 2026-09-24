// Command b2b-catalog-candidates scans only local video fixtures and emits
// review-required PolarStar B2B catalog candidates. It never writes MongoDB,
// calls PolarStar, or chooses a public product/model mapping.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"ai-business-service/internal/conf"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	kratosconfig "github.com/go-kratos/kratos/v3/config"
	"github.com/go-kratos/kratos/v3/config/env"
	"github.com/go-kratos/kratos/v3/config/file"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	defaultConfigPath     = "/data/conf/config.yaml"
	candidateSchemaV1     = "v1"
	sourceTemplatePrefix  = "local-video-5-"
	candidateScanTimeout  = 30 * time.Second
	atomTextToImage       = "text_to_image"
	atomImageToVideo      = "image_to_video"
	blockUnsupportedShape = "unsupported_template_schema"
)

var aspectRatioPattern = regexp.MustCompile(`^\d+:\d+$`)

type inputFilter struct {
	Enabled          bool   `json:"enabled"`
	Mode             string `json:"mode"`
	ContentSurface   string `json:"content_surface"`
	TemplateIDPrefix string `json:"template_id_prefix"`
}

type candidateReport struct {
	SchemaVersion     string             `json:"schema_version"`
	Input             inputFilter        `json:"input"`
	SourceFingerprint string             `json:"source_fingerprint"`
	Recipes           []recipeCandidate  `json:"recipes"`
	ModelCandidates   []modelCandidate   `json:"model_candidates"`
	Blocked           []blockedCandidate `json:"blocked"`
}

// recipeCandidate deliberately leaves product_key/template_key absent: an old
// execution SKU is evidence to review, never evidence for a public B2B mapping.
type recipeCandidate struct {
	ReviewRequired      bool           `json:"review_required"`
	SourceFingerprint   string         `json:"source_fingerprint"`
	TemplateID          string         `json:"template_id"`
	TemplateVersion     int64          `json:"template_version"`
	Atom                string         `json:"atom"`
	SourceModelSKU      string         `json:"source_model_sku"`
	Input               map[string]any `json:"input"`
	AllowedUserInputs   []string       `json:"allowed_user_inputs"`
	PromptUserInputMode string         `json:"prompt_user_input_mode,omitempty"`
}

type sourceTuple struct {
	TemplateID      string `json:"template_id"`
	TemplateVersion int64  `json:"template_version"`
}

type modelCandidate struct {
	ReviewRequired       bool          `json:"review_required"`
	SourceFingerprint    string        `json:"source_fingerprint"`
	Capability           string        `json:"capability"`
	SourceModelSKU       string        `json:"source_model_sku"`
	SourceTemplates      []sourceTuple `json:"source_templates"`
	ObservedInputFields  []string      `json:"observed_input_fields"`
	ObservedAspectRatios []string      `json:"observed_aspect_ratios,omitempty"`
	ObservedDurations    []int32       `json:"observed_durations,omitempty"`
}

type blockedCandidate struct {
	Code              string `json:"code"`
	SourceFingerprint string `json:"source_fingerprint"`
	TemplateID        string `json:"template_id"`
	TemplateVersion   int64  `json:"template_version"`
	Atom              string `json:"atom,omitempty"`
}

type recipeTuple struct {
	TemplateID      string
	TemplateVersion int64
	Atom            string
}

type parsedTemplate struct {
	TemplateID string
	Version    int64
	I2V        stableTechnicalRecipe
	T2I        stableTechnicalRecipe
}

type stableTechnicalRecipe struct {
	ModelSKU       string
	Prompt         string
	NegativePrompt string
	Parameters     map[string]any
}

func main() {
	configPath := flag.String("conf", defaultConfigPath, "本地 Bootstrap 配置路径")
	flag.Parse()
	if err := run(context.Background(), *configPath, os.Stdout); err != nil {
		// Do not expose bootstrap errors because they can contain Mongo credentials.
		fmt.Fprintln(os.Stderr, "候选扫描失败。")
		os.Exit(1)
	}
}

func run(parent context.Context, configPath string, stdout io.Writer) error {
	if stdout == nil {
		return errors.New("output writer is required")
	}
	bootstrap, closeConfig, err := loadBootstrap(configPath)
	if err != nil {
		return errors.New("bootstrap unavailable")
	}
	defer closeConfig()
	if err := conf.ValidateConfiguredMongo(bootstrap.GetData()); err != nil {
		return errors.New("MongoDB configuration unavailable")
	}

	client, err := mongo.Connect(options.Client().ApplyURI(bootstrap.GetData().GetMongo().GetUri()))
	if err != nil {
		return errors.New("MongoDB unavailable")
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	ctx, cancel := context.WithTimeout(parent, candidateScanTimeout)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		return errors.New("MongoDB unavailable")
	}
	report, err := scanCandidates(ctx, client.Database(bootstrap.GetData().GetMongo().GetDatabase()))
	if err != nil {
		return errors.New("candidate scan unavailable")
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return errors.New("write candidate report")
	}
	return nil
}

// loadBootstrap follows the standard command bootstrap: file plus KRATOS
// environment source, then the shared Mongo profile override before dialing.
func loadBootstrap(path string) (*conf.Bootstrap, func(), error) {
	configuration := kratosconfig.New(kratosconfig.WithSource(file.NewSource(path), env.NewSource("KRATOS")))
	if err := configuration.Load(); err != nil {
		_ = configuration.Close()
		return nil, nil, err
	}
	bootstrap := &conf.Bootstrap{}
	if err := configuration.Scan(bootstrap); err != nil {
		_ = configuration.Close()
		return nil, nil, err
	}
	if err := conf.ApplyMongoEnvironmentOverrides(bootstrap, os.Getenv); err != nil {
		_ = configuration.Close()
		return nil, nil, err
	}
	return bootstrap, func() { _ = configuration.Close() }, nil
}

func scanCandidates(ctx context.Context, database *mongo.Database) (candidateReport, error) {
	if database == nil || database.Client() == nil {
		return candidateReport{}, errors.New("MongoDB database is required")
	}
	templates, err := readSourceTemplates(ctx, database)
	if err != nil {
		return candidateReport{}, err
	}
	existing, err := readExistingRecipes(ctx, database, templates)
	if err != nil {
		return candidateReport{}, err
	}
	return buildCandidates(templates, existing), nil
}

func readSourceTemplates(ctx context.Context, database *mongo.Database) ([]model.TemplateDocument, error) {
	cursor, err := database.Collection(schema.CollectionTemplates).Find(ctx, sourceTemplateMongoFilter(), options.Find().SetSort(bson.D{
		{Key: "template_id", Value: 1}, {Key: "version", Value: 1}, {Key: "_id", Value: 1},
	}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var templates []model.TemplateDocument
	for cursor.Next(ctx) {
		var document model.TemplateDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, err
		}
		templates = append(templates, document)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return templates, nil
}

func readExistingRecipes(ctx context.Context, database *mongo.Database, templates []model.TemplateDocument) (map[recipeTuple]struct{}, error) {
	ids := make([]string, 0, len(templates))
	seen := make(map[string]struct{}, len(templates))
	for _, template := range templates {
		if !matchesSourceFilter(template) {
			continue
		}
		if _, exists := seen[template.TemplateID]; !exists {
			seen[template.TemplateID] = struct{}{}
			ids = append(ids, template.TemplateID)
		}
	}
	if len(ids) == 0 {
		return map[recipeTuple]struct{}{}, nil
	}
	sort.Strings(ids)
	cursor, err := database.Collection(schema.CollectionGenerationProductRecipes).Find(ctx, bson.D{
		{Key: "template_id", Value: bson.D{{Key: "$in", Value: ids}}},
		{Key: "atom", Value: bson.D{{Key: "$in", Value: bson.A{atomTextToImage, atomImageToVideo}}}},
	}, options.Find().SetProjection(bson.D{
		{Key: "template_id", Value: 1}, {Key: "template_version", Value: 1}, {Key: "atom", Value: 1},
	}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	existing := make(map[recipeTuple]struct{})
	for cursor.Next(ctx) {
		var document struct {
			TemplateID      string `bson:"template_id"`
			TemplateVersion int64  `bson:"template_version"`
			Atom            string `bson:"atom"`
		}
		if err := cursor.Decode(&document); err != nil {
			return nil, err
		}
		existing[recipeTuple{TemplateID: document.TemplateID, TemplateVersion: document.TemplateVersion, Atom: document.Atom}] = struct{}{}
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return existing, nil
}

func sourceTemplateMongoFilter() bson.D {
	return bson.D{
		{Key: "enabled", Value: true},
		{Key: "mode", Value: "template_video"},
		{Key: "content_surface", Value: "sfw"},
		{Key: "template_id", Value: bson.Regex{Pattern: "^" + regexp.QuoteMeta(sourceTemplatePrefix)}},
	}
}

func candidateInputFilter() inputFilter {
	return inputFilter{Enabled: true, Mode: "template_video", ContentSurface: "sfw", TemplateIDPrefix: sourceTemplatePrefix}
}

func matchesSourceFilter(template model.TemplateDocument) bool {
	return template.Enabled && template.Mode == "template_video" && template.ContentSurface == "sfw" && strings.HasPrefix(template.TemplateID, sourceTemplatePrefix)
}

func buildCandidates(templates []model.TemplateDocument, existing map[recipeTuple]struct{}) candidateReport {
	report := candidateReport{
		SchemaVersion:   candidateSchemaV1,
		Input:           candidateInputFilter(),
		Recipes:         []recipeCandidate{},
		ModelCandidates: []modelCandidate{},
		Blocked:         []blockedCandidate{},
	}
	if existing == nil {
		existing = map[recipeTuple]struct{}{}
	}

	filtered := make([]model.TemplateDocument, 0, len(templates))
	for _, template := range templates {
		if matchesSourceFilter(template) {
			filtered = append(filtered, template)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].TemplateID != filtered[j].TemplateID {
			return filtered[i].TemplateID < filtered[j].TemplateID
		}
		if filtered[i].Version != filtered[j].Version {
			return filtered[i].Version < filtered[j].Version
		}
		return filtered[i].ID < filtered[j].ID
	})

	counts := make(map[sourceTuple]int, len(filtered))
	for _, template := range filtered {
		counts[sourceTuple{TemplateID: template.TemplateID, TemplateVersion: template.Version}]++
	}
	blockedTuples := make(map[sourceTuple]struct{}, len(filtered))
	for _, template := range filtered {
		tuple := sourceTuple{TemplateID: template.TemplateID, TemplateVersion: template.Version}
		if counts[tuple] > 1 {
			if _, seen := blockedTuples[tuple]; !seen {
				report.Blocked = append(report.Blocked, blockedFor(template, "duplicate_source_tuple", ""))
				blockedTuples[tuple] = struct{}{}
			}
			continue
		}
		if template.TemplateID == "" || template.Version <= 0 {
			report.Blocked = append(report.Blocked, blockedFor(template, "invalid_template_identity", ""))
			continue
		}
		if atom := existingAtom(template, existing); atom != "" {
			report.Blocked = append(report.Blocked, blockedFor(template, "existing_recipe", atom))
			continue
		}
		stable, err := parseStableTemplate(template)
		if err != nil {
			report.Blocked = append(report.Blocked, blockedFor(template, blockUnsupportedShape, ""))
			continue
		}
		report.Recipes = append(report.Recipes, recipeCandidatesFor(stable)...)
	}

	sort.Slice(report.Recipes, func(i, j int) bool {
		left, right := report.Recipes[i], report.Recipes[j]
		if left.TemplateID != right.TemplateID {
			return left.TemplateID < right.TemplateID
		}
		if left.TemplateVersion != right.TemplateVersion {
			return left.TemplateVersion < right.TemplateVersion
		}
		return left.Atom < right.Atom
	})
	report.ModelCandidates = aggregateModelCandidates(report.Recipes)
	sort.Slice(report.Blocked, func(i, j int) bool {
		left, right := report.Blocked[i], report.Blocked[j]
		if left.TemplateID != right.TemplateID {
			return left.TemplateID < right.TemplateID
		}
		if left.TemplateVersion != right.TemplateVersion {
			return left.TemplateVersion < right.TemplateVersion
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Atom < right.Atom
	})
	report.SourceFingerprint = canonicalFingerprint(struct {
		SchemaVersion string             `json:"schema_version"`
		Input         inputFilter        `json:"input"`
		Recipes       []recipeCandidate  `json:"recipes"`
		Models        []modelCandidate   `json:"model_candidates"`
		Blocked       []blockedCandidate `json:"blocked"`
	}{report.SchemaVersion, report.Input, report.Recipes, report.ModelCandidates, report.Blocked})
	return report
}

func existingAtom(template model.TemplateDocument, existing map[recipeTuple]struct{}) string {
	for _, atom := range []string{atomImageToVideo, atomTextToImage} {
		if _, exists := existing[recipeTuple{TemplateID: template.TemplateID, TemplateVersion: template.Version, Atom: atom}]; exists {
			return atom
		}
	}
	return ""
}

func blockedFor(template model.TemplateDocument, code, atom string) blockedCandidate {
	return blockedCandidate{
		Code: code, TemplateID: template.TemplateID, TemplateVersion: template.Version, Atom: atom,
		// A blocked record cannot safely project arbitrary source BSON. Its tuple
		// is canonical, sufficient to identify the exact source record, and leaks
		// neither prompts nor unknown/private parameter values.
		SourceFingerprint: canonicalFingerprint(sourceTuple{TemplateID: template.TemplateID, TemplateVersion: template.Version}),
	}
}

func parseStableTemplate(template model.TemplateDocument) (parsedTemplate, error) {
	var source struct {
		Kind string   `bson:"kind"`
		I2V  bson.Raw `bson:"i2v"`
		T2I  bson.Raw `bson:"t2i"`
	}
	if !decodeExactBSON(template.Parameters, &source, "kind", "i2v", "t2i") || source.Kind != "template_video" {
		return parsedTemplate{}, errors.New("unsupported parameters")
	}
	i2v, err := parseStableTechnical(source.I2V, atomImageToVideo)
	if err != nil {
		return parsedTemplate{}, err
	}
	t2i, err := parseStableTechnical(source.T2I, atomTextToImage)
	if err != nil {
		return parsedTemplate{}, err
	}
	return parsedTemplate{TemplateID: template.TemplateID, Version: template.Version, I2V: i2v, T2I: t2i}, nil
}

func parseStableTechnical(raw bson.Raw, atom string) (stableTechnicalRecipe, error) {
	var source struct {
		ModelSKU       string   `bson:"model_sku"`
		Prompt         string   `bson:"prompt"`
		NegativePrompt string   `bson:"negative_prompt"`
		Parameters     bson.Raw `bson:"parameters"`
	}
	if !decodeExactBSON(raw, &source, "model_sku", "prompt", "negative_prompt", "parameters") ||
		strings.TrimSpace(source.ModelSKU) == "" || strings.TrimSpace(source.Prompt) == "" || strings.TrimSpace(source.NegativePrompt) == "" {
		return stableTechnicalRecipe{}, errors.New("unsupported technical recipe")
	}
	parameters := map[string]any{}
	switch atom {
	case atomImageToVideo:
		var sourceParameters struct {
			DurationSeconds int32 `bson:"durationSeconds"`
		}
		if !decodeExactBSON(source.Parameters, &sourceParameters, "durationSeconds") || sourceParameters.DurationSeconds != 5 {
			return stableTechnicalRecipe{}, errors.New("unsupported image-to-video parameters")
		}
		parameters["durationSeconds"] = sourceParameters.DurationSeconds
	case atomTextToImage:
		var sourceParameters struct {
			AspectRatio string `bson:"aspectRatio"`
		}
		if !decodeExactBSON(source.Parameters, &sourceParameters, "aspectRatio") || !aspectRatioPattern.MatchString(sourceParameters.AspectRatio) {
			return stableTechnicalRecipe{}, errors.New("unsupported text-to-image parameters")
		}
		parameters["aspectRatio"] = sourceParameters.AspectRatio
	default:
		return stableTechnicalRecipe{}, errors.New("unsupported atom")
	}
	return stableTechnicalRecipe{
		ModelSKU: strings.TrimSpace(source.ModelSKU), Prompt: strings.TrimSpace(source.Prompt),
		NegativePrompt: strings.TrimSpace(source.NegativePrompt), Parameters: parameters,
	}, nil
}

// decodeExactBSON rejects unknown, duplicate and missing fields before BSON
// unmarshal can silently ignore or collapse them.
func decodeExactBSON(raw bson.Raw, destination any, fields ...string) bool {
	if len(raw) == 0 {
		return false
	}
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	elements, err := raw.Elements()
	if err != nil || len(elements) != len(fields) {
		return false
	}
	seen := make(map[string]struct{}, len(fields))
	for _, element := range elements {
		key, err := element.KeyErr()
		if err != nil {
			return false
		}
		if _, valid := allowed[key]; !valid {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	return len(seen) == len(fields) && bson.Unmarshal(raw, destination) == nil
}

func recipeCandidatesFor(source parsedTemplate) []recipeCandidate {
	textInput := map[string]any{
		"prompt": source.T2I.Prompt, "negativePrompt": source.T2I.NegativePrompt,
		"aspectRatio": source.T2I.Parameters["aspectRatio"],
	}
	imageInput := map[string]any{
		"prompt": source.I2V.Prompt, "negativePrompt": source.I2V.NegativePrompt,
		"durationSeconds": source.I2V.Parameters["durationSeconds"],
	}
	text := recipeCandidate{
		ReviewRequired: true, TemplateID: source.TemplateID, TemplateVersion: source.Version, Atom: atomTextToImage,
		SourceModelSKU: source.T2I.ModelSKU, Input: textInput, AllowedUserInputs: []string{"prompt"}, PromptUserInputMode: "append",
	}
	text.SourceFingerprint = canonicalFingerprint(textFingerprintSource(text))
	image := recipeCandidate{
		ReviewRequired: true, TemplateID: source.TemplateID, TemplateVersion: source.Version, Atom: atomImageToVideo,
		SourceModelSKU: source.I2V.ModelSKU, Input: imageInput, AllowedUserInputs: []string{"durationSeconds"},
	}
	image.SourceFingerprint = canonicalFingerprint(textFingerprintSource(image))
	return []recipeCandidate{text, image}
}

func textFingerprintSource(candidate recipeCandidate) any {
	return struct {
		TemplateID          string         `json:"template_id"`
		TemplateVersion     int64          `json:"template_version"`
		Atom                string         `json:"atom"`
		SourceModelSKU      string         `json:"source_model_sku"`
		Input               map[string]any `json:"input"`
		AllowedUserInputs   []string       `json:"allowed_user_inputs"`
		PromptUserInputMode string         `json:"prompt_user_input_mode,omitempty"`
	}{candidate.TemplateID, candidate.TemplateVersion, candidate.Atom, candidate.SourceModelSKU, candidate.Input, candidate.AllowedUserInputs, candidate.PromptUserInputMode}
}

func aggregateModelCandidates(recipes []recipeCandidate) []modelCandidate {
	type aggregate struct {
		templates map[sourceTuple]struct{}
		fields    map[string]struct{}
		aspects   map[string]struct{}
		durations map[int32]struct{}
	}
	aggregates := make(map[string]*aggregate)
	for _, recipe := range recipes {
		key := recipe.Atom + "\x00" + recipe.SourceModelSKU
		entry := aggregates[key]
		if entry == nil {
			entry = &aggregate{templates: map[sourceTuple]struct{}{}, fields: map[string]struct{}{}, aspects: map[string]struct{}{}, durations: map[int32]struct{}{}}
			aggregates[key] = entry
		}
		entry.templates[sourceTuple{TemplateID: recipe.TemplateID, TemplateVersion: recipe.TemplateVersion}] = struct{}{}
		for field := range recipe.Input {
			entry.fields[field] = struct{}{}
		}
		if ratio, ok := recipe.Input["aspectRatio"].(string); ok {
			entry.aspects[ratio] = struct{}{}
		}
		if duration, ok := recipe.Input["durationSeconds"].(int32); ok {
			entry.durations[duration] = struct{}{}
		}
	}

	models := make([]modelCandidate, 0, len(aggregates))
	for key, entry := range aggregates {
		capability, sku, _ := strings.Cut(key, "\x00")
		candidate := modelCandidate{
			ReviewRequired: true, Capability: capability, SourceModelSKU: sku,
			SourceTemplates: sortedTuples(entry.templates), ObservedInputFields: sortedStrings(entry.fields),
			ObservedAspectRatios: sortedStrings(entry.aspects), ObservedDurations: sortedDurations(entry.durations),
		}
		candidate.SourceFingerprint = canonicalFingerprint(struct {
			Capability           string        `json:"capability"`
			SourceModelSKU       string        `json:"source_model_sku"`
			SourceTemplates      []sourceTuple `json:"source_templates"`
			ObservedInputFields  []string      `json:"observed_input_fields"`
			ObservedAspectRatios []string      `json:"observed_aspect_ratios,omitempty"`
			ObservedDurations    []int32       `json:"observed_durations,omitempty"`
		}{candidate.Capability, candidate.SourceModelSKU, candidate.SourceTemplates, candidate.ObservedInputFields, candidate.ObservedAspectRatios, candidate.ObservedDurations})
		models = append(models, candidate)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Capability != models[j].Capability {
			return models[i].Capability < models[j].Capability
		}
		return models[i].SourceModelSKU < models[j].SourceModelSKU
	})
	return models
}

func sortedTuples(values map[sourceTuple]struct{}) []sourceTuple {
	result := make([]sourceTuple, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TemplateID != result[j].TemplateID {
			return result[i].TemplateID < result[j].TemplateID
		}
		return result[i].TemplateVersion < result[j].TemplateVersion
	})
	return result
}

func sortedStrings(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sortedDurations(values map[int32]struct{}) []int32 {
	result := make([]int32, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func canonicalFingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}
