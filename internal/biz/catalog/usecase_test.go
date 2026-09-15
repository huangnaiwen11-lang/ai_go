package catalog_test

import (
	"context"
	"reflect"
	"testing"

	"ai-business-service/internal/biz/catalog"
)

func TestBuildManifestReturnsOnlySFWTemplatesForReviewedUser(t *testing.T) {
	repository := memoryTemplateRepository{templates: []catalog.Template{
		newTemplate("safe-video", catalog.ContentSurfaceSFW, catalog.ProductModeTemplateVideo, 20, true),
		newTemplate("adult-image", catalog.ContentSurfaceNSFW, catalog.ProductModeTemplateImage, 10, true),
		newTemplate("safe-image", catalog.ContentSurfaceSFW, catalog.ProductModeTemplateImage, 10, true),
	}}
	usecase := catalog.NewUsecase(repository)

	manifest, err := usecase.BuildManifest(context.Background(), catalog.ManifestRequest{
		Platform: catalog.ClientPlatformWeb,
		Reviewed: true,
	})
	if err != nil {
		t.Fatalf("BuildManifest() error = %v", err)
	}
	if got, want := templateIDs(manifest), []string{"safe-image", "safe-video"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reviewed manifest template ids = %v, want %v", got, want)
	}
	for _, template := range manifest.Templates {
		if template.ContentSurface != catalog.ContentSurfaceSFW {
			t.Fatalf("reviewed manifest exposes %q template %q", template.ContentSurface, template.TemplateID)
		}
	}
}

func TestBuildManifestIsIdenticalAcrossSupportedPlatforms(t *testing.T) {
	repository := memoryTemplateRepository{templates: []catalog.Template{
		newTemplate("video-template", catalog.ContentSurfaceSFW, catalog.ProductModeTemplateVideo, 30, true),
		newTemplate("image-template", catalog.ContentSurfaceSFW, catalog.ProductModeTemplateImage, 10, true),
		newTemplate("adult-template", catalog.ContentSurfaceNSFW, catalog.ProductModeTemplateImage, 20, true),
	}}
	usecase := catalog.NewUsecase(repository)

	var baseline *catalog.Manifest
	for _, platform := range []catalog.ClientPlatform{
		catalog.ClientPlatformWeb,
		catalog.ClientPlatformIOS,
		catalog.ClientPlatformAndroid,
	} {
		manifest, err := usecase.BuildManifest(context.Background(), catalog.ManifestRequest{Platform: platform})
		if err != nil {
			t.Fatalf("BuildManifest(%q) error = %v", platform, err)
		}
		if baseline == nil {
			baseline = manifest
			continue
		}
		if !reflect.DeepEqual(manifest, baseline) {
			t.Fatalf("manifest for %q = %#v, want same as %#v", platform, manifest, baseline)
		}
	}
}

func TestBuildManifestDoesNotExposeDisabledOrInvalidTemplates(t *testing.T) {
	repository := memoryTemplateRepository{templates: []catalog.Template{
		newTemplate("enabled-safe", catalog.ContentSurfaceSFW, catalog.ProductModeTemplateImage, 10, true),
		newTemplate("disabled-safe", catalog.ContentSurfaceSFW, catalog.ProductModeTemplateImage, 20, false),
		newTemplate("invalid-surface", catalog.ContentSurface("unknown"), catalog.ProductModeTemplateImage, 30, true),
		newTemplate("technical-atom", catalog.ContentSurfaceSFW, catalog.ProductMode("image_to_video"), 40, true),
	}}
	usecase := catalog.NewUsecase(repository)

	manifest, err := usecase.BuildManifest(context.Background(), catalog.ManifestRequest{Platform: catalog.ClientPlatformWeb})
	if err != nil {
		t.Fatalf("BuildManifest() error = %v", err)
	}
	if got, want := templateIDs(manifest), []string{"enabled-safe"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest template ids = %v, want %v", got, want)
	}
}

type memoryTemplateRepository struct {
	templates []catalog.Template
}

func (repository memoryTemplateRepository) ListEnabled(ctx context.Context) ([]catalog.Template, error) {
	return append([]catalog.Template(nil), repository.templates...), nil
}

func newTemplate(templateID string, contentSurface catalog.ContentSurface, productMode catalog.ProductMode, sortOrder int32, enabled bool) catalog.Template {
	return catalog.Template{
		ID:             "document-" + templateID,
		TemplateID:     templateID,
		Version:        1,
		ContentSurface: contentSurface,
		ProductMode:    productMode,
		SortOrder:      sortOrder,
		Enabled:        enabled,
	}
}

func templateIDs(manifest *catalog.Manifest) []string {
	ids := make([]string, 0, len(manifest.Templates))
	for _, template := range manifest.Templates {
		ids = append(ids, template.TemplateID)
	}
	return ids
}
