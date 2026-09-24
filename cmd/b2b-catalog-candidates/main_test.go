package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"ai-business-service/internal/data/model"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestBuildCandidatesStableTemplateProducesReviewRequiredRecipesAndModels(t *testing.T) {
	report := buildCandidates([]model.TemplateDocument{stableTemplate(t, "local-video-5-a", 1)}, nil)

	if report.SchemaVersion != "v1" || len(report.Blocked) != 0 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Recipes) != 2 || len(report.ModelCandidates) != 2 {
		t.Fatalf("recipe/model candidates = %#v", report)
	}
	for _, candidate := range report.Recipes {
		if !candidate.ReviewRequired || candidate.SourceFingerprint == "" {
			t.Fatalf("recipe candidate must be review-required and fingerprinted: %#v", candidate)
		}
	}
	for _, candidate := range report.ModelCandidates {
		if !candidate.ReviewRequired || candidate.SourceFingerprint == "" {
			t.Fatalf("model candidate must be review-required and fingerprinted: %#v", candidate)
		}
	}
	if report.Recipes[0].Atom != "image_to_video" || report.Recipes[1].Atom != "text_to_image" {
		t.Fatalf("recipes must use stable atom ordering: %#v", report.Recipes)
	}
	if report.Recipes[0].Input["durationSeconds"] != int32(5) || report.Recipes[1].Input["aspectRatio"] != "9:16" {
		t.Fatalf("recipe inputs = %#v", report.Recipes)
	}
}

func TestBuildCandidatesBlocksUnknownTemplateStructure(t *testing.T) {
	template := stableTemplate(t, "local-video-5-unknown", 1)
	template.Parameters = mustBSON(t, bson.D{
		{Key: "kind", Value: "template_video"},
		{Key: "i2v", Value: bson.D{
			{Key: "model_sku", Value: "ps-auto"}, {Key: "prompt", Value: "safe action"},
			{Key: "negative_prompt", Value: "blur"}, {Key: "parameters", Value: bson.D{{Key: "durationSeconds", Value: int32(5)}}},
			{Key: "workflow", Value: "not reviewed"},
		}},
		{Key: "t2i", Value: bson.D{
			{Key: "model_sku", Value: "ps-image-v1"}, {Key: "prompt", Value: "safe frame"},
			{Key: "negative_prompt", Value: "blur"}, {Key: "parameters", Value: bson.D{{Key: "aspectRatio", Value: "9:16"}}},
		}},
	})

	report := buildCandidates([]model.TemplateDocument{template}, nil)
	if len(report.Recipes) != 0 || len(report.ModelCandidates) != 0 || len(report.Blocked) != 1 {
		t.Fatalf("unknown structure must emit only one block: %#v", report)
	}
	if report.Blocked[0].Code != "unsupported_template_schema" || report.Blocked[0].TemplateID != template.TemplateID {
		t.Fatalf("blocked = %#v", report.Blocked[0])
	}
}

func TestBuildCandidatesBlocksTemplateWithExistingRecipe(t *testing.T) {
	template := stableTemplate(t, "local-video-5-existing", 4)
	report := buildCandidates([]model.TemplateDocument{template}, map[recipeTuple]struct{}{
		{TemplateID: template.TemplateID, TemplateVersion: template.Version, Atom: "text_to_image"}: {},
	})

	if len(report.Recipes) != 0 || len(report.ModelCandidates) != 0 || len(report.Blocked) != 1 {
		t.Fatalf("existing recipe must block the whole source tuple: %#v", report)
	}
	if report.Blocked[0].Code != "existing_recipe" || report.Blocked[0].Atom != "text_to_image" {
		t.Fatalf("blocked = %#v", report.Blocked[0])
	}
}

func TestBuildCandidatesBlockedOutputExcludesSourcePromptURLAndSecret(t *testing.T) {
	template := stableTemplate(t, "local-video-5-private-source", 1)
	template.Parameters = mustBSON(t, bson.D{
		{Key: "kind", Value: "template_video"},
		{Key: "i2v", Value: bson.D{{Key: "prompt", Value: "do not expose prompt"}}},
		{Key: "source_url", Value: "https://example.invalid/private"},
		{Key: "secret", Value: "do-not-expose-secret"},
	})

	report := buildCandidates([]model.TemplateDocument{template}, nil)
	if len(report.Blocked) != 1 || report.Blocked[0].Code != "unsupported_template_schema" {
		t.Fatalf("blocked = %#v", report)
	}
	encoded, err := json.Marshal(report.Blocked[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"do not expose prompt", "https://example.invalid/private", "do-not-expose-secret", "source_url", "secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("blocked output leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestBuildCandidatesStrictlyFiltersSourceTemplates(t *testing.T) {
	valid := stableTemplate(t, "local-video-5-valid", 1)
	for _, mutate := range []func(*model.TemplateDocument){
		func(item *model.TemplateDocument) { item.Enabled = false },
		func(item *model.TemplateDocument) { item.Mode = "image" },
		func(item *model.TemplateDocument) { item.ContentSurface = "nsfw" },
		func(item *model.TemplateDocument) { item.TemplateID = "remote-video-5-valid" },
	} {
		item := valid
		mutate(&item)
		report := buildCandidates([]model.TemplateDocument{item}, nil)
		if len(report.Recipes) != 0 || len(report.ModelCandidates) != 0 || len(report.Blocked) != 0 {
			t.Fatalf("out-of-scope template emitted candidate: %#v", report)
		}
	}
}

func TestBuildCandidatesFingerprintIsCanonicalAndDeterministic(t *testing.T) {
	first := stableTemplate(t, "local-video-5-deterministic", 1)
	second := stableTemplate(t, "local-video-5-deterministic", 1)
	second.Parameters = mustBSON(t, bson.D{
		{Key: "t2i", Value: bson.D{
			{Key: "parameters", Value: bson.D{{Key: "aspectRatio", Value: "9:16"}}},
			{Key: "negative_prompt", Value: "blur"}, {Key: "prompt", Value: "safe frame"}, {Key: "model_sku", Value: "ps-image-v1"},
		}},
		{Key: "i2v", Value: bson.D{
			{Key: "parameters", Value: bson.D{{Key: "durationSeconds", Value: int32(5)}}},
			{Key: "negative_prompt", Value: "blur"}, {Key: "prompt", Value: "safe action"}, {Key: "model_sku", Value: "ps-auto"},
		}},
		{Key: "kind", Value: "template_video"},
	})

	firstReport := buildCandidates([]model.TemplateDocument{first}, nil)
	secondReport := buildCandidates([]model.TemplateDocument{second}, nil)
	if firstReport.Recipes[0].SourceFingerprint != secondReport.Recipes[0].SourceFingerprint ||
		firstReport.ModelCandidates[0].SourceFingerprint != secondReport.ModelCandidates[0].SourceFingerprint ||
		firstReport.SourceFingerprint != secondReport.SourceFingerprint {
		t.Fatalf("fingerprints must be independent of BSON field ordering:\nfirst=%#v\nsecond=%#v", firstReport, secondReport)
	}
	firstJSON, err := json.Marshal(firstReport)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(secondReport)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("report JSON must be canonical across BSON field order:\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
	}
}

func TestBuildCandidatesBlocksDuplicateSourceTuple(t *testing.T) {
	template := stableTemplate(t, "local-video-5-duplicate", 1)
	report := buildCandidates([]model.TemplateDocument{template, template}, nil)
	if len(report.Recipes) != 0 || len(report.ModelCandidates) != 0 || len(report.Blocked) != 1 || report.Blocked[0].Code != "duplicate_source_tuple" {
		t.Fatalf("duplicate source tuple = %#v", report)
	}
}

func stableTemplate(t *testing.T, templateID string, version int64) model.TemplateDocument {
	t.Helper()
	return model.TemplateDocument{
		TemplateID:     templateID,
		Version:        version,
		Enabled:        true,
		Mode:           "template_video",
		ContentSurface: "sfw",
		Parameters: mustBSON(t, bson.D{
			{Key: "kind", Value: "template_video"},
			{Key: "i2v", Value: bson.D{
				{Key: "model_sku", Value: "ps-auto"}, {Key: "prompt", Value: "safe action"},
				{Key: "negative_prompt", Value: "blur"}, {Key: "parameters", Value: bson.D{{Key: "durationSeconds", Value: int32(5)}}},
			}},
			{Key: "t2i", Value: bson.D{
				{Key: "model_sku", Value: "ps-image-v1"}, {Key: "prompt", Value: "safe frame"},
				{Key: "negative_prompt", Value: "blur"}, {Key: "parameters", Value: bson.D{{Key: "aspectRatio", Value: "9:16"}}},
			}},
		}),
	}
}

func mustBSON(t *testing.T, value any) bson.Raw {
	t.Helper()
	raw, err := bson.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
