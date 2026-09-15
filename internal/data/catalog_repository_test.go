package data

import (
	"context"
	"reflect"
	"testing"
	"time"

	"ai-business-service/internal/biz/catalog"
	"ai-business-service/internal/data/migrate"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoTemplateRepositoryBuildsSafeAndCrossPlatformConsistentManifest(t *testing.T) {
	client := newLocalMongoClient(t)
	database := client.Database("cling_main")
	ctx, cancel := newMongoTestContext()
	defer cancel()
	if err := migrate.NewInitializer(database).Ensure(ctx); err != nil {
		t.Fatalf("ensure local schema: %v", err)
	}

	createdAt := time.Now().UTC()
	documents := []model.TemplateDocument{
		{
			ID:             uuid.NewString(),
			TemplateID:     "safe-video-" + uuid.NewString(),
			Version:        1,
			ContentSurface: string(catalog.ContentSurfaceSFW),
			Mode:           string(catalog.ProductModeTemplateVideo),
			SortOrder:      20,
			Enabled:        true,
			Parameters:     emptyTemplateParameters,
			CreatedAt:      createdAt,
			UpdatedAt:      createdAt,
		},
		{
			ID:             uuid.NewString(),
			TemplateID:     "adult-image-" + uuid.NewString(),
			Version:        1,
			ContentSurface: string(catalog.ContentSurfaceNSFW),
			Mode:           string(catalog.ProductModeTemplateImage),
			SortOrder:      10,
			Enabled:        true,
			Parameters:     emptyTemplateParameters,
			CreatedAt:      createdAt,
			UpdatedAt:      createdAt,
		},
		{
			ID:             uuid.NewString(),
			TemplateID:     "disabled-safe-" + uuid.NewString(),
			Version:        1,
			ContentSurface: string(catalog.ContentSurfaceSFW),
			Mode:           string(catalog.ProductModeTemplateImage),
			SortOrder:      5,
			Enabled:        false,
			Parameters:     emptyTemplateParameters,
			CreatedAt:      createdAt,
			UpdatedAt:      createdAt,
		},
		{
			ID:             uuid.NewString(),
			TemplateID:     "invalid-mode-" + uuid.NewString(),
			Version:        1,
			ContentSurface: string(catalog.ContentSurfaceSFW),
			Mode:           "image_to_video",
			SortOrder:      15,
			Enabled:        true,
			Parameters:     emptyTemplateParameters,
			CreatedAt:      createdAt,
			UpdatedAt:      createdAt,
		},
	}
	templatesCollection := database.Collection(schema.CollectionTemplates)
	for _, document := range documents {
		if _, err := templatesCollection.InsertOne(ctx, document); err != nil {
			t.Fatalf("insert template %q: %v", document.ID, err)
		}
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := newMongoTestContext()
		defer cleanupCancel()
		for _, document := range documents {
			if _, err := templatesCollection.DeleteOne(cleanupContext, bson.D{{Key: "_id", Value: document.ID}}); err != nil {
				t.Errorf("delete test template %q: %v", document.ID, err)
			}
		}
	})

	repository := NewTemplateRepository(&Data{database: database})
	enabledTemplates, err := repository.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("ListEnabled() error = %v", err)
	}
	testTemplateIDs := map[string]struct{}{
		documents[0].TemplateID: {},
		documents[1].TemplateID: {},
		documents[2].TemplateID: {},
		documents[3].TemplateID: {},
	}
	if got := filterCatalogTemplates(enabledTemplates, testTemplateIDs); len(got) != 3 {
		t.Fatalf("本测试启用模板数量 = %d，want 3", len(got))
	}

	usecase := catalog.NewUsecase(repository)
	var standardManifest *catalog.Manifest
	for _, platform := range []catalog.ClientPlatform{
		catalog.ClientPlatformWeb,
		catalog.ClientPlatformIOS,
		catalog.ClientPlatformAndroid,
	} {
		manifest, err := usecase.BuildManifest(context.Background(), catalog.ManifestRequest{Platform: platform})
		if err != nil {
			t.Fatalf("BuildManifest(%q) error = %v", platform, err)
		}
		if standardManifest == nil {
			standardManifest = manifest
			continue
		}
		if !reflect.DeepEqual(manifest, standardManifest) {
			t.Fatalf("manifest for %q = %#v, want same as %#v", platform, manifest, standardManifest)
		}
	}
	if got := filterTemplateIDs(catalogTemplateIDs(standardManifest), testTemplateIDs); !reflect.DeepEqual(got, []string{documents[1].TemplateID, documents[0].TemplateID}) {
		t.Fatalf("standard manifest ids = %v, want adult then safe template", got)
	}

	reviewedManifest, err := usecase.BuildManifest(context.Background(), catalog.ManifestRequest{
		Platform: catalog.ClientPlatformWeb,
		Reviewed: true,
	})
	if err != nil {
		t.Fatalf("BuildManifest(reviewed) error = %v", err)
	}
	if got := filterTemplateIDs(catalogTemplateIDs(reviewedManifest), testTemplateIDs); !reflect.DeepEqual(got, []string{documents[0].TemplateID}) {
		t.Fatalf("reviewed manifest ids = %v, want only safe template", got)
	}
}

// emptyTemplateParameters 是 BSON 可编码的空文档，不能使用 nil 的 bson.Raw 零值。
var emptyTemplateParameters = bson.Raw{5, 0, 0, 0, 0}

func catalogTemplateIDs(manifest *catalog.Manifest) []string {
	ids := make([]string, 0, len(manifest.Templates))
	for _, template := range manifest.Templates {
		ids = append(ids, template.TemplateID)
	}
	return ids
}

// filterCatalogTemplates 只保留当前测试创建的随机模板，避免本地手工联调数据污染断言。
func filterCatalogTemplates(templates []catalog.Template, templateIDs map[string]struct{}) []catalog.Template {
	result := make([]catalog.Template, 0, len(templates))
	for _, template := range templates {
		if _, exists := templateIDs[template.TemplateID]; exists {
			result = append(result, template)
		}
	}
	return result
}

// filterTemplateIDs 对公开 Manifest 进行同样的测试数据筛选，保留平台一致性断言。
func filterTemplateIDs(templateIDs []string, expected map[string]struct{}) []string {
	result := make([]string, 0, len(templateIDs))
	for _, templateID := range templateIDs {
		if _, exists := expected[templateID]; exists {
			result = append(result, templateID)
		}
	}
	return result
}
