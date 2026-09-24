package creations

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// ErrB2BProductRecipeUnavailable 表示指定旧模板版本没有一条已审核、已发布的
// B2B 产品配方，或其输入无法安全编译。调用方必须 fail closed，不能从旧 SKU、
// workflow 或模板名称猜测公开产品。
var ErrB2BProductRecipeUnavailable = &admissionError{message: "creation B2B product recipe unavailable", code: "GENERATION_RECIPE_UNAVAILABLE"}

// PromptUserInputMode is an explicit product rule for the one public field
// whose historical semantics can be either replacement (freeform) or suffix
// composition (template editing). No caller may infer it from a template name.
type PromptUserInputMode string

const (
	PromptUserInputModeReplace PromptUserInputMode = "replace"
	PromptUserInputModeAppend  PromptUserInputMode = "append"
)

// PublishedB2BProductRecipe 是“旧模板版本 + 原子 → 公开 B2B 产品”的不可变发布
// 记录。它和模型目录分层：这里定义产品语义、默认输入和允许的用户覆盖；目录只
// 将已审核 ProductKey 映射成真实公开 model/template ID。
type PublishedB2BProductRecipe struct {
	TemplateID        string
	TemplateVersion   int64
	Atom              StepAtom
	ProductKey        string
	TemplateKey       string
	Input             json.RawMessage
	Assets            []B2BAsset
	AllowedUserInputs []string
	// PromptUserInputMode controls an allowed prompt override. Empty published
	// values normalize to replace for backward-compatible explicit behavior.
	PromptUserInputMode PromptUserInputMode
}

// B2BProductRecipeReader 只读取已发布的不可变配方。它故意不提供更新方法；发布
// 控制面必须先完成离线目录校验，再写一个新模板版本或新发布记录。
type B2BProductRecipeReader interface {
	LoadB2BProductRecipe(context.Context, string, int64, StepAtom) (PublishedB2BProductRecipe, error)
}

// CompileB2BProductRecipe 将模板已审核默认值、受限用户输入和本次已授权素材合成
// 到创建事务将冻结的 B2BProductRecipe。调用者不能加入未发布字段、覆盖固定素材，
// 也不能以相同角色叠加两份素材。
func (source PublishedB2BProductRecipe) CompileB2BProductRecipe(overrides map[string]json.RawMessage, assets []B2BAsset) (B2BProductRecipe, error) {
	normalized, err := source.Normalize()
	if err != nil {
		return B2BProductRecipe{}, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(normalized.Input, &fields) != nil || fields == nil {
		return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	for key, raw := range overrides {
		if !slices.Contains(normalized.AllowedUserInputs, key) || len(raw) == 0 || string(raw) == "null" || !json.Valid(raw) {
			return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
		}
		if key == "prompt" && normalized.PromptUserInputMode == PromptUserInputModeAppend {
			composed, err := appendPublishedPrompt(fields[key], raw)
			if err != nil {
				return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
			}
			fields[key] = composed
			continue
		}
		fields[key] = append(json.RawMessage(nil), raw...)
	}
	input, err := json.Marshal(fields)
	if err != nil {
		return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	combined := make([]B2BAsset, 0, len(normalized.Assets)+len(assets))
	seenRoles := make(map[string]bool, len(normalized.Assets)+len(assets))
	for _, asset := range append(append([]B2BAsset(nil), normalized.Assets...), assets...) {
		if seenRoles[asset.Role] {
			return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
		}
		seenRoles[asset.Role] = true
		combined = append(combined, asset)
	}
	recipe, err := (B2BProductRecipe{
		ProductKey: normalized.ProductKey, TemplateKey: normalized.TemplateKey,
		Input: input, Assets: combined,
	}).Normalize()
	if err != nil {
		return B2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	return recipe, nil
}

// Normalize returns a copied, structurally valid published product recipe.
func (source PublishedB2BProductRecipe) Normalize() (PublishedB2BProductRecipe, error) {
	if source.TemplateID == "" || source.TemplateVersion <= 0 ||
		(source.Atom != AtomTextToImage && source.Atom != AtomImageEdit && source.Atom != AtomImageToVideo) {
		return PublishedB2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	base, err := (B2BProductRecipe{ProductKey: source.ProductKey, TemplateKey: source.TemplateKey, Input: source.Input, Assets: source.Assets}).Normalize()
	if err != nil {
		return PublishedB2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	seen := make(map[string]bool, len(source.AllowedUserInputs))
	allowed := make([]string, 0, len(source.AllowedUserInputs))
	for _, field := range source.AllowedUserInputs {
		if !validAdmissionKey(field) || seen[field] {
			return PublishedB2BProductRecipe{}, ErrB2BProductRecipeUnavailable
		}
		seen[field] = true
		allowed = append(allowed, field)
	}
	mode := source.PromptUserInputMode
	if mode == "" {
		mode = PromptUserInputModeReplace
	}
	if mode != PromptUserInputModeReplace && mode != PromptUserInputModeAppend {
		return PublishedB2BProductRecipe{}, ErrB2BProductRecipeUnavailable
	}
	if mode == PromptUserInputModeAppend {
		var fields map[string]json.RawMessage
		if json.Unmarshal(base.Input, &fields) != nil || !slices.Contains(allowed, "prompt") {
			return PublishedB2BProductRecipe{}, ErrB2BProductRecipeUnavailable
		}
		var prompt string
		if raw, exists := fields["prompt"]; !exists || json.Unmarshal(raw, &prompt) != nil || strings.TrimSpace(prompt) == "" {
			return PublishedB2BProductRecipe{}, ErrB2BProductRecipeUnavailable
		}
	}
	source.Input = append(json.RawMessage(nil), base.Input...)
	source.Assets = append([]B2BAsset(nil), base.Assets...)
	source.AllowedUserInputs = allowed
	source.PromptUserInputMode = mode
	return source, nil
}

func appendPublishedPrompt(baseRaw, userRaw json.RawMessage) (json.RawMessage, error) {
	var base, user string
	if json.Unmarshal(baseRaw, &base) != nil || json.Unmarshal(userRaw, &user) != nil || strings.TrimSpace(base) == "" {
		return nil, ErrB2BProductRecipeUnavailable
	}
	if strings.TrimSpace(user) == "" {
		return json.Marshal(base)
	}
	return json.Marshal(base + "，" + user)
}
