package generation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"ai-business-service/internal/biz/creations"
	bizgeneration "ai-business-service/internal/biz/generation"
	"ai-business-service/internal/integrations/polarstarb2b"
)

// ErrProviderMappingSnapshot 表示已发布目录无法投影为可用的 B2B 映射快照。
// 调用方必须 fail closed：不得退回本地执行，也不得发出一条映射为空的请求。
var ErrProviderMappingSnapshot = errors.New("generation provider mapping snapshot unavailable")

// MappingSnapshotFromCatalog 把已发布目录投影成 B2B 适配器需要的映射快照。
//
// 目录版本必须与步骤冻结的 mapping_version 完全一致：已发布版本不可原地修改，
// 因此同一步骤在任何时刻重放都会得到同一份快照，不会因目录更新而漂移。
// 版本不一致属于冻结身份不一致，返回 ErrProviderRouteMismatch 而不是静默改用最新版本。
//
// 停用条目保留在目录里用于审计与历史重放，但不参与新任务映射。投影只做形状转换，
// 公开合同合法性（model 命名、字段与 capability 的对应）仍由适配器在映射时判定。
func MappingSnapshotFromCatalog(catalog bizgeneration.PublishedMappingCatalog, mappingVersion string) (polarstarb2b.MappingSnapshot, error) {
	if mappingVersion == "" || catalog.Version != mappingVersion {
		return polarstarb2b.MappingSnapshot{}, ErrProviderRouteMismatch
	}
	normalized, err := catalog.Normalize()
	if err != nil {
		return polarstarb2b.MappingSnapshot{}, ErrProviderMappingSnapshot
	}
	snapshot := polarstarb2b.MappingSnapshot{
		Version: normalized.Version,
		Models:  make(map[string]polarstarb2b.ModelMapping, len(normalized.Entries)),
	}
	for _, entry := range normalized.Entries {
		if !entry.Enabled {
			continue
		}
		if _, exists := snapshot.Models[entry.ProductKey]; exists {
			return polarstarb2b.MappingSnapshot{}, ErrProviderMappingSnapshot
		}
		sizes := make([]polarstarb2b.ImageSize, 0, len(entry.Sizes))
		for _, size := range entry.Sizes {
			sizes = append(sizes, polarstarb2b.ImageSize{Width: size.Width, Height: size.Height})
		}
		templates := make(map[string]string, len(entry.Templates))
		for _, template := range entry.Templates {
			templates[template.Key] = template.ID
		}
		snapshot.Models[entry.ProductKey] = polarstarb2b.ModelMapping{
			Capability:    entry.Capability,
			Model:         entry.PublicModel,
			AllowedInputs: append([]string(nil), entry.AllowedInputs...),
			Sizes:         sizes,
			AspectRatios:  append([]string(nil), entry.AspectRatios...),
			Durations:     append([]int(nil), entry.Durations...),
			Templates:     templates,
		}
	}
	// 没有启用条目说明这个版本对新任务不可用；必须报错而不是继续映射。
	if len(snapshot.Models) == 0 {
		return polarstarb2b.MappingSnapshot{}, ErrProviderMappingSnapshot
	}
	return snapshot, nil
}

// ProductInputFromRecipe is the sole conversion from a frozen domain recipe
// to the PolarStar adapter input. It rejects unknown JSON fields rather than
// letting encoding/json silently discard them; otherwise a malformed published
// recipe could charge a user for a different request than the one reviewed.
func ProductInputFromRecipe(recipe creations.B2BProductRecipe, capability string) (polarstarb2b.ProductInput, error) {
	normalized, err := recipe.Normalize()
	if err != nil {
		return polarstarb2b.ProductInput{}, creations.ErrInvalidAdmission
	}
	decoder := json.NewDecoder(bytes.NewReader(normalized.Input))
	decoder.DisallowUnknownFields()
	var input polarstarb2b.Input
	if err := decoder.Decode(&input); err != nil {
		return polarstarb2b.ProductInput{}, creations.ErrInvalidAdmission
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return polarstarb2b.ProductInput{}, creations.ErrInvalidAdmission
	}
	assets := make([]polarstarb2b.Asset, 0, len(normalized.Assets))
	for _, asset := range normalized.Assets {
		assets = append(assets, polarstarb2b.Asset{Role: asset.Role, URL: asset.URL})
	}
	return polarstarb2b.ProductInput{
		Capability:  capability,
		ModelKey:    normalized.ProductKey,
		TemplateKey: normalized.TemplateKey,
		Input:       input,
		Assets:      assets,
	}, nil
}
