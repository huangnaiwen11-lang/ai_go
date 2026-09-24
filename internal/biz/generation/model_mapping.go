package generation

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	// ErrInvalidMappingCatalog 表示目录版本缺少稳定身份或结构不合法。
	// 目录是「已发布、不可变」的运营产物，结构不完整时必须拒绝整份版本，
	// 不能跳过个别条目后继续使用。
	ErrInvalidMappingCatalog = errors.New("generation: invalid mapping catalog")
	// ErrMappingCatalogUnavailable 表示当前没有可用于新任务的已发布目录版本。
	// 调用方必须 fail closed：不得退回内置默认模型、也不得退回本地执行。
	ErrMappingCatalogUnavailable = errors.New("generation: published mapping catalog unavailable")
)

// MappingCatalogStatus 表示目录版本的发布状态。只有已发布版本可用于新任务。
type MappingCatalogStatus string

const MappingCatalogStatusPublished MappingCatalogStatus = "published"

// ModelMappingSize 是已发布尺寸白名单中的一项。
type ModelMappingSize struct{ Width, Height int }

// ModelMappingTemplate 把 Go 侧模板键映射到公开 templateId。
// 用切片而不是 map，避免模板键里的 `.` 或 `$` 变成 BSON 字段名。
type ModelMappingTemplate struct{ Key, ID string }

// ModelMappingEntry 是已发布目录里一条「capability → 公开模型」的映射。
// ProductKey 是 Go 侧已审核的 SKU 键；公开合同只接受经它映射后的 PublicModel，
// 因此任意前端取值都不会直接出境。
type ModelMappingEntry struct {
	Capability    string
	ProductKey    string
	PublicModel   string
	AllowedInputs []string
	Sizes         []ModelMappingSize
	AspectRatios  []string
	Durations     []int
	Templates     []ModelMappingTemplate
	Enabled       bool
}

// PublishedMappingCatalog 是一个不可变的已发布目录版本。
// 步骤在 admission 时冻结版本号，之后所有映射都必须能用同一版本重放；
// 因此已发布版本不可原地修改，只能发布新版本。
//
// 本类型只做结构校验。公开合同的合法性（如 model 命名规则、字段白名单与
// capability 的对应关系）仍由 B2B 适配器在映射时判定并保持唯一权威。
type PublishedMappingCatalog struct {
	Version       string
	SourceVersion string
	PublishedAt   time.Time
	Entries       []ModelMappingEntry
}

// MappingCatalogStore 是只读的已发布目录边界。
// version 为空时返回当前最新的已发布版本；没有任何已发布版本时返回
// ErrMappingCatalogUnavailable。发布属于控制面，不在本接口内。
type MappingCatalogStore interface {
	PublishedCatalog(ctx context.Context, version string) (*PublishedMappingCatalog, error)
}

// Normalize 校验并标准化一个已发布目录版本。
func (catalog PublishedMappingCatalog) Normalize() (PublishedMappingCatalog, error) {
	if !providerJobIdentifier(catalog.Version) || !providerJobIdentifier(catalog.SourceVersion) ||
		catalog.PublishedAt.IsZero() || len(catalog.Entries) == 0 {
		return PublishedMappingCatalog{}, ErrInvalidMappingCatalog
	}
	entries := make([]ModelMappingEntry, 0, len(catalog.Entries))
	seen := make(map[string]bool, len(catalog.Entries))
	for _, raw := range catalog.Entries {
		entry, err := raw.Normalize()
		if err != nil {
			return PublishedMappingCatalog{}, err
		}
		// 同一产品键在同一个版本里只能出现一次：重复会让映射结果取决于遍历顺序。
		if seen[entry.ProductKey] {
			return PublishedMappingCatalog{}, ErrInvalidMappingCatalog
		}
		seen[entry.ProductKey] = true
		entries = append(entries, entry)
	}
	catalog.Entries = entries
	catalog.PublishedAt = catalog.PublishedAt.UTC()
	return catalog, nil
}

// Normalize 校验并标准化一条映射条目。
func (entry ModelMappingEntry) Normalize() (ModelMappingEntry, error) {
	if !validProviderCapability(entry.Capability) || !providerJobIdentifier(entry.ProductKey) {
		return ModelMappingEntry{}, ErrInvalidMappingCatalog
	}
	if entry.PublicModel == "" || entry.PublicModel != strings.TrimSpace(entry.PublicModel) || len(entry.PublicModel) > 100 {
		return ModelMappingEntry{}, ErrInvalidMappingCatalog
	}
	inputs, err := normalizeUniqueIdentifiers(entry.AllowedInputs)
	if err != nil {
		return ModelMappingEntry{}, err
	}
	ratios, err := normalizeUniqueIdentifiers(entry.AspectRatios)
	if err != nil {
		return ModelMappingEntry{}, err
	}
	sizes := make([]ModelMappingSize, 0, len(entry.Sizes))
	for _, size := range entry.Sizes {
		if size.Width <= 0 || size.Height <= 0 {
			return ModelMappingEntry{}, ErrInvalidMappingCatalog
		}
		sizes = append(sizes, size)
	}
	durations := make([]int, 0, len(entry.Durations))
	for _, duration := range entry.Durations {
		if duration <= 0 {
			return ModelMappingEntry{}, ErrInvalidMappingCatalog
		}
		durations = append(durations, duration)
	}
	templates := make([]ModelMappingTemplate, 0, len(entry.Templates))
	seenTemplates := make(map[string]bool, len(entry.Templates))
	for _, template := range entry.Templates {
		if !providerJobIdentifier(template.Key) || !providerJobIdentifier(template.ID) || seenTemplates[template.Key] {
			return ModelMappingEntry{}, ErrInvalidMappingCatalog
		}
		seenTemplates[template.Key] = true
		templates = append(templates, template)
	}
	entry.AllowedInputs, entry.AspectRatios = inputs, ratios
	entry.Sizes, entry.Durations, entry.Templates = sizes, durations, templates
	return entry, nil
}

// Entry 返回指定产品键的映射条目。调用方仍需自行判断 Enabled：
// 停用条目保留在目录里用于审计与历史重放，但不参与新任务映射。
func (catalog PublishedMappingCatalog) Entry(productKey string) (ModelMappingEntry, bool) {
	for _, entry := range catalog.Entries {
		if entry.ProductKey == productKey {
			return entry, true
		}
	}
	return ModelMappingEntry{}, false
}

// Template 返回指定模板键对应的公开 templateId。
// admission 用它确认模板编译器给出的模板键确实属于该产品键，
// 避免把未知模板键留到映射时才失败。
func (entry ModelMappingEntry) Template(key string) (string, bool) {
	for _, template := range entry.Templates {
		if template.Key == key {
			return template.ID, true
		}
	}
	return "", false
}

func normalizeUniqueIdentifiers(values []string) ([]string, error) {
	normalized := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !providerJobIdentifier(value) || seen[value] {
			return nil, ErrInvalidMappingCatalog
		}
		seen[value] = true
		normalized = append(normalized, value)
	}
	return normalized, nil
}
