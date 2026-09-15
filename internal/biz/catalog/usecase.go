package catalog

import (
	"context"
	"sort"
)

// Usecase 编译跨端一致的用户可见模板清单。
type Usecase struct {
	templates TemplateRepository
}

// NewUsecase 创建模板目录用例。
func NewUsecase(templates TemplateRepository) *Usecase {
	return &Usecase{templates: templates}
}

// BuildManifest 按调用者审核状态过滤并稳定排序模板。
// 平台只参与合法性校验，保证相同审核状态在 Web、iOS、Android 得到同一结果。
func (usecase *Usecase) BuildManifest(ctx context.Context, request ManifestRequest) (*Manifest, error) {
	if !isSupportedPlatform(request.Platform) {
		return nil, ErrInvalidClientPlatform
	}

	templates, err := usecase.templates.ListEnabled(ctx)
	if err != nil {
		return nil, err
	}

	summaries := make([]TemplateSummary, 0, len(templates))
	for _, template := range templates {
		if !isVisibleTemplate(template, request.Reviewed) {
			continue
		}
		summaries = append(summaries, TemplateSummary{
			TemplateID:      template.TemplateID,
			Version:         template.Version,
			ContentSurface:  template.ContentSurface,
			ProductMode:     template.ProductMode,
			SortOrder:       template.SortOrder,
			Title:           template.Title,
			CoverURL:        template.CoverURL,
			VideoURL:        template.VideoURL,
			PreviewVideoURL: template.PreviewVideoURL,
			Tag:             template.Tag,
			Badge:           template.Badge,
		})
	}

	sort.Slice(summaries, func(left, right int) bool {
		if summaries[left].SortOrder != summaries[right].SortOrder {
			return summaries[left].SortOrder < summaries[right].SortOrder
		}
		if summaries[left].TemplateID != summaries[right].TemplateID {
			return summaries[left].TemplateID < summaries[right].TemplateID
		}
		return summaries[left].Version < summaries[right].Version
	})

	return &Manifest{Templates: summaries}, nil
}

func isSupportedPlatform(platform ClientPlatform) bool {
	return platform == ClientPlatformWeb || platform == ClientPlatformIOS || platform == ClientPlatformAndroid
}

func isVisibleTemplate(template Template, reviewed bool) bool {
	if !template.Enabled || !isSupportedContentSurface(template.ContentSurface) || !isSupportedProductMode(template.ProductMode) {
		return false
	}
	return !reviewed || template.ContentSurface == ContentSurfaceSFW
}

func isSupportedContentSurface(contentSurface ContentSurface) bool {
	return contentSurface == ContentSurfaceSFW || contentSurface == ContentSurfaceNSFW
}

func isSupportedProductMode(productMode ProductMode) bool {
	return productMode == ProductModeTemplateImage || productMode == ProductModeTemplateVideo
}
