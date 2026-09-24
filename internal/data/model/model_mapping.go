package model

import "time"

// MappingCatalogDocument 是一个已发布模型目录版本。
// `_id` 就是目录版本号：版本发布后不可原地修改，因此不再需要额外的 version 字段，
// 唯一性也由 `_id` 直接保证。
type MappingCatalogDocument struct {
	ID            string                 `bson:"_id"`
	Status        string                 `bson:"status"`
	SourceVersion string                 `bson:"source_version"`
	PublishedAt   time.Time              `bson:"published_at"`
	Models        []MappingModelDocument `bson:"models"`
}

// MappingModelDocument 是目录里一条 capability 到公开模型的映射。
// 只保存公开合同允许出现的字段，不含任何 Go 侧私有执行参数。
type MappingModelDocument struct {
	Capability    string                    `bson:"capability"`
	ProductKey    string                    `bson:"product_key"`
	PublicModel   string                    `bson:"public_model"`
	AllowedInputs []string                  `bson:"allowed_inputs"`
	Sizes         []MappingSizeDocument     `bson:"sizes"`
	AspectRatios  []string                  `bson:"aspect_ratios"`
	Durations     []int32                   `bson:"durations"`
	Templates     []MappingTemplateDocument `bson:"templates"`
	Enabled       bool                      `bson:"enabled"`
}

// MappingSizeDocument 是已发布尺寸白名单中的一项。
type MappingSizeDocument struct {
	Width  int32 `bson:"width"`
	Height int32 `bson:"height"`
}

// MappingTemplateDocument 把 Go 侧模板键映射到公开 templateId。
// 用数组而不是子文档：模板键可能含 `.` 或 `$`，不能直接当 BSON 字段名。
type MappingTemplateDocument struct {
	Key string `bson:"key"`
	ID  string `bson:"id"`
}
